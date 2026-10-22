package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/metrics"
)

func runThumbnailReconcilePass(t *testing.T, w *ThumbnailReconcileWorker) {
	runThumbnailReconcilePassArgs(t, w, ThumbnailReconcileArgs{})
}

func runThumbnailReconcilePassArgs(t *testing.T, w *ThumbnailReconcileWorker, args ThumbnailReconcileArgs) {
	t.Helper()
	if _, ok := w.Enqueuer.(*recordingEnqueuer); !ok {
		w.Enqueuer = newRecordingEnqueuer()
	}
	if err := runWorkerViaJobQFake(context.Background(), w, args); err != nil {
		t.Fatalf("ThumbnailReconcileWorker.Work: %v", err)
	}
}

// runContinuationChain は first から始め、各パスが投入した継続を投入順に実行する。
// 継続が止まらない（カーソルが進まない）変異でハングせず、maxPasses を超えたら
// アサーションとして落とす。
func runContinuationChain[A jobq.Args](t *testing.T, enqueuer *recordingEnqueuer, first A, maxPasses int, run func(A) error) {
	t.Helper()
	args := first
	for pass, next := 1, 0; ; pass++ {
		if pass > maxPasses {
			t.Fatalf("reconcile chain still continuing after %d passes; cursor is not advancing", maxPasses)
		}
		if err := run(args); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
		continuations := enqueuedArgs[A](enqueuer, args.Kind())
		if next >= len(continuations) {
			return
		}
		args = continuations[next]
		next++
	}
}

func TestThumbnailReconcile_TargetedPassFiltersAllCandidatesAndLeavesFullStateAlone(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	first := insertTestRecordingForSite(t, pool, "default", 981)
	target := insertTestRecordingForSite(t, pool, "default", 982)
	third := insertTestRecordingForSite(t, pool, "default", 983)
	unrelatedMissing := insertTestRecordingForSite(t, pool, "default", 984)
	for i, id := range []int64{first, target, third, unrelatedMissing} {
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("targeted/%d.ts", i), []byte("fake-ts"))
	}
	// The selected recording also exercises the reselection query. Its current seek
	// points inside a detected CM range, while its poster and seek-tile jobs already
	// exist or are missing as separate observations.
	thumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, target,
		db.AssetKindThumbnail, nil, thumbnailRelPath(target), []byte("old-thumbnail"))
	thirdThumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, third,
		db.AssetKindThumbnail, nil, thumbnailRelPath(third), []byte("old-thumbnail"))
	if err := sqlcgen.New(pool).SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: target,
		CmRanges:    "{[0,60000)}",
	}); err != nil {
		t.Fatalf("seeding CM ranges: %v", err)
	}
	if err := sqlcgen.New(pool).SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: third,
		CmRanges:    "{[0,60000)}",
	}); err != nil {
		t.Fatalf("seeding unrelated CM ranges: %v", err)
	}
	if err := sqlcgen.New(pool).UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: thumbnailID,
		SeekMs:       30000,
	}); err != nil {
		t.Fatalf("seeding thumbnail seek: %v", err)
	}
	if err := sqlcgen.New(pool).UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: thirdThumbnailID,
		SeekMs:       30000,
	}); err != nil {
		t.Fatalf("seeding unrelated thumbnail seek: %v", err)
	}

	metrics.ThumbnailReconcileCandidates.Set(19)
	metrics.ThumbnailReconcileLastPass.Set(29)
	t.Cleanup(func() {
		metrics.ThumbnailReconcileCandidates.Set(0)
		metrics.ThumbnailReconcileLastPass.Set(0)
	})
	// RowLimit 1 にすると、first の対象パスでは missing と seek_tiles、target の対象パスでは
	// 再選択と seek_tiles の窓が満杯になる。窓が満杯になる条件でなければ、対象パスが
	// 継続を積まないことは固定にならない。
	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool), RowLimit: 1}

	runThumbnailReconcilePassArgs(t, w, ThumbnailReconcileArgs{RecordingID: first})
	for _, tc := range []struct {
		id        int64
		thumbnail int
		seekTiles int
	}{{first, 1, 1}, {target, 0, 0}, {third, 0, 0}} {
		if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), tc.id); got != tc.thumbnail {
			t.Errorf("thumbnail jobs for recording %d = %d, want %d", tc.id, got, tc.thumbnail)
		}
		if got := countQueuedJobsForRecording(t, w.Enqueuer.(*recordingEnqueuer), "seek_tiles", tc.id); got != tc.seekTiles {
			t.Errorf("seek_tiles jobs for recording %d = %d, want %d", tc.id, got, tc.seekTiles)
		}
	}
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), unrelatedMissing); got != 0 {
		t.Errorf("thumbnail jobs for unrelated missing-poster recording = %d, want 0", got)
	}
	if got := countQueuedJobsForRecording(t, w.Enqueuer.(*recordingEnqueuer), "seek_tiles", unrelatedMissing); got != 0 {
		t.Errorf("seek_tiles jobs for unrelated missing-poster recording = %d, want 0", got)
	}
	runThumbnailReconcilePassArgs(t, w, ThumbnailReconcileArgs{RecordingID: target})
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), target); got != 1 {
		t.Errorf("thumbnail jobs for reselection target = %d, want 1", got)
	}
	if got := countQueuedJobsForRecording(t, w.Enqueuer.(*recordingEnqueuer), "seek_tiles", target); got != 1 {
		t.Errorf("seek_tiles jobs for reselection target = %d, want 1", got)
	}
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), third); got != 0 {
		t.Errorf("thumbnail jobs for unrelated recording = %d, want 0", got)
	}
	if got := len(enqueuedArgs[ThumbnailReconcileArgs](w.Enqueuer.(*recordingEnqueuer), ThumbnailReconcileArgs{}.Kind())); got != 0 {
		t.Errorf("targeted passes enqueued %d continuation jobs, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.ThumbnailReconcileCandidates); got != 19 {
		t.Errorf("candidate gauge = %v, want unchanged 19", got)
	}
	if got := promtestutil.ToFloat64(metrics.ThumbnailReconcileLastPass); got != 29 {
		t.Errorf("last-pass gauge = %v, want unchanged 29", got)
	}
}

