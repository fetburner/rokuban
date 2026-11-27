package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/fetburner/rokuban/internal/reservation"
)

func TestIsEmptyOverridesJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "empty object", raw: json.RawMessage(`{}`), want: true},
		{name: "nonempty object", raw: json.RawMessage(`{"skip":true}`), want: false},
		{name: "invalid JSON", raw: json.RawMessage(`{`), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsEmptyOverridesJSON(tc.raw); got != tc.want {
				t.Errorf("IsEmptyOverridesJSON(%q) = %t, want %t", tc.raw, got, tc.want)
			}
		})
	}
}

// TestPlanOverridesPatch は、ロック後の base と既存の上書きに patch を適用する判断の表。
// 期待値はリテラルで書く。InvalidEffectiveOptionsError の取り出しは errors.As で行う。
func TestPlanOverridesPatch(t *testing.T) {
	untilEncoded := "until_encoded"
	h264 := []string{"h264"}
	emptyProfiles := []string{}
	ten := 10

	cases := []struct {
		name        string
		base        json.RawMessage
		existing    json.RawMessage
		patch       reservation.OverridesPatch
		want        string // 書くべき上書きの JSON（wantErr のときは見ない）
		wantEmpty   bool
		wantErr     bool // error を期待する
		wantInvalid bool // *reservation.InvalidEffectiveOptionsError を期待する（wantErr も真）
	}{
		{
			name:  "no base and no existing: patch sets a value",
			patch: reservation.OverridesPatch{Priority: &ten},
			want:  `{"priority":10}`,
		},
		{
			name:     "existing fields survive a patch on another field",
			existing: json.RawMessage(`{"priority":3}`),
			patch:    reservation.OverridesPatch{FilenameTemplate: strPtr("a")},
			want:     `{"priority":3,"filenameTemplate":"a"}`,
		},
		{
			name:      "reset of the last field empties the overrides",
			existing:  json.RawMessage(`{"priority":7}`),
			patch:     reservation.OverridesPatch{Reset: []reservation.OverrideField{reservation.OverridePriority}},
			want:      `{}`,
			wantEmpty: true,
		},
		{
			name:    "reset of an unknown field is an error",
			patch:   reservation.OverridesPatch{Reset: []reservation.OverrideField{"skip"}},
			wantErr: true,
		},
		{
			name:     "invalid existing JSON is an error",
			existing: json.RawMessage(`{`),
			patch:    reservation.OverridesPatch{Priority: &ten},
			wantErr:  true,
		},
		{
			name:    "invalid base JSON is an error",
			base:    json.RawMessage(`{`),
			patch:   reservation.OverridesPatch{Priority: &ten},
			wantErr: true,
		},
		{
			name:        "until_encoded with no encode profiles is invalid (patch alone)",
			patch:       reservation.OverridesPatch{KeepOriginal: &untilEncoded},
			wantErr:     true,
			wantInvalid: true,
		},
		{
			name:        "reset of encode profiles under existing until_encoded is invalid",
			existing:    json.RawMessage(`{"keepOriginal":"until_encoded","encodeProfiles":["h264"]}`),
			patch:       reservation.OverridesPatch{Reset: []reservation.OverrideField{reservation.OverrideEncodeProfiles}},
			wantErr:     true,
			wantInvalid: true,
		},
		{
			name:        "explicit empty encode profiles under existing until_encoded is invalid",
			existing:    json.RawMessage(`{"keepOriginal":"until_encoded","encodeProfiles":["h264"]}`),
			patch:       reservation.OverridesPatch{EncodeProfiles: &emptyProfiles},
			wantErr:     true,
			wantInvalid: true,
		},
		{
			name:  "base encode profiles satisfy an until_encoded patch",
			base:  json.RawMessage(`{"encodeProfiles":["h264"]}`),
			patch: reservation.OverridesPatch{KeepOriginal: &untilEncoded},
			want:  `{"keepOriginal":"until_encoded"}`,
		},
		{
			name:      "reset of encode profiles lets the base profiles take effect",
			base:      json.RawMessage(`{"keepOriginal":"until_encoded","encodeProfiles":["h264"]}`),
			existing:  json.RawMessage(`{"encodeProfiles":[]}`),
			patch:     reservation.OverridesPatch{Reset: []reservation.OverrideField{reservation.OverrideEncodeProfiles}},
			want:      `{}`,
			wantEmpty: true,
		},
		{
			name:        "explicit empty encode profiles over base until_encoded is invalid",
			base:        json.RawMessage(`{"keepOriginal":"until_encoded","encodeProfiles":["h264"]}`),
			patch:       reservation.OverridesPatch{EncodeProfiles: &emptyProfiles},
			wantErr:     true,
			wantInvalid: true,
		},
		{
			name:  "base with encode profiles and a non-empty patch keeps the patch's profiles",
			base:  json.RawMessage(`{"keepOriginal":"until_encoded","encodeProfiles":["h264"]}`),
			patch: reservation.OverridesPatch{EncodeProfiles: &h264},
			want:  `{"encodeProfiles":["h264"]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overrides, empty, err := PlanOverridesPatch(tc.base, tc.existing, tc.patch)
			var invalid *reservation.InvalidEffectiveOptionsError
			gotInvalid := errors.As(err, &invalid)
			if (err != nil) != tc.wantErr {
				t.Fatalf("PlanOverridesPatch() error = %v, want error %t", err, tc.wantErr)
			}
			if gotInvalid != tc.wantInvalid {
				t.Fatalf("PlanOverridesPatch() error = %v, errors.As InvalidEffectiveOptionsError = %t, want %t", err, gotInvalid, tc.wantInvalid)
			}
			if tc.wantErr {
				if overrides != nil || empty {
					t.Errorf("on error PlanOverridesPatch() = (%s, %t), want (nil, false)", overrides, empty)
				}
				return
			}
			if empty != tc.wantEmpty {
				t.Errorf("empty = %t, want %t", empty, tc.wantEmpty)
			}
			if got, want := decodeForCompare(t, overrides), decodeForCompare(t, json.RawMessage(tc.want)); !reflect.DeepEqual(got, want) {
				t.Errorf("overrides = %s, want %s", overrides, tc.want)
			}
		})
	}
}

// decodeForCompare は JSON の整形（空白・キーの並び）に依らず比べられるよう、値に decode する。
func decodeForCompare(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return v
}

func strPtr(s string) *string { return &s }
