package worker

import (
	"context"
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/storagemaint"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestUnknownDesiredProfile_PredicateAsymmetry は、encode の候補と削除側の
// until_encoded_deletable_originals が同じ view unencoded_desired_profiles を引きつつ、
// 現在の設定（known_profiles）で絞るのは encode 側（関数 encodable_profile）だけで
// あることを固定する（docs/storage/retention.md §保持ポリシー）。
//
// フィクスチャは 1 件の録画で共通: keep_original='until_encoded'、desired が
// 現在の設定に無いプロファイル（"gone"）だけ、原本・サムネイル・タイルは active。
//
//   - 方向 A: encode の候補（現在の設定で絞る）には含まれない --- 投入しても
//     EncodeWorker が弾くだけのゴミになるため
//   - 方向 B: 削除エンジンの候補（絞らない）には含まれず、原本は active のまま
//     残る --- 共有 view に設定の絞りを足すと、config の 1 行の編集で原本が
//     削除可能になってしまうため
func TestUnknownDesiredProfile_PredicateAsymmetry(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)

	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "asym325/orig.m2ts", []byte("data"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindThumbnail, nil, "asym325/thumb.jpg", []byte("jpg"))
	// 原本を残す理由を desired の不足 1 つに絞る（タイルが無いと別の理由で残り、
	// 方向 B が空虚に通る）。
	seedSeekTilesAsset(t, pool, mediaDir, recordingID, "asym325/tiles.jpg")
	markRecordingUntilEncoded(t, pool, recordingID, []string{"gone"})

	// 方向 A: 候補クエリ（known_profiles で絞る）に現れない。
	q := sqlcgen.New(pool)
	candidates, err := q.ListMissingEncodeProfiles(context.Background(), sqlcgen.ListMissingEncodeProfilesParams{
		KnownProfiles: []string{"h264"},
		AttemptLimit:  jobs.EncodeAttemptLimit,
		RowLimit:      1000,
	})
	testutil.RequireNoError(t, err, "ListMissingEncodeProfiles")
	if slices.IndexFunc(candidates, func(row sqlcgen.ListMissingEncodeProfilesRow) bool {
		return row.RecordingID == recordingID
	}) >= 0 {
		t.Errorf("ListMissingEncodeProfiles candidates = %v, recording %d (desired profile %q not in current config) must not be a candidate",
			candidates, recordingID, "gone")
	}

	// 方向 B: 削除エンジンを 1 パス回しても、この録画の原本は消えない
	// （until_encoded_deletable_originals は known_profiles で絞らないので、
	// 「desired が揃っている」という判定に永久に到達しない）。
	dw := &storagemaint.DeleteReconcileWorker{DeleteReconcileStores: store.DeleteReconcileStores{MediaDelete: pgstoretest.Media(pool), RecordingPurge: pgstoretest.Recordings(pool)}, Storage: pgstoretest.Media(pool), Breakers: pgstoretest.Breakers(pool), MediaDir: mediaDir, StorageObjects: testStorageObjects(t, "", mediaDir),
		DerivedObjects: testFilesystemObjectStore(t, mediaDir)}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), dw, DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
	if got := singleCopyState(t, pool, originalID); got != "active" {
		t.Errorf("original copy state after delete reconcile = %q, want active "+
			"(a config typo/rename must not make this original deletable; it must stay until the profile is re-encoded or restored)", got)
	}
}