func countQueuedJobsForRecording(t *testing.T, enqueuer *recordingEnqueuer, kind string, recordingID int64) int {
	return countEnqueuedForRecording(t, enqueuer, kind, recordingID)
}

func countThumbnailJobs(t *testing.T, enqueuer *recordingEnqueuer, recordingID int64) int {
	return countQueuedJobsForRecording(t, enqueuer, "thumbnail", recordingID)
}

// ヒント投入が無く、edge record の削除で record_sweep からも再投入できない
// 状態でも、定期パスが thumbnail ジョブを作り、通常の worker が thumbnail を
// コミットできることを端から端まで固定する。
func TestThumbnailReconcile_ReenqueuesAfterLostHintAndDeletedEdgeRecord(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, recordingID, "lost-hint/original.m2ts", []byte("fake-ts"))
	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool), Enqueuer: newRecordingEnqueuer()}

	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 0 {
		t.Fatalf("thumbnail jobs before the periodic pass = %d, want 0", got)
	}

	runThumbnailReconcilePass(t, w)
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 1 {
		t.Fatalf("thumbnail jobs after the periodic pass = %d, want 1", got)
	}

	thumbnailWorker := &ThumbnailWorker{
		Publisher:  newTestMediaStore(pool),
		Media:      newTestMediaStore(pool),
		MediaDir:   mediaDir,
		ScratchDir: scratchDir,
		runCmd:     fakeThumbnailTools(t, 60),
	}
	job := ThumbnailJobArgs{RecordingID: recordingID}
	if err := runWorkerViaJobQFake(context.Background(), thumbnailWorker, job); err != nil {
		t.Fatalf("ThumbnailWorker.Work: %v", err)
	}
	if _, err := sqlcgen.New(pool).GetActiveThumbnailMediaAssetID(context.Background(), recordingID); err != nil {
		t.Fatalf("thumbnail was not born after periodic recovery: %v", err)
	}
}

func TestThumbnailReconcile_EnqueuesOnlyWhenRecordedThumbnailIsInCM(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	inCM := insertTestRecordingForSite(t, pool, "default", 941)
	inKeep := insertTestRecordingForSite(t, pool, "default", 942)
	q := sqlcgen.New(pool)
	for _, recordingID := range []int64{inCM, inKeep} {
		seedOriginalAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("reselect/%d.ts", recordingID), []byte("fake-ts"))
		thumbID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
			db.AssetKindThumbnail, nil, thumbnailRelPath(recordingID), []byte("old-thumbnail"))
		if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
			RecordingID: recordingID,
			CmRanges:    "{[0,60000)}",
		}); err != nil {
			t.Fatalf("seeding CM ranges for %d: %v", recordingID, err)
		}
		seek := int64(30000)
		if recordingID == inKeep {
			seek = 60000
		}
		if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
			MediaAssetID: thumbID,
			SeekMs:       seek,
		}); err != nil {
			t.Fatalf("seeding thumbnail seek for %d: %v", recordingID, err)
		}
	}

	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool), Enqueuer: newRecordingEnqueuer()}
	runThumbnailReconcilePass(t, w)
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), inCM); got != 1 {
		t.Errorf("thumbnail jobs for CM frame = %d, want 1", got)
	}
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), inKeep); got != 0 {
		t.Errorf("thumbnail jobs for keep frame = %d, want 0", got)
	}
}

// 再選択候補の窓も missing thumbnail の窓と独立して進める。1・2 は
// thumbnail 欠落、3・4・5 は既存 thumbnail と chapter 候補に分け、2 パスで
// reselect 側の末尾 5 まで判定されることを確認する。
func TestThumbnailReconcile_ReselectCursorIsIndependent(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	ids := make([]int64, 0, 5)
	for eventID := int32(1); eventID <= 5; eventID++ {
		ids = append(ids, insertTestRecordingWithEventID(t, pool, eventID))
	}
	q := sqlcgen.New(pool)
	for i, recordingID := range ids {
		seedOriginalAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("reselect-cursor/%d.ts", i+1), []byte("fake-ts"))
		if i < 2 {
			continue
		}
		thumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
			db.AssetKindThumbnail, nil, thumbnailRelPath(recordingID), []byte("old-thumbnail"))
		if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
			RecordingID: recordingID,
			CmRanges:    "{[0,60000)}",
		}); err != nil {
			t.Fatalf("seeding CM ranges for %d: %v", recordingID, err)
		}
		seek := int64(60000) // 3・4 は keep 内なので再選択しない。
		if i == 4 {
			seek = 30000 // 5 は CM 内なので再選択ジョブが必要。
		}
		if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
			MediaAssetID: thumbnailID,
			SeekMs:       seek,
		}); err != nil {
			t.Fatalf("seeding thumbnail seek for %d: %v", recordingID, err)
		}
	}

	enqueuer := newRecordingEnqueuer()
	firstWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 2}
	runThumbnailReconcilePass(t, firstWorker)
	continuations := enqueuedArgs[ThumbnailReconcileArgs](enqueuer, ThumbnailReconcileArgs{}.Kind())
	if len(continuations) != 1 || continuations[0].AfterReselectRecordingID != ids[3] {
		t.Fatalf("first reselection continuation = %+v, want cursor after recording %d", continuations, ids[3])
	}
	secondWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 2}
	runThumbnailReconcilePassArgs(t, secondWorker, continuations[0])

	if got := countThumbnailJobs(t, enqueuer, ids[4]); got != 1 {
		t.Errorf("reselection jobs for recording %d = %d, want 1 after the second window", ids[4], got)
	}
}

