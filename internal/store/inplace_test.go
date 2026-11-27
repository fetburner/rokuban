package store_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

// inPlaceRejection は PlanInPlaceRegistration が拒否する 1 つの入力と、期待する error を表す。
// 各行は名前の規則に当たる形にし、それより前の規則では落ちない値を使う。
type inPlaceRejection struct {
	name     string
	assets   []store.InPlaceAsset
	observed []*store.InPlaceCopyObservation
	wantIs   []error
	// wantText は error の全文（空なら比べない）、wantHas は含まれるべき断片（空なら比べない）。
	wantText string
	wantHas  string
}

// checkInPlaceRejections は、各入力が期待どおり拒否されることを確かめる。
func checkInPlaceRejections(t *testing.T, cases []inPlaceRejection) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := store.PlanInPlaceRegistration("tokyo", tc.assets, tc.observed)
			if err == nil {
				t.Fatalf("PlanInPlaceRegistration error = nil, want a rejection; plan = %+v", plan)
			}
			for _, want := range tc.wantIs {
				if !errors.Is(err, want) {
					t.Errorf("error %q does not match %v", err, want)
				}
			}
			if tc.wantText != "" && err.Error() != tc.wantText {
				t.Errorf("error text = %q, want %q", err.Error(), tc.wantText)
			}
			if tc.wantHas != "" && !strings.Contains(err.Error(), tc.wantHas) {
				t.Errorf("error text = %q, want it to contain %q", err.Error(), tc.wantHas)
			}
		})
	}
}

// TestPlanInPlaceRegistrationReuses は、既存の生きた行が通る入力と、行が無いアセットの扱いを、期待値のリテラルで固定する。
func TestPlanInPlaceRegistrationReuses(t *testing.T) {
	h264 := "h264"
	h264Again := "h264"
	original := store.InPlaceAsset{Kind: store.AssetKindOriginal, RelPath: "sites/default/a.m2ts"}
	encoded := store.InPlaceAsset{Kind: store.AssetKindEncoded, Profile: &h264, RelPath: "sites/default/a.mp4"}
	row := func(assetID, recordingID int64, kind string, profile *string) *store.InPlaceCopyObservation {
		return &store.InPlaceCopyObservation{AssetID: assetID, RecordingID: recordingID, Kind: kind, Profile: profile, CopyState: "active"}
	}

	tests := []struct {
		name            string
		assets          []store.InPlaceAsset
		observed        []*store.InPlaceCopyObservation
		wantRecordingID int64
		wantReused      []int64
	}{
		{
			name:       "no existing row registers everything new",
			assets:     []store.InPlaceAsset{original},
			observed:   []*store.InPlaceCopyObservation{nil},
			wantReused: []int64{0},
		},
		{
			name:            "active row with the same kind and an equal profile value is reused",
			assets:          []store.InPlaceAsset{{Kind: store.AssetKindEncoded, Profile: &h264Again, RelPath: "sites/default/a.mp4"}},
			observed:        []*store.InPlaceCopyObservation{row(7, 3, store.AssetKindEncoded, &h264)},
			wantRecordingID: 3,
			wantReused:      []int64{7},
		},
		{
			name:            "active row with a nil profile matches a nil profile",
			assets:          []store.InPlaceAsset{original},
			observed:        []*store.InPlaceCopyObservation{row(7, 3, store.AssetKindOriginal, nil)},
			wantRecordingID: 3,
			wantReused:      []int64{7},
		},
		{
			name:            "two assets of one recording are both reused",
			assets:          []store.InPlaceAsset{original, encoded},
			observed:        []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil), row(2, 5, store.AssetKindEncoded, &h264)},
			wantRecordingID: 5,
			wantReused:      []int64{1, 2},
		},
		{
			name:            "an asset without a row is registered while the others are reused",
			assets:          []store.InPlaceAsset{original, encoded},
			observed:        []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil), nil},
			wantRecordingID: 5,
			wantReused:      []int64{1, 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := store.PlanInPlaceRegistration("tokyo", tc.assets, tc.observed)
			if err != nil {
				t.Fatalf("PlanInPlaceRegistration error = %v, want nil", err)
			}
			if plan.RecordingID != tc.wantRecordingID {
				t.Errorf("RecordingID = %d, want %d", plan.RecordingID, tc.wantRecordingID)
			}
			if !reflect.DeepEqual(plan.ReusedAssetIDs, tc.wantReused) {
				t.Errorf("ReusedAssetIDs = %v, want %v", plan.ReusedAssetIDs, tc.wantReused)
			}
		})
	}
}

