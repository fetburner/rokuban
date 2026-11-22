package db_test

import (
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestListMissingDerivedRecordings は、サムネイルとシークタイルの欠落候補から
// ごみ箱の録画を外す判定を SQL クエリで直接検査する。原本 1 件だけの録画を
// ケースごとに変えて、録画 ID で絞った呼び出しでも同じ結果になることを確かめる。
// 述語 r.deleted_at IS NULL を外すと Trashed_NotListed が候補に現れて落ちる。
// 実体無しの原本の除外は internal/store/pgstore の既存テストが検査する。
// 遠隔への転送待ちのコピーは、設定済みの FS にある場合だけ既存の派生物として扱う。
func TestListMissingDerivedRecordings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// trash は録画をごみ箱へ入れる。
		trash         bool
		derivedStore  string
		stagingStores []string
		seedDerived   bool
		// wantThumbnail / wantSeekTiles は、その録画が候補に含まれるべきか。
		wantThumbnail bool
		wantSeekTiles bool
	}{
		{name: "Plain_Listed", trash: false, derivedStore: dbtest.StoreName, wantThumbnail: true, wantSeekTiles: true},
		{name: "Trashed_NotListed", trash: true, derivedStore: dbtest.StoreName, wantThumbnail: false, wantSeekTiles: false},
		{name: "PublishedOnStagingFS_NotListed", derivedStore: "cloud", stagingStores: []string{dbtest.StoreName}, seedDerived: true},
		{name: "PublishedOutsideStagingFS_Listed", derivedStore: "cloud", stagingStores: []string{"other"}, seedDerived: true, wantThumbnail: true, wantSeekTiles: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := testutil.SetupDB(t)
			ctx := t.Context()
			q := sqlcgen.New(pool)
			recordingID := dbtest.Recording(t, pool)
			dbtest.MediaAsset(t, pool, recordingID)
			if tc.seedDerived {
				for _, kind := range []string{"thumbnail", "seek_tiles"} {
					dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
						p.Kind, p.RelPath = kind, "thumbnails/"+kind+".jpg"
					})
				}
			}
			if tc.trash {
				_, err := q.SoftDeleteRecording(ctx, recordingID)
				testutil.RequireNoError(t, err, "soft deleting recording")
			}

			for _, scope := range []struct {
				name        string
				recordingID *int64
			}{
				{name: "unscoped", recordingID: nil},
				{name: "targeted", recordingID: &recordingID},
			} {
				thumbnails, err := q.ListMissingThumbnailRecordings(ctx, sqlcgen.ListMissingThumbnailRecordingsParams{
					RecordingID:   scope.recordingID,
					DerivedStore:  tc.derivedStore,
					StagingStores: tc.stagingStores,
					RowLimit:      10,
				})
				testutil.RequireNoError(t, err, "ListMissingThumbnailRecordings")
				checkListedRecording(t, scope.name+" thumbnail candidates", thumbnails, recordingID, tc.wantThumbnail)

				seekTiles, err := q.ListMissingSeekTilesRecordings(ctx, sqlcgen.ListMissingSeekTilesRecordingsParams{
					RecordingID:   scope.recordingID,
					DerivedStore:  tc.derivedStore,
					StagingStores: tc.stagingStores,
					RowLimit:      10,
				})
				testutil.RequireNoError(t, err, "ListMissingSeekTilesRecordings")
				checkListedRecording(t, scope.name+" seek_tiles candidates", seekTiles, recordingID, tc.wantSeekTiles)
			}
		})
	}
}

// checkListedRecording は候補の一覧に recordingID が含まれるかを want と比べる。
func checkListedRecording(t *testing.T, label string, got []int64, recordingID int64, want bool) {
	t.Helper()
	var wantIDs []int64
	if want {
		wantIDs = []int64{recordingID}
	}
	if !slices.Equal(got, wantIDs) {
		t.Errorf("%s = %v, want %v", label, got, wantIDs)
	}
}