func TestThumbnailReconcile_DoesNotRepeatUnactionableReselections(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)
	allCutID := insertTestRecordingWithEventID(t, pool, 61)
	seedOriginalAsset(t, pool, mediaDir, allCutID, "reselect-noop/all-cut.ts", []byte("fake-ts"))
	allCutThumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, allCutID,
		db.AssetKindThumbnail, nil, thumbnailRelPath(allCutID), []byte("old-thumbnail"))
	cutOnlyID := insertTestRecordingWithEventID(t, pool, 62)
	cutProfile := "cut-only"
	cutAssetID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, cutOnlyID,
		db.AssetKindEncoded, &cutProfile, "reselect-noop/cut-only.mp4", []byte("fake-mp4"))
	cutThumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, cutOnlyID,
		db.AssetKindThumbnail, nil, thumbnailRelPath(cutOnlyID), []byte("old-thumbnail"))
	if _, err := pool.Exec(ctx,
		`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, $2::int8multirange)`,
		cutAssetID, "{[0,300000)}"); err != nil {
		t.Fatalf("seeding cut-only frozen keep ranges: %v", err)
	}
	for _, recordingID := range []int64{allCutID, cutOnlyID} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID); err != nil {
			t.Fatalf("adopting chapters for %d: %v", recordingID, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(0, 1800000), 'CM', true)`,
		allCutID); err != nil {
		t.Fatalf("seeding all-cut span: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO recording_chapter_spans (recording_id, span, label, cut) VALUES ($1, int8range(25000, 35000), 'CM', true)`,
		cutOnlyID); err != nil {
		t.Fatalf("seeding edited cut-only span: %v", err)
	}
	if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: allCutThumbnailID,
		SeekMs:       30000,
	}); err != nil {
		t.Fatalf("seeding all-cut thumbnail seek: %v", err)
	}
	if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: cutThumbnailID,
		SeekMs:       30000,
	}); err != nil {
		t.Fatalf("seeding cut-only thumbnail seek: %v", err)
	}

	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool)}
	runThumbnailReconcilePass(t, w)
	runThumbnailReconcilePass(t, w)
	for _, recordingID := range []int64{allCutID, cutOnlyID} {
		if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 0 {
			t.Errorf("thumbnail jobs for no-op reselection recording %d = %d, want 0 after two passes", recordingID, got)
		}
	}
}

// ファイルが無いことを delete_reconcile が確認した原本は定期パスから除外する。
// マーカーが消えた後は同じ録画を回収対象へ戻す。
func TestThumbnailReconcile_SkipsKnownMissingOriginalUntilRestored(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "missing/original.m2ts", []byte("fake-ts"))
	if err := os.Remove(filepath.Join(mediaDir, "missing", "original.m2ts")); err != nil {
		t.Fatalf("removing original fixture: %v", err)
	}
	if err := q.UpsertMissingMediaAsset(ctx, originalID); err != nil {
		t.Fatalf("marking original missing: %v", err)
	}

	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool), Enqueuer: newRecordingEnqueuer()}
	runThumbnailReconcilePass(t, w)
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 0 {
		t.Fatalf("thumbnail jobs for known-missing original = %d, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.ThumbnailReconcileCandidates); got != 0 {
		t.Fatalf("thumbnail reconcile candidates for known-missing original = %v, want 0", got)
	}

	if err := q.DeleteMissingMediaAsset(ctx, originalID); err != nil {
		t.Fatalf("clearing restored original marker: %v", err)
	}
	runThumbnailReconcilePass(t, w)
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 1 {
		t.Fatalf("thumbnail jobs after clearing restored marker = %d, want 1", got)
	}
}