// TestPlanInPlaceRegistrationRejectsEachRule は、規則 1 から 5 を 1 つずつ当てる入力を、期待する error で固定する。
func TestPlanInPlaceRegistrationRejectsEachRule(t *testing.T) {
	h264 := "h264"
	mobile := "mobile"
	original := store.InPlaceAsset{Kind: store.AssetKindOriginal, RelPath: "sites/default/a.m2ts"}
	encoded := store.InPlaceAsset{Kind: store.AssetKindEncoded, Profile: &h264, RelPath: "sites/default/a.mp4"}
	row := func(assetID, recordingID int64, kind string, profile *string, state string, removed bool) *store.InPlaceCopyObservation {
		return &store.InPlaceCopyObservation{
			AssetID: assetID, RecordingID: recordingID, Kind: kind, Profile: profile,
			CopyState: state, RecordingRemoved: removed,
		}
	}
	checkInPlaceRejections(t, []inPlaceRejection{
		{
			name:     "rule 1: rows of two recordings conflict",
			assets:   []store.InPlaceAsset{original, encoded},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil, "active", false), row(2, 6, store.AssetKindEncoded, &h264, "active", false)},
			wantIs:   []error{store.ErrConflict},
			wantText: "in-place assets belong to different recordings: 5 and 6: conflict",
		},
		{
			name:     "rule 2: a tombstone conflicts and reports the tombstone",
			assets:   []store.InPlaceAsset{original},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil, "deleted", false)},
			wantIs:   []error{store.ErrConflict, store.ErrInPlacePathTombstoned},
			wantText: "in-place asset \"sites/default/a.m2ts\" is tombstoned in store \"tokyo\"; move or rename the file before registering it again: conflict\nin-place path is tombstoned",
		},
		{
			name:     "rule 3: a trashed recording conflicts",
			assets:   []store.InPlaceAsset{original},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil, "active", true)},
			wantIs:   []error{store.ErrConflict},
			wantText: "in-place asset \"sites/default/a.m2ts\" belongs to trashed recording 5: conflict",
		},
		{
			name:     "rule 4: a deleting copy conflicts",
			assets:   []store.InPlaceAsset{original},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil, "deleting", false)},
			wantIs:   []error{store.ErrConflict},
			wantText: "in-place asset \"sites/default/a.m2ts\" is deleting and will be unlinked: conflict",
		},
		{
			name:     "rule 4: a pending copy conflicts",
			assets:   []store.InPlaceAsset{original},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil, "pending", false)},
			wantIs:   []error{store.ErrConflict},
			wantHas:  "is pending and will be unlinked",
		},
		{
			name:     "rule 5: a different kind conflicts",
			assets:   []store.InPlaceAsset{original},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindEncoded, &h264, "active", false)},
			wantIs:   []error{store.ErrConflict},
			wantHas:  "published asset \"sites/default/a.m2ts\" is encoded/",
		},
		{
			name:     "rule 5: a different profile value conflicts",
			assets:   []store.InPlaceAsset{encoded},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindEncoded, &mobile, "active", false)},
			wantIs:   []error{store.ErrConflict},
			wantHas:  "published asset \"sites/default/a.mp4\" is encoded/",
		},
		{
			name:     "rule 5: a nil profile against a set profile conflicts",
			assets:   []store.InPlaceAsset{original},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, &h264, "active", false)},
			wantIs:   []error{store.ErrConflict},
			wantHas:  "published asset \"sites/default/a.m2ts\" is original/",
		},
	})
}

// TestPlanInPlaceRegistrationPrecedence は、規則の順序（前の規則が先に拒否する）を期待する error で固定する。
func TestPlanInPlaceRegistrationPrecedence(t *testing.T) {
	h264 := "h264"
	original := store.InPlaceAsset{Kind: store.AssetKindOriginal, RelPath: "sites/default/a.m2ts"}
	encoded := store.InPlaceAsset{Kind: store.AssetKindEncoded, Profile: &h264, RelPath: "sites/default/a.mp4"}
	row := func(assetID, recordingID int64, kind string, profile *string, state string, removed bool) *store.InPlaceCopyObservation {
		return &store.InPlaceCopyObservation{
			AssetID: assetID, RecordingID: recordingID, Kind: kind, Profile: profile,
			CopyState: state, RecordingRemoved: removed,
		}
	}
	checkInPlaceRejections(t, []inPlaceRejection{
		{
			name:     "rule 1 runs before rule 2 for the second asset",
			assets:   []store.InPlaceAsset{original, encoded},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil, "active", false), row(2, 6, store.AssetKindEncoded, &h264, "deleted", false)},
			wantIs:   []error{store.ErrConflict},
			wantText: "in-place assets belong to different recordings: 5 and 6: conflict",
		},
		{
			name:     "rule 2 runs before rule 3",
			assets:   []store.InPlaceAsset{original},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil, "deleted", true)},
			wantIs:   []error{store.ErrInPlacePathTombstoned},
		},
		{
			name:     "rule 3 runs before rule 4",
			assets:   []store.InPlaceAsset{original},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindOriginal, nil, "deleting", true)},
			wantIs:   []error{store.ErrConflict},
			wantText: "in-place asset \"sites/default/a.m2ts\" belongs to trashed recording 5: conflict",
		},
		{
			name:     "rule 4 runs before rule 5",
			assets:   []store.InPlaceAsset{original},
			observed: []*store.InPlaceCopyObservation{row(1, 5, store.AssetKindEncoded, &h264, "deleting", false)},
			wantIs:   []error{store.ErrConflict},
			wantText: "in-place asset \"sites/default/a.m2ts\" is deleting and will be unlinked: conflict",
		},
	})
}
