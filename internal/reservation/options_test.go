package reservation

import "testing"

func TestReservationOptions_Effective(t *testing.T) {
	priority1 := 1
	priority2 := 2
	skip := true
	path := "videos/test.m2ts"
	keepOrig := "untilEncoded"

	profiles := []string{"h265-1080p"}
	base := &ReservationOptions{
		Priority:       &priority1,
		ContentPath:    &path,
		EncodeProfiles: &profiles,
		KeepOriginal:   &keepOrig,
	}
	overrides := &ReservationOptions{
		Skip:     &skip,
		Priority: &priority2,
	}

	eff := overrides.Effective(base)

	if eff.Skip == nil || !*eff.Skip {
		t.Error("skip should be true from overrides")
	}
	if eff.Priority == nil || *eff.Priority != 2 {
		t.Errorf("priority = %v, want 2", eff.Priority)
	}
	if eff.ContentPath == nil || *eff.ContentPath != path {
		t.Error("contentPath should come from base")
	}
	if eff.EncodeProfiles == nil || len(*eff.EncodeProfiles) != 1 || (*eff.EncodeProfiles)[0] != "h265-1080p" {
		t.Error("encodeProfiles should come from base")
	}
	if eff.KeepOriginal == nil || *eff.KeepOriginal != keepOrig {
		t.Error("keepOriginal should come from base")
	}
}

func TestReservationOptions_EffectiveNilBase(t *testing.T) {
	skip := true
	overrides := &ReservationOptions{Skip: &skip}
	eff := overrides.Effective(nil)
	if eff.Skip == nil || !*eff.Skip {
		t.Error("manual reservation: skip should be true from overrides alone")
	}
}

func TestCloneStringSlicePtr_NormalizesNilSlice(t *testing.T) {
	var profiles []string
	got := cloneStringSlicePtr(&profiles)
	if got == nil || *got == nil {
		t.Fatal("non-nil pointer to nil slice should become non-nil empty slice")
	}
}

// docs/recording.md §4.2 が定める式は
// effective.skip = (action = 'skip') OR (意図がなく base.skip)
// であり、action が record なら base.skip の値に関わらず false になる。
// M2-6 の重複排除が base.skip を立てても、ユーザーの record 意図が勝つという主張
// （同 §4.2「dedup skip（重複排除）」）はこの分岐に依存している。
//
// intentAction == nil のケースを両方向で押さえているのが要点: 上書きを常に
// 適用する実装にすると base 由来の skip が効かなくなる（重複排除が機能しなくなる）。
func TestEffectiveOptions_IntentActionOverridesBaseSkip(t *testing.T) {
	record := IntentRecord
	skip := IntentSkip
	baseSkip := []byte(`{"skip":true,"priority":3}`)

	tests := []struct {
		name         string
		base         []byte
		intentAction *string
		wantSkip     bool
	}{
		{"record intent beats base.skip", baseSkip, &record, false},
		{"skip intent keeps skip", baseSkip, &skip, true},
		{"no intent lets base.skip through", baseSkip, nil, true},
		{"record intent without base", nil, &record, false},
		{"skip intent without base", nil, &skip, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eff, err := EffectiveOptions(tt.base, nil, tt.intentAction)
			if err != nil {
				t.Fatalf("EffectiveOptions: %v", err)
			}
			got := eff.Skip != nil && *eff.Skip
			if got != tt.wantSkip {
				t.Errorf("effective skip = %v (Skip=%v), want %v", got, eff.Skip, tt.wantSkip)
			}
		})
	}
}