// 対象を絞った reconcile も known-missing original を飛び越えず、マーカーが消えた後は拾う。
func TestThumbnailReconcile_TargetedPassSkipsKnownMissingOriginalUntilRestored(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	q := sqlcgen.New(pool)
	recordingID := insertTestRecording(t, pool)
	originalID := seedOriginalAsset(t, pool, t.TempDir(), recordingID, "targeted-recovery/original.m2ts", []byte("fake-ts"))
	if err := q.UpsertMissingMediaAsset(ctx, originalID); err != nil {
		t.Fatalf("marking original missing: %v", err)
	}

	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool)}
	runThumbnailReconcilePassArgs(t, w, ThumbnailReconcileArgs{RecordingID: recordingID})
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 0 {
		t.Fatalf("thumbnail jobs for targeted known-missing original = %d, want 0", got)
	}
	if got := countQueuedJobsForRecording(t, w.Enqueuer.(*recordingEnqueuer), "seek_tiles", recordingID); got != 0 {
		t.Fatalf("seek_tiles jobs for targeted known-missing original = %d, want 0", got)
	}

	if err := q.DeleteMissingMediaAsset(ctx, originalID); err != nil {
		t.Fatalf("clearing restored original marker: %v", err)
	}
	runThumbnailReconcilePassArgs(t, w, ThumbnailReconcileArgs{RecordingID: recordingID})
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 1 {
		t.Fatalf("thumbnail jobs after clearing restored marker = %d, want 1", got)
	}
	if got := countQueuedJobsForRecording(t, w.Enqueuer.(*recordingEnqueuer), "seek_tiles", recordingID); got != 1 {
		t.Fatalf("seek_tiles jobs after clearing restored marker = %d, want 1", got)
	}
}

// ごみ箱の録画は original が残っていても定期パスから除外する。
func TestThumbnailReconcile_ExcludesTrash(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	q := sqlcgen.New(pool)
	recordingID := insertTestRecording(t, pool)
	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     "trash/original.m2ts",
		SizeBytes:   1,
	}); err != nil {
		t.Fatalf("seeding original: %v", err)
	}
	if _, err := q.SoftDeleteRecording(ctx, recordingID); err != nil {
		t.Fatalf("soft deleting recording: %v", err)
	}

	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool), Enqueuer: newRecordingEnqueuer()}
	runThumbnailReconcilePass(t, w)
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 0 {
		t.Errorf("thumbnail jobs for trashed recording = %d, want 0", got)
	}
}

// RowLimit は録画単位の上限であり、先頭候補が解消しなくても窓を回して
// 後続候補へ到達する。
func TestThumbnailReconcile_WindowRotatesPastStuckCandidates(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	firstID := insertTestRecordingForSite(t, pool, "default", 801)
	secondID := insertTestRecordingForSite(t, pool, "default", 802)
	seedOriginalAsset(t, pool, mediaDir, firstID, "window/first.m2ts", []byte("first"))
	seedOriginalAsset(t, pool, mediaDir, secondID, "window/second.m2ts", []byte("second"))

	enqueuer := newRecordingEnqueuer()
	firstWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 1}
	runThumbnailReconcilePass(t, firstWorker)
	if got := countThumbnailJobs(t, enqueuer, firstID); got != 1 {
		t.Fatalf("first recording jobs after first pass = %d, want 1", got)
	}
	if got := countThumbnailJobs(t, enqueuer, secondID); got != 0 {
		t.Fatalf("second recording jobs after first pass = %d, want 0", got)
	}
	continuations := enqueuedArgs[ThumbnailReconcileArgs](enqueuer, ThumbnailReconcileArgs{}.Kind())
	if len(continuations) != 1 || continuations[0].AfterRecordingID != firstID {
		t.Fatalf("continuation = %+v, want cursor after recording %d", continuations, firstID)
	}

	secondWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 1}
	runThumbnailReconcilePassArgs(t, secondWorker, continuations[0])
	if got := countThumbnailJobs(t, enqueuer, secondID); got != 1 {
		t.Fatalf("second recording jobs after rotated pass = %d, want 1", got)
	}
}

// TestThumbnailReconcile_ContinuationSurvivesFreshWorker mirrors --once: each
// continuation starts a new worker, and all three candidate windows advance from
// the positions produced by the previous pass.
func TestThumbnailReconcile_ContinuationSurvivesFreshWorker(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	ids := make([]int64, 0, 6)
	for eventID := int32(14101); eventID <= 14106; eventID++ {
		ids = append(ids, insertTestRecordingWithEventID(t, pool, eventID))
	}
	q := sqlcgen.New(pool)
	for i, recordingID := range ids {
		seedOriginalAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("continuation/%d.ts", i+1), []byte("fake-ts"))
		switch i {
		case 0, 1:
			// These are missing-thumbnail candidates, but not seek_tiles candidates.
			seedSeekTilesAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("continuation/%d-tiles.jpg", i+1))
		case 2, 3:
			// These are reselection candidates. Their seek positions are inside the CM range.
			thumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
				db.AssetKindThumbnail, nil, thumbnailRelPath(recordingID), []byte("old-thumbnail"))
			seedSeekTilesAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("continuation/%d-tiles.jpg", i+1))
			if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
				RecordingID: recordingID,
				CmRanges:    "{[0,60000)}",
			}); err != nil {
				t.Fatalf("seeding CM ranges for %d: %v", recordingID, err)
			}
			if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
				MediaAssetID: thumbnailID,
				SeekMs:       30000,
			}); err != nil {
				t.Fatalf("seeding thumbnail seek for %d: %v", recordingID, err)
			}
		case 4, 5:
			// These need seek_tiles only; an existing thumbnail keeps them out of the
			// missing-poster window and the absent chapter/CM state keeps them out of reselection.
			seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
				db.AssetKindThumbnail, nil, thumbnailRelPath(recordingID), []byte("thumbnail"))
		}
	}

	enqueuer := newRecordingEnqueuer()
	firstWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 1}
	runThumbnailReconcilePass(t, firstWorker)
	for _, tc := range []struct {
		kind string
		id   int64
	}{
		{kind: "thumbnail", id: ids[0]},
		{kind: "thumbnail", id: ids[2]},
		{kind: "seek_tiles", id: ids[4]},
	} {
		if got := countQueuedJobsForRecording(t, enqueuer, tc.kind, tc.id); got != 1 {
			t.Errorf("%s jobs for first window candidate %d = %d, want 1", tc.kind, tc.id, got)
		}
	}
	for _, tc := range []struct {
		kind string
		id   int64
	}{
		{kind: "thumbnail", id: ids[1]},
		{kind: "thumbnail", id: ids[3]},
		{kind: "seek_tiles", id: ids[5]},
	} {
		if got := countQueuedJobsForRecording(t, enqueuer, tc.kind, tc.id); got != 0 {
			t.Errorf("%s jobs for later window candidate %d after first page = %d, want 0", tc.kind, tc.id, got)
		}
	}

	continuations := enqueuedArgs[ThumbnailReconcileArgs](enqueuer, ThumbnailReconcileArgs{}.Kind())
	if len(continuations) != 1 || continuations[0].AfterRecordingID != ids[0] ||
		continuations[0].AfterReselectRecordingID != ids[2] ||
		continuations[0].AfterSeekTilesRecordingID != ids[4] {
		t.Fatalf("first continuation = %+v, want independent cursors after %d, %d, and %d",
			continuations, ids[0], ids[2], ids[4])
	}
	secondWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 1}
	runThumbnailReconcilePassArgs(t, secondWorker, continuations[0])
	for _, tc := range []struct {
		kind string
		id   int64
	}{
		{kind: "thumbnail", id: ids[1]},
		{kind: "thumbnail", id: ids[3]},
		{kind: "seek_tiles", id: ids[5]},
	} {
		if got := countQueuedJobsForRecording(t, enqueuer, tc.kind, tc.id); got != 1 {
			t.Errorf("%s jobs for later window candidate %d after fresh worker = %d, want 1", tc.kind, tc.id, got)
		}
	}
	continuations = enqueuedArgs[ThumbnailReconcileArgs](enqueuer, ThumbnailReconcileArgs{}.Kind())
	if len(continuations) != 2 || continuations[1].AfterRecordingID != ids[1] ||
		continuations[1].AfterReselectRecordingID != ids[3] ||
		continuations[1].AfterSeekTilesRecordingID != ids[5] {
		t.Fatalf("second continuation = %+v, want independent cursors after %d, %d, and %d",
			continuations, ids[1], ids[3], ids[5])
	}
	thirdWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 1}
	runThumbnailReconcilePassArgs(t, thirdWorker, continuations[1])
	if got := len(enqueuedArgs[ThumbnailReconcileArgs](enqueuer, ThumbnailReconcileArgs{}.Kind())); got != 2 {
		t.Errorf("continuation jobs after all windows reached the end = %d, want 2", got)
	}
}

func TestThumbnailReconcile_ExhaustedWindowNotRestarted(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	ids := make([]int64, 0, 5)
	for eventID := int32(15121); eventID <= 15125; eventID++ {
		ids = append(ids, insertTestRecordingWithEventID(t, pool, eventID))
	}
	q := sqlcgen.New(pool)
	for index, recordingID := range ids {
		seedOriginalAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("unequal-windows/%d.ts", index+1), []byte("fake-ts"))
		if index < 3 {
			continue
		}
		thumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
			db.AssetKindThumbnail, nil, thumbnailRelPath(recordingID), []byte("old-thumbnail"))
		seedSeekTilesAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("unequal-windows/%d-tiles.jpg", index+1))
		if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
			RecordingID: recordingID,
			CmRanges:    "{[0,60000)}",
		}); err != nil {
			t.Fatalf("seeding CM ranges for %d: %v", recordingID, err)
		}
		if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
			MediaAssetID: thumbnailID,
			SeekMs:       30000,
		}); err != nil {
			t.Fatalf("seeding thumbnail seek for %d: %v", recordingID, err)
		}
	}
	for _, index := range []int{1, 2} {
		seedSeekTilesAsset(t, pool, mediaDir, ids[index], fmt.Sprintf("unequal-windows/%d-tiles.jpg", index+1))
	}

	enqueuer := newRecordingEnqueuer()
	runContinuationChain(t, enqueuer, ThumbnailReconcileArgs{}, 10, func(args ThumbnailReconcileArgs) error {
		worker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 1}
		return runWorkerViaJobQFake(ctx, worker, args)
	})

	for _, recordingID := range ids[:3] {
		if got := countThumbnailJobs(t, enqueuer, recordingID); got != 1 {
			t.Errorf("missing-thumbnail jobs for %d = %d, want 1", recordingID, got)
		}
	}
	for _, recordingID := range ids[3:] {
		if got := countThumbnailJobs(t, enqueuer, recordingID); got != 1 {
			t.Errorf("reselection jobs for %d = %d, want 1", recordingID, got)
		}
	}
	if got := countSeekTilesJobs(t, enqueuer, ids[0]); got != 1 {
		t.Errorf("seek_tiles jobs for %d = %d, want 1", ids[0], got)
	}
	continuations := enqueuedArgs[ThumbnailReconcileArgs](enqueuer, ThumbnailReconcileArgs{}.Kind())
	if len(continuations) != 3 {
		t.Fatalf("continuation count = %d, want 3 for 3/2/1 candidate windows", len(continuations))
	}
	if continuations[0].MissingDone || continuations[0].ReselectDone || continuations[0].SeekTilesDone {
		t.Errorf("first continuation done flags = missing:%v reselect:%v seek_tiles:%v, want false:false:false",
			continuations[0].MissingDone, continuations[0].ReselectDone, continuations[0].SeekTilesDone)
	}
	if continuations[1].MissingDone || continuations[1].ReselectDone || !continuations[1].SeekTilesDone {
		t.Errorf("second continuation done flags = missing:%v reselect:%v seek_tiles:%v, want false:false:true",
			continuations[1].MissingDone, continuations[1].ReselectDone, continuations[1].SeekTilesDone)
	}
	if continuations[2].MissingDone || !continuations[2].ReselectDone || !continuations[2].SeekTilesDone {
		t.Errorf("third continuation done flags = missing:%v reselect:%v seek_tiles:%v, want false:true:true",
			continuations[2].MissingDone, continuations[2].ReselectDone, continuations[2].SeekTilesDone)
	}
}

// TestThumbnailReconcile_ShortWindowsStayDoneWhileLongerWindowContinues は、窓の長さが
// missing 1 / 再選択 2 / seek_tiles 3 のとき、先に末尾へ達した窓が後続ページで先頭から
// 読み直されない（先頭の候補を投入し直さない）ことを固定する。
func TestThumbnailReconcile_ShortWindowsStayDoneWhileLongerWindowContinues(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	ids := make([]int64, 0, 6)
	for eventID := int32(15131); eventID <= 15136; eventID++ {
		ids = append(ids, insertTestRecordingWithEventID(t, pool, eventID))
	}
	q := sqlcgen.New(pool)
	for index, recordingID := range ids {
		seedOriginalAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("short-first/%d.ts", index+1), []byte("fake-ts"))
		switch {
		case index == 0:
			// missing poster のみ（seek_tiles は揃っている）。
			seedSeekTilesAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("short-first/%d-tiles.jpg", index+1))
		case index <= 2:
			// 再選択のみ。
			thumbnailID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
				db.AssetKindThumbnail, nil, thumbnailRelPath(recordingID), []byte("old-thumbnail"))
			seedSeekTilesAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("short-first/%d-tiles.jpg", index+1))
			if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
				RecordingID: recordingID, CmRanges: "{[0,60000)}",
			}); err != nil {
				t.Fatalf("seeding CM ranges for %d: %v", recordingID, err)
			}
			if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
				MediaAssetID: thumbnailID, SeekMs: 30000,
			}); err != nil {
				t.Fatalf("seeding thumbnail seek for %d: %v", recordingID, err)
			}
		default:
			// seek_tiles のみ。
			seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID,
				db.AssetKindThumbnail, nil, thumbnailRelPath(recordingID), []byte("thumbnail"))
		}
	}

	enqueuer := newRecordingEnqueuer()
	runContinuationChain(t, enqueuer, ThumbnailReconcileArgs{}, 10, func(args ThumbnailReconcileArgs) error {
		worker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 1}
		return runWorkerViaJobQFake(ctx, worker, args)
	})

	for _, recordingID := range ids[:3] {
		if got := countThumbnailJobs(t, enqueuer, recordingID); got != 1 {
			t.Errorf("thumbnail jobs for %d = %d, want 1", recordingID, got)
		}
	}
	for _, recordingID := range ids[3:] {
		if got := countSeekTilesJobs(t, enqueuer, recordingID); got != 1 {
			t.Errorf("seek_tiles jobs for %d = %d, want 1", recordingID, got)
		}
	}
	continuations := enqueuedArgs[ThumbnailReconcileArgs](enqueuer, ThumbnailReconcileArgs{}.Kind())
	if len(continuations) != 3 {
		t.Fatalf("continuation count = %d, want 3 for 1/2/3 candidate windows", len(continuations))
	}
	if !continuations[1].MissingDone || continuations[1].ReselectDone {
		t.Errorf("second continuation done flags = missing:%v reselect:%v, want true:false",
			continuations[1].MissingDone, continuations[1].ReselectDone)
	}
	if !continuations[2].MissingDone || !continuations[2].ReselectDone || continuations[2].SeekTilesDone {
		t.Errorf("third continuation done flags = missing:%v reselect:%v seek_tiles:%v, want true:true:false",
			continuations[2].MissingDone, continuations[2].ReselectDone, continuations[2].SeekTilesDone)
	}
}

type thumbnailContinuationFailingEnqueuer struct {
	*recordingEnqueuer
	continuationAttempts int
}

func (e *thumbnailContinuationFailingEnqueuer) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	if args.Kind() == (ThumbnailReconcileArgs{}).Kind() {
		e.continuationAttempts++
		return jobq.Result{}, fmt.Errorf("continuation unavailable")
	}
	return e.recordingEnqueuer.Enqueue(ctx, args, opts...)
}

func TestThumbnailReconcile_ContinuationEnqueueFailureReturnsSuccess(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	firstID := insertTestRecordingForSite(t, pool, "default", 14107)
	secondID := insertTestRecordingForSite(t, pool, "default", 14108)
	seedOriginalAsset(t, pool, mediaDir, firstID, "continuation-error/first.ts", []byte("first"))
	seedOriginalAsset(t, pool, mediaDir, secondID, "continuation-error/second.ts", []byte("second"))
	enqueuer := &thumbnailContinuationFailingEnqueuer{recordingEnqueuer: newRecordingEnqueuer()}
	w := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 1}
	if err := runWorkerViaJobQFake(context.Background(), w, ThumbnailReconcileArgs{}); err != nil {
		t.Fatalf("Work after continuation enqueue failure = %v, want nil", err)
	}
	if got := countThumbnailJobs(t, enqueuer.recordingEnqueuer, firstID); got != 1 {
		t.Errorf("thumbnail jobs after continuation enqueue failure = %d, want 1", got)
	}
	if enqueuer.continuationAttempts != 1 {
		t.Errorf("continuation enqueue attempts = %d, want 1", enqueuer.continuationAttempts)
	}
}

func TestThumbnailReconcileWorker_WorkWithoutClientErrors(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool)}
	job := ThumbnailReconcileArgs{}
	if err := runWorkerViaJobQFake(context.Background(), w, job); err == nil {
		t.Fatal("expected error when no Enqueuer is configured, got nil")
	}
}

func TestThumbnailReconcileArgs_KindAndSpec(t *testing.T) {
	if kind := (ThumbnailReconcileArgs{}).Kind(); kind != "thumbnail_reconcile" {
		t.Errorf("Kind() = %q, want thumbnail_reconcile", kind)
	}
	spec := ThumbnailReconcileArgs{}.Spec()
	if spec.Queue != "thumbnail" {
		t.Errorf("Spec().Queue = %q, want thumbnail", spec.Queue)
	}
	if !spec.Coalesce {
		t.Error("Spec().Coalesce should be true")
	}
}

func countSeekTilesJobs(t *testing.T, enqueuer *recordingEnqueuer, recordingID int64) int {
	return countQueuedJobsForRecording(t, enqueuer, "seek_tiles", recordingID)
}

// 定期パスは thumbnail だけでなく seek_tiles のギャップも埋める。
func TestThumbnailReconcile_EnqueuesMissingSeekTiles(t *testing.T) {
	pool := setupTestPool(t)
	recordingID := insertTestRecording(t, pool)
	seedOriginalAsset(t, pool, t.TempDir(), recordingID, "seek-tiles/original.m2ts", []byte("fake-ts"))
	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool), Enqueuer: newRecordingEnqueuer()}

	runThumbnailReconcilePass(t, w)

	if got := countSeekTilesJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 1 {
		t.Errorf("seek_tiles jobs after the periodic pass = %d, want 1", got)
	}
	// 同じパスが thumbnail も積む（どちらも desired − observed の差分）。
	if got := countThumbnailJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 1 {
		t.Errorf("thumbnail jobs after the periodic pass = %d, want 1", got)
	}
	if got := len(enqueuedArgs[ThumbnailReconcileArgs](w.Enqueuer.(*recordingEnqueuer), ThumbnailReconcileArgs{}.Kind())); got != 0 {
		t.Errorf("continuation jobs for short thumbnail and seek_tiles pages = %d, want 0", got)
	}
}

// 実体無しと確認済みの原本は seek_tiles の候補からも除外する（poster と同じ扱い）。
func TestThumbnailReconcile_SeekTilesSkipsKnownMissingOriginal(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := insertTestRecording(t, pool)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "seek-missing/original.m2ts", []byte("fake-ts"))
	if err := os.Remove(filepath.Join(mediaDir, "seek-missing", "original.m2ts")); err != nil {
		t.Fatalf("removing original fixture: %v", err)
	}
	if err := sqlcgen.New(pool).UpsertMissingMediaAsset(ctx, originalID); err != nil {
		t.Fatalf("marking original missing: %v", err)
	}

	w := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool), Enqueuer: newRecordingEnqueuer()}
	runThumbnailReconcilePass(t, w)
	if got := countSeekTilesJobs(t, w.Enqueuer.(*recordingEnqueuer), recordingID); got != 0 {
		t.Errorf("seek_tiles jobs for known-missing original = %d, want 0", got)
	}
}

// **再開位置は派生物の種類ごとに別に持つ。** 共通のカーソルにすると、thumbnail の
// 候補が常に上限に張り付いているとき（恒久失敗が先頭に居座る等）、そのカーソルが
// 進み続けて seek_tiles 側の候補を窓の外へ飛ばす。
//
// ここでは thumbnail の候補を録画 1・2 に、seek_tiles の候補を録画 3・4・5 に
// 分けてある。RowLimit=2 で 2 パス回すと、独立したカーソルなら 3・4・5 の
// 3 件すべてにジョブが積まれる。共通のカーソルだと 2 パス目が
// 「thumbnail が尽きたので 0 に戻る」→ seek_tiles の窓が 0 に戻り、5 が永久に
// 届かない（2 件のまま）ので落ちる。
func TestThumbnailReconcile_SeekTilesCursorIsIndependent(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()

	// 録画 1..5（event_id を 1..5 にずらす。同一キーのアクティブ行は一意制約に当たる）。
	ids := make([]int64, 0, 5)
	for i := int32(1); i <= 5; i++ {
		ids = append(ids, insertTestRecordingWithEventID(t, pool, i))
	}
	for i, id := range ids {
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cursor/%d.m2ts", i+1), []byte("fake-ts"))
	}
	// thumbnail があるのは録画 3・4・5 → thumbnail の候補は 1・2。
	for _, id := range ids[2:] {
		seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindThumbnail, nil, fmt.Sprintf("cursor/t%d.jpg", id), []byte("jpg"))
	}
	// seek_tiles があるのは録画 1・2 → seek_tiles の候補は 3・4・5。
	for _, id := range ids[:2] {
		seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindSeekTiles, nil, fmt.Sprintf("cursor/s%d.jpg", id), []byte("jpg"))
	}

	enqueuer := newRecordingEnqueuer()
	firstWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 2}
	runThumbnailReconcilePass(t, firstWorker)
	continuations := enqueuedArgs[ThumbnailReconcileArgs](enqueuer, ThumbnailReconcileArgs{}.Kind())
	if len(continuations) != 1 || continuations[0].AfterSeekTilesRecordingID != ids[3] {
		t.Fatalf("first seek_tiles continuation = %+v, want cursor after recording %d", continuations, ids[3])
	}
	secondWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: newTestMediaStore(pool), RowLimit: 2}
	runThumbnailReconcilePassArgs(t, secondWorker, continuations[0])

	for _, id := range ids[:2] {
		if got := countThumbnailJobs(t, enqueuer, id); got != 1 {
			t.Errorf("thumbnail jobs for recording %d = %d, want 1", id, got)
		}
	}
	for _, id := range ids[2:] {
		if got := countSeekTilesJobs(t, enqueuer, id); got != 1 {
			t.Errorf("seek_tiles jobs for recording %d = %d, want 1 "+
				"(the seek_tiles cursor must not be dragged along by the thumbnail window)", id, got)
		}
	}
}

// 本番の既存サムネイルには衛星表の行が無い。チャプターが無い録画は作り直さず、
// 検出済みの録画だけを作り直す（崩れるとデプロイ直後に全録画が作り直される）。
func TestThumbnailReconcile_RowlessThumbnailOnlyReselectsWithChapters(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)
	seed := func(site int32) int64 {
		id := insertTestRecordingForSite(t, pool, "default", site)
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("rowless/%d.ts", id), []byte("fake-ts"))
		seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindThumbnail, nil,
			thumbnailRelPath(id), []byte("old-thumbnail"))
		return id
	}
	noChapters := seed(951)
	detected := seed(952)
	if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: detected, CmRanges: "{[0,60000)}",
	}); err != nil {
		t.Fatal(err)
	}

	reconcileWorker := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool), Enqueuer: newRecordingEnqueuer()}
	runThumbnailReconcilePass(t, reconcileWorker)
	if got := countThumbnailJobs(t, reconcileWorker.Enqueuer.(*recordingEnqueuer), noChapters); got != 0 {
		t.Errorf("thumbnail jobs without chapters = %d, want 0", got)
	}
	if got := countThumbnailJobs(t, reconcileWorker.Enqueuer.(*recordingEnqueuer), detected); got != 1 {
		t.Errorf("thumbnail jobs with CM detection = %d, want 1", got)
	}

	w := &ThumbnailWorker{
		Publisher: newTestMediaStore(pool),
		Media:     newTestMediaStore(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(),
		runCmd: func(context.Context, string, ...string) ([]byte, error) {
			t.Error("recording without chapters must not run ffmpeg/ffprobe")
			return nil, fmt.Errorf("unexpected command")
		},
	}
	job := ThumbnailJobArgs{RecordingID: noChapters}
	if err := runWorkerViaJobQFake(ctx, w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	cur, err := q.LockActiveThumbnailMediaAsset(ctx, noChapters)
	if err != nil {
		t.Fatal(err)
	}
	if cur.RelPath != thumbnailRelPath(noChapters) {
		t.Errorf("rel_path = %q, want unchanged", cur.RelPath)
	}
}

// 使える入力が無い録画（原本は missing、encoded 無し）は、ジョブがエラーにならず
// サムネイルも変えず、reconcile も投入しない。
func TestThumbnailWorker_NoUsableInputKeepsThumbnailWithoutError(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)
	id := insertTestRecordingForSite(t, pool, "default", 961)
	originalID := seedOriginalAsset(t, pool, mediaDir, id, "noinput/original.ts", []byte("fake-ts"))
	thumbID := seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindThumbnail, nil,
		thumbnailRelPath(id), []byte("old-thumbnail"))
	if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: thumbID, SeekMs: 30000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: id, CmRanges: "{[0,60000)}",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO missing_media_assets (media_asset_id) VALUES ($1)`, originalID); err != nil {
		t.Fatal(err)
	}

	w := &ThumbnailWorker{
		Publisher: newTestMediaStore(pool),
		Media:     newTestMediaStore(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(),
		runCmd: func(context.Context, string, ...string) ([]byte, error) {
			t.Error("no usable input must not run ffmpeg/ffprobe")
			return nil, fmt.Errorf("unexpected command")
		},
	}
	job := ThumbnailJobArgs{RecordingID: id}
	if err := runWorkerViaJobQFake(ctx, w, job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	cur, err := q.LockActiveThumbnailMediaAsset(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if cur.RelPath != thumbnailRelPath(id) || cur.SeekMs == nil || *cur.SeekMs != 30000 {
		t.Errorf("thumbnail = %q seek %v, want unchanged %q / 30000", cur.RelPath, cur.SeekMs, thumbnailRelPath(id))
	}
	reconcileWorker := &ThumbnailReconcileWorker{Media: newTestMediaStore(pool), Enqueuer: newRecordingEnqueuer()}
	runThumbnailReconcilePass(t, reconcileWorker)
	if got := countThumbnailJobs(t, reconcileWorker.Enqueuer.(*recordingEnqueuer), id); got != 0 {
		t.Errorf("thumbnail jobs = %d, want 0", got)
	}
}
