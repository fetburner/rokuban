package thumbnail

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func runThumbnailReconcilePass(t *testing.T, w *ThumbnailReconcileWorker) {
	runThumbnailReconcilePassArgs(t, w, jobs.ThumbnailReconcileArgs{})
}

func runThumbnailReconcilePassArgs(t *testing.T, w *ThumbnailReconcileWorker, args jobs.ThumbnailReconcileArgs) {
	t.Helper()
	if _, ok := w.Enqueuer.(*jobqfaketest.RecordingEnqueuer); !ok {
		w.Enqueuer = jobqfaketest.NewRecordingEnqueuer()
	}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, args), "ThumbnailReconcileWorker.Work")
}

// runContinuationChain は first から始め、各パスが投入した継続を投入順に実行する。
// 継続が止まらない（カーソルが進まない）変異でハングせず、maxPasses を超えたら
// アサーションとして落とす。
func runContinuationChain[A jobq.Args](t *testing.T, enqueuer *jobqfaketest.RecordingEnqueuer, first A, maxPasses int, run func(A) error) {
	t.Helper()
	args := first
	for pass, next := 1, 0; ; pass++ {
		if pass > maxPasses {
			t.Fatalf("reconcile chain still continuing after %d passes; cursor is not advancing", maxPasses)
		}
		testutil.RequireNoError(t, run(args), fmt.Sprintf("reconcile pass %d", pass))
		continuations := recordedArgs[A](t, enqueuer.CallsOfKind(args.Kind()))
		if next >= len(continuations) {
			return
		}
		args = continuations[next]
		next++
	}
}

func TestThumbnailReconcile_TargetedPassFiltersAllCandidatesAndLeavesFullStateAlone(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	first := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 981
	})
	target := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 982
	})
	third := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 983
	})
	unrelatedMissing := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 984
	})
	for i, id := range []int64{first, target, third, unrelatedMissing} {
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("targeted/%d.ts", i), []byte("fake-ts"))
	}
	// The selected recording also exercises the reselection query. Its current seek
	// points inside a detected CM range, while its poster and seek-tile jobs already
	// exist or are missing as separate observations.
	thumbnailID := seedMediaAsset(t, pool, mediaDir, target,
		store.AssetKindThumbnail, nil, thumbnailRelPath(target, 0), []byte("old-thumbnail"))
	thirdThumbnailID := seedMediaAsset(t, pool, mediaDir, third,
		store.AssetKindThumbnail, nil, thumbnailRelPath(third, 0), []byte("old-thumbnail"))
	testutil.RequireNoError(t, sqlcgen.New(pool).SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: target,
		CmRanges:    "{[0,60000)}",
	}), "seeding CM ranges")
	testutil.RequireNoError(t, sqlcgen.New(pool).SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: third,
		CmRanges:    "{[0,60000)}",
	}), "seeding unrelated CM ranges")
	testutil.RequireNoError(t, sqlcgen.New(pool).UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: thumbnailID,
		SeekMs:       30000,
	}), "seeding thumbnail seek")
	testutil.RequireNoError(t, sqlcgen.New(pool).UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: thirdThumbnailID,
		SeekMs:       30000,
	}), "seeding unrelated thumbnail seek")

	metrics.ThumbnailReconcileCandidates.Set(19)
	t.Cleanup(func() {
		metrics.ThumbnailReconcileCandidates.Set(0)
	})
	// RowLimit 1 にすると、first の対象パスでは missing と seek_tiles、target の対象パスでは
	// 再選択と seek_tiles の窓が満杯になる。窓が満杯になる条件でなければ、対象パスが
	// 継続を積まないことは固定にならない。
	markers := pgstoretest.Reservations(pool)
	w := &ThumbnailReconcileWorker{Media: pgstoretest.Media(pool), RowLimit: 1, Markers: markers}

	runThumbnailReconcilePassArgs(t, w, jobs.ThumbnailReconcileArgs{RecordingID: first})
	for _, tc := range []struct {
		id        int64
		thumbnail int
		seekTiles int
	}{{first, 1, 1}, {target, 0, 0}, {third, 0, 0}} {
		if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), tc.id); got != tc.thumbnail {
			t.Errorf("thumbnail jobs for recording %d = %d, want %d", tc.id, got, tc.thumbnail)
		}
		if got := countQueuedJobsForRecording(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), "seek_tiles", tc.id); got != tc.seekTiles {
			t.Errorf("seek_tiles jobs for recording %d = %d, want %d", tc.id, got, tc.seekTiles)
		}
	}
	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), unrelatedMissing); got != 0 {
		t.Errorf("thumbnail jobs for unrelated missing-poster recording = %d, want 0", got)
	}
	if got := countQueuedJobsForRecording(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), "seek_tiles", unrelatedMissing); got != 0 {
		t.Errorf("seek_tiles jobs for unrelated missing-poster recording = %d, want 0", got)
	}
	runThumbnailReconcilePassArgs(t, w, jobs.ThumbnailReconcileArgs{RecordingID: target})
	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), target); got != 1 {
		t.Errorf("thumbnail jobs for reselection target = %d, want 1", got)
	}
	if got := countQueuedJobsForRecording(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), "seek_tiles", target); got != 1 {
		t.Errorf("seek_tiles jobs for reselection target = %d, want 1", got)
	}
	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), third); got != 0 {
		t.Errorf("thumbnail jobs for unrelated recording = %d, want 0", got)
	}
	if got := len(w.Enqueuer.(*jobqfaketest.RecordingEnqueuer).CallsOfKind("thumbnail_reconcile")); got != 0 {
		t.Errorf("targeted passes enqueued %d continuation jobs, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.ThumbnailReconcileCandidates); got != 19 {
		t.Errorf("candidate gauge = %v, want unchanged 19", got)
	}
	// 対象パスは全件パスの完走（loop_success_markers の thumbnail）を記録しない。
	saved, err := markers.ReadLoopMarkers(context.Background(), store.LoopReconcilePass)
	testutil.RequireNoError(t, err, "ReadLoopMarkers")
	if at, ok := saved[store.ReconcilePassThumbnail]; ok {
		t.Errorf("targeted pass recorded a full thumbnail pass at %v, want none", at)
	}
}

func countQueuedJobsForRecording(t *testing.T, enqueuer *jobqfaketest.RecordingEnqueuer, kind string, recordingID int64) int {
	t.Helper()
	count := 0
	for _, call := range enqueuer.CallsOfKind(kind) {
		switch args := call.Args.(type) {
		case jobs.ThumbnailJobArgs:
			if args.RecordingID == recordingID {
				count++
			}
		case jobs.SeekTilesJobArgs:
			if args.RecordingID == recordingID {
				count++
			}
		default:
			t.Fatalf("unexpected args type %T for %s", call.Args, kind)
		}
	}
	return count
}

func countThumbnailJobs(t *testing.T, enqueuer *jobqfaketest.RecordingEnqueuer, recordingID int64) int {
	return countQueuedJobsForRecording(t, enqueuer, "thumbnail", recordingID)
}

// ヒント投入が無く、edge record の削除で record_sweep からも再投入できない
// 状態でも、定期パスが thumbnail ジョブを作り、通常の worker が thumbnail を
// コミットできることを端から端まで固定する。
func TestThumbnailReconcile_ReenqueuesAfterLostHintAndDeletedEdgeRecord(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	scratchDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, recordingID, "lost-hint/original.m2ts", []byte("fake-ts"))
	w := &ThumbnailReconcileWorker{Media: pgstoretest.Media(pool), Enqueuer: jobqfaketest.NewRecordingEnqueuer()}

	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID); got != 0 {
		t.Fatalf("thumbnail jobs before the periodic pass = %d, want 0", got)
	}

	runThumbnailReconcilePass(t, w)
	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID); got != 1 {
		t.Fatalf("thumbnail jobs after the periodic pass = %d, want 1", got)
	}

	thumbnailWorker := &ThumbnailWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool),
		MediaDir:        mediaDir,
		ScratchDir:      scratchDir,
		runCmd:          fakeThumbnailTools(t, 60),
	}
	job := jobs.ThumbnailJobArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), thumbnailWorker, job), "ThumbnailWorker.Work")
	_, err := sqlcgen.New(pool).GetActiveThumbnailMediaAssetID(context.Background(), sqlcgen.GetActiveThumbnailMediaAssetIDParams{
		RecordingID: recordingID, StoreName: testStoreNameArg(),
	})
	testutil.RequireNoError(t, err, "thumbnail was not born after periodic recovery")
}

func TestThumbnailReconcile_EnqueuesOnlyWhenRecordedThumbnailIsInCM(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	inCM := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 941
	})
	inKeep := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 942
	})
	q := sqlcgen.New(pool)
	for _, recordingID := range []int64{inCM, inKeep} {
		seedOriginalAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("reselect/%d.ts", recordingID), []byte("fake-ts"))
		thumbID := seedMediaAsset(t, pool, mediaDir, recordingID,
			store.AssetKindThumbnail, nil, thumbnailRelPath(recordingID, 0), []byte("old-thumbnail"))
		testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
			RecordingID: recordingID,
			CmRanges:    "{[0,60000)}",
		}), fmt.Sprintf("seeding CM ranges for %d", recordingID))
		seek := int64(30000)
		if recordingID == inKeep {
			seek = 60000
		}
		testutil.RequireNoError(t, q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
			MediaAssetID: thumbID,
			SeekMs:       seek,
		}), fmt.Sprintf("seeding thumbnail seek for %d", recordingID))
	}

	w := &ThumbnailReconcileWorker{Media: pgstoretest.Media(pool), Enqueuer: jobqfaketest.NewRecordingEnqueuer()}
	runThumbnailReconcilePass(t, w)
	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), inCM); got != 1 {
		t.Errorf("thumbnail jobs for CM frame = %d, want 1", got)
	}
	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), inKeep); got != 0 {
		t.Errorf("thumbnail jobs for keep frame = %d, want 0", got)
	}
}

func TestThumbnailReconcile_ReselectionWindowContinuesWithoutEnqueueing(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)
	ids := make([]int64, 0, 2)
	for eventID := int32(15161); eventID <= 15162; eventID++ {
		recordingID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
			fixtureParams.EventID = eventID
		})
		ids = append(ids, recordingID)
		seedOriginalAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("reselect-full-window/%d.ts", eventID), []byte("fake-ts"))
		thumbnailID := seedMediaAsset(t, pool, mediaDir, recordingID,
			store.AssetKindThumbnail, nil, thumbnailRelPath(recordingID, 0), []byte("old-thumbnail"))
		seedSeekTilesAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("reselect-full-window/%d-tiles.jpg", eventID))
		testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
			RecordingID: recordingID,
			CmRanges:    "{[60000,120000)}",
		}), fmt.Sprintf("seeding CM ranges for %d", recordingID))
		testutil.RequireNoError(t, q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
			MediaAssetID: thumbnailID,
			SeekMs:       30000,
		}), fmt.Sprintf("seeding thumbnail seek for %d", recordingID))
	}

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	worker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 1}
	runThumbnailReconcilePass(t, worker)

	continuations := recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
	if len(continuations) != 1 || continuations[0].AfterReselectRecordingID != ids[0] {
		t.Fatalf("reselection continuations = %+v, want one continuation after recording %d", continuations, ids[0])
	}
	secondWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 1}
	runThumbnailReconcilePassArgs(t, secondWorker, continuations[0])
	continuations = recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
	if len(continuations) != 2 || continuations[1].AfterReselectRecordingID != ids[1] {
		t.Fatalf("second reselection continuation = %+v, want one continuation after recording %d", continuations, ids[1])
	}
	for _, recordingID := range ids {
		if got := countThumbnailJobs(t, enqueuer, recordingID); got != 0 {
			t.Errorf("thumbnail jobs for no-op reselection recording %d after both pages = %d, want 0", recordingID, got)
		}
	}
}

// 再選択候補の窓も missing thumbnail の窓と独立して進める。1・2 は
// thumbnail 欠落、3・4・5 は既存 thumbnail と chapter 候補に分け、2 パスで
// reselect 側の末尾 5 まで判定されることを確認する。
func TestThumbnailReconcile_ReselectCursorIsIndependent(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	ids := make([]int64, 0, 5)
	for eventID := int32(1); eventID <= 5; eventID++ {
		ids = append(ids, dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) { fixtureParams.EventID = eventID }))
	}
	q := sqlcgen.New(pool)
	for i, recordingID := range ids {
		seedOriginalAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("reselect-cursor/%d.ts", i+1), []byte("fake-ts"))
		if i < 2 {
			continue
		}
		thumbnailID := seedMediaAsset(t, pool, mediaDir, recordingID,
			store.AssetKindThumbnail, nil, thumbnailRelPath(recordingID, 0), []byte("old-thumbnail"))
		testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
			RecordingID: recordingID,
			CmRanges:    "{[0,60000)}",
		}), fmt.Sprintf("seeding CM ranges for %d", recordingID))
		seek := int64(60000) // 3・4 は keep 内なので再選択しない。
		if i == 4 {
			seek = 30000 // 5 は CM 内なので再選択ジョブが必要。
		}
		testutil.RequireNoError(t, q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
			MediaAssetID: thumbnailID,
			SeekMs:       seek,
		}), fmt.Sprintf("seeding thumbnail seek for %d", recordingID))
	}

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	firstWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 2}
	runThumbnailReconcilePass(t, firstWorker)
	continuations := recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
	if len(continuations) != 1 || continuations[0].AfterReselectRecordingID != ids[3] {
		t.Fatalf("first reselection continuation = %+v, want cursor after recording %d", continuations, ids[3])
	}
	secondWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 2}
	runThumbnailReconcilePassArgs(t, secondWorker, continuations[0])

	if got := countThumbnailJobs(t, enqueuer, ids[4]); got != 1 {
		t.Errorf("reselection jobs for recording %d = %d, want 1 after the second window", ids[4], got)
	}
}

func TestThumbnailReconcile_DoesNotRepeatUnactionableReselections(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)
	allCutID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) { fixtureParams.EventID = 61 })
	seedOriginalAsset(t, pool, mediaDir, allCutID, "reselect-noop/all-cut.ts", []byte("fake-ts"))
	allCutThumbnailID := seedMediaAsset(t, pool, mediaDir, allCutID,
		store.AssetKindThumbnail, nil, thumbnailRelPath(allCutID, 0), []byte("old-thumbnail"))
	cutOnlyID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) { fixtureParams.EventID = 62 })
	cutProfile := "cut-only"
	cutAssetID := seedMediaAsset(t, pool, mediaDir, cutOnlyID,
		store.AssetKindEncoded, &cutProfile, "reselect-noop/cut-only.mp4", []byte("fake-mp4"))
	cutThumbnailID := seedMediaAsset(t, pool, mediaDir, cutOnlyID,
		store.AssetKindThumbnail, nil, thumbnailRelPath(cutOnlyID, 0), []byte("old-thumbnail"))
	testutil.MustExec(t, pool, "seeding cut-only frozen keep ranges",
		`INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES ($1, $2::int8multirange)`,
		cutAssetID, "{[0,300000)}")
	for _, recordingID := range []int64{allCutID, cutOnlyID} {
		testutil.MustExec(t, pool, fmt.Sprintf("adopting chapters for %d", recordingID),
			`INSERT INTO recording_chapter_ownership (recording_id) VALUES ($1)`, recordingID)
	}
	dbtest.ChapterSpan(t, pool, allCutID, func(p *dbtest.ChapterSpanParams) { p.StartMs, p.EndMs = 0, 1800000 })
	dbtest.ChapterSpan(t, pool, cutOnlyID, func(p *dbtest.ChapterSpanParams) { p.StartMs, p.EndMs = 25000, 35000 })
	testutil.RequireNoError(t, q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: allCutThumbnailID,
		SeekMs:       30000,
	}), "seeding all-cut thumbnail seek")
	testutil.RequireNoError(t, q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: cutThumbnailID,
		SeekMs:       30000,
	}), "seeding cut-only thumbnail seek")

	w := &ThumbnailReconcileWorker{Media: pgstoretest.Media(pool)}
	runThumbnailReconcilePass(t, w)
	runThumbnailReconcilePass(t, w)
	for _, recordingID := range []int64{allCutID, cutOnlyID} {
		if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID); got != 0 {
			t.Errorf("thumbnail jobs for no-op reselection recording %d = %d, want 0 after two passes", recordingID, got)
		}
	}
}

// ファイルが無いことを delete_reconcile が確認した原本は定期パスから除外する。
// マーカーが消えた後は同じ録画を回収対象へ戻す。
func TestThumbnailReconcile_SkipsKnownMissingOriginalUntilRestored(t *testing.T) {
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, "missing/original.m2ts", []byte("fake-ts"))
	testutil.RequireNoError(t, os.Remove(filepath.Join(mediaDir, "missing", "original.m2ts")), "removing original fixture")
	copyID := dbtest.MediaAssetCopyID(t, pool, originalID, dbtest.StoreName)
	testutil.RequireNoError(t, q.UpsertMissingMediaAsset(ctx, copyID), "marking original missing")

	w := &ThumbnailReconcileWorker{Media: pgstoretest.Media(pool), Enqueuer: jobqfaketest.NewRecordingEnqueuer()}
	runThumbnailReconcilePass(t, w)
	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID); got != 0 {
		t.Fatalf("thumbnail jobs for known-missing original = %d, want 0", got)
	}
	if got := countSeekTilesJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID); got != 0 {
		t.Fatalf("seek_tiles jobs for known-missing original = %d, want 0", got)
	}
	if got := promtestutil.ToFloat64(metrics.ThumbnailReconcileCandidates); got != 0 {
		t.Fatalf("thumbnail reconcile candidates for known-missing original = %v, want 0", got)
	}

	testutil.RequireNoError(t, q.DeleteMissingMediaAsset(ctx, copyID), "clearing restored original marker")
	runThumbnailReconcilePass(t, w)
	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID); got != 1 {
		t.Fatalf("thumbnail jobs after clearing restored marker = %d, want 1", got)
	}
	if got := countSeekTilesJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID); got != 1 {
		t.Fatalf("seek_tiles jobs after clearing restored marker = %d, want 1", got)
	}
}

// RowLimit は録画単位の上限であり、先頭候補が解消しなくても窓を回して
// 後続候補へ到達する。
func TestThumbnailReconcile_WindowRotatesPastStuckCandidates(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	firstID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 801
	})
	secondID := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 802
	})
	seedOriginalAsset(t, pool, mediaDir, firstID, "window/first.m2ts", []byte("first"))
	seedOriginalAsset(t, pool, mediaDir, secondID, "window/second.m2ts", []byte("second"))

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	firstWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 1}
	runThumbnailReconcilePass(t, firstWorker)
	if got := countThumbnailJobs(t, enqueuer, firstID); got != 1 {
		t.Fatalf("first recording jobs after first pass = %d, want 1", got)
	}
	if got := countThumbnailJobs(t, enqueuer, secondID); got != 0 {
		t.Fatalf("second recording jobs after first pass = %d, want 0", got)
	}
	continuations := recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
	if len(continuations) != 1 || continuations[0].AfterRecordingID != firstID {
		t.Fatalf("continuation = %+v, want cursor after recording %d", continuations, firstID)
	}

	secondWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 1}
	runThumbnailReconcilePassArgs(t, secondWorker, continuations[0])
	if got := countThumbnailJobs(t, enqueuer, secondID); got != 1 {
		t.Fatalf("second recording jobs after rotated pass = %d, want 1", got)
	}
}

// TestThumbnailReconcile_ContinuationSurvivesFreshWorker mirrors --once: each
// continuation starts a new worker, and all three candidate windows advance from
// the positions produced by the previous pass.
func TestThumbnailReconcile_ContinuationSurvivesFreshWorker(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	ids := make([]int64, 0, 6)
	for eventID := int32(14101); eventID <= 14106; eventID++ {
		ids = append(ids, dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) { fixtureParams.EventID = eventID }))
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
			thumbnailID := seedMediaAsset(t, pool, mediaDir, recordingID,
				store.AssetKindThumbnail, nil, thumbnailRelPath(recordingID, 0), []byte("old-thumbnail"))
			seedSeekTilesAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("continuation/%d-tiles.jpg", i+1))
			testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
				RecordingID: recordingID,
				CmRanges:    "{[0,60000)}",
			}), fmt.Sprintf("seeding CM ranges for %d", recordingID))
			testutil.RequireNoError(t, q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
				MediaAssetID: thumbnailID,
				SeekMs:       30000,
			}), fmt.Sprintf("seeding thumbnail seek for %d", recordingID))
		case 4, 5:
			// These need seek_tiles only; an existing thumbnail keeps them out of the
			// missing-poster window and the absent chapter/CM state keeps them out of reselection.
			seedMediaAsset(t, pool, mediaDir, recordingID,
				store.AssetKindThumbnail, nil, thumbnailRelPath(recordingID, 0), []byte("thumbnail"))
		}
	}

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	firstWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 1}
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

	continuations := recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
	if len(continuations) != 1 || continuations[0].AfterRecordingID != ids[0] ||
		continuations[0].AfterReselectRecordingID != ids[2] ||
		continuations[0].AfterSeekTilesRecordingID != ids[4] {
		t.Fatalf("first continuation = %+v, want independent cursors after %d, %d, and %d",
			continuations, ids[0], ids[2], ids[4])
	}
	secondWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 1}
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
	continuations = recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
	if len(continuations) != 2 || continuations[1].AfterRecordingID != ids[1] ||
		continuations[1].AfterReselectRecordingID != ids[3] ||
		continuations[1].AfterSeekTilesRecordingID != ids[5] {
		t.Fatalf("second continuation = %+v, want independent cursors after %d, %d, and %d",
			continuations, ids[1], ids[3], ids[5])
	}
	thirdWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 1}
	runThumbnailReconcilePassArgs(t, thirdWorker, continuations[1])
	if got := len(enqueuer.CallsOfKind("thumbnail_reconcile")); got != 2 {
		t.Errorf("continuation jobs after all windows reached the end = %d, want 2", got)
	}
}

func TestThumbnailReconcile_ExhaustedWindowNotRestarted(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	ids := make([]int64, 0, 5)
	for eventID := int32(15121); eventID <= 15125; eventID++ {
		ids = append(ids, dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) { fixtureParams.EventID = eventID }))
	}
	q := sqlcgen.New(pool)
	for index, recordingID := range ids {
		seedOriginalAsset(t, pool, mediaDir, recordingID,
			fmt.Sprintf("unequal-windows/%d.ts", index+1), []byte("fake-ts"))
		if index < 3 {
			continue
		}
		thumbnailID := seedMediaAsset(t, pool, mediaDir, recordingID,
			store.AssetKindThumbnail, nil, thumbnailRelPath(recordingID, 0), []byte("old-thumbnail"))
		seedSeekTilesAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("unequal-windows/%d-tiles.jpg", index+1))
		testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
			RecordingID: recordingID,
			CmRanges:    "{[0,60000)}",
		}), fmt.Sprintf("seeding CM ranges for %d", recordingID))
		testutil.RequireNoError(t, q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
			MediaAssetID: thumbnailID,
			SeekMs:       30000,
		}), fmt.Sprintf("seeding thumbnail seek for %d", recordingID))
	}
	for _, index := range []int{1, 2} {
		seedSeekTilesAsset(t, pool, mediaDir, ids[index], fmt.Sprintf("unequal-windows/%d-tiles.jpg", index+1))
	}

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	runContinuationChain(t, enqueuer, jobs.ThumbnailReconcileArgs{}, 10, func(args jobs.ThumbnailReconcileArgs) error {
		worker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 1}
		return jobqfaketest.RunWorkerOnce(ctx, worker, args)
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
	continuations := recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
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
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	ctx := context.Background()
	mediaDir := t.TempDir()
	ids := make([]int64, 0, 6)
	for eventID := int32(15131); eventID <= 15136; eventID++ {
		ids = append(ids, dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) { fixtureParams.EventID = eventID }))
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
			thumbnailID := seedMediaAsset(t, pool, mediaDir, recordingID,
				store.AssetKindThumbnail, nil, thumbnailRelPath(recordingID, 0), []byte("old-thumbnail"))
			seedSeekTilesAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("short-first/%d-tiles.jpg", index+1))
			testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
				RecordingID: recordingID, CmRanges: "{[0,60000)}",
			}), fmt.Sprintf("seeding CM ranges for %d", recordingID))
			testutil.RequireNoError(t, q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
				MediaAssetID: thumbnailID, SeekMs: 30000,
			}), fmt.Sprintf("seeding thumbnail seek for %d", recordingID))
		default:
			// seek_tiles のみ。
			seedMediaAsset(t, pool, mediaDir, recordingID,
				store.AssetKindThumbnail, nil, thumbnailRelPath(recordingID, 0), []byte("thumbnail"))
		}
	}

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	runContinuationChain(t, enqueuer, jobs.ThumbnailReconcileArgs{}, 10, func(args jobs.ThumbnailReconcileArgs) error {
		worker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 1}
		return jobqfaketest.RunWorkerOnce(ctx, worker, args)
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
	continuations := recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
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

func TestThumbnailReconcileWorker_WorkWithoutClientErrors(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}

	w := &ThumbnailReconcileWorker{Media: pgstoretest.Media(pool)}
	job := jobs.ThumbnailReconcileArgs{}
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, job); err == nil {
		t.Fatal("expected error when no Enqueuer is configured, got nil")
	}
}

func TestThumbnailReconcileArgs_KindAndSpec(t *testing.T) {
	t.Parallel()
	if kind := (jobs.ThumbnailReconcileArgs{}).Kind(); kind != "thumbnail_reconcile" {
		t.Errorf("Kind() = %q, want thumbnail_reconcile", kind)
	}
	spec := jobs.ThumbnailReconcileArgs{}.Spec()
	if spec.Queue != "cleanup" {
		t.Errorf("Spec().Queue = %q, want cleanup (DB-only reconcile is not store-qualified)", spec.Queue)
	}
	if !spec.Coalesce {
		t.Error("Spec().Coalesce should be true")
	}
}

func countSeekTilesJobs(t *testing.T, enqueuer *jobqfaketest.RecordingEnqueuer, recordingID int64) int {
	return countQueuedJobsForRecording(t, enqueuer, "seek_tiles", recordingID)
}

// 定期パスは thumbnail だけでなく seek_tiles のギャップも埋める。
func TestThumbnailReconcile_EnqueuesMissingSeekTiles(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	seedOriginalAsset(t, pool, t.TempDir(), recordingID, "seek-tiles/original.m2ts", []byte("fake-ts"))
	w := &ThumbnailReconcileWorker{Media: pgstoretest.Media(pool), Enqueuer: jobqfaketest.NewRecordingEnqueuer()}

	runThumbnailReconcilePass(t, w)

	if got := countSeekTilesJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID); got != 1 {
		t.Errorf("seek_tiles jobs after the periodic pass = %d, want 1", got)
	}
	// 同じパスが thumbnail も積む（どちらも desired − observed の差分）。
	if got := countThumbnailJobs(t, w.Enqueuer.(*jobqfaketest.RecordingEnqueuer), recordingID); got != 1 {
		t.Errorf("thumbnail jobs after the periodic pass = %d, want 1", got)
	}
	if got := len(w.Enqueuer.(*jobqfaketest.RecordingEnqueuer).CallsOfKind("thumbnail_reconcile")); got != 0 {
		t.Errorf("continuation jobs for short thumbnail and seek_tiles pages = %d, want 0", got)
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
	t.Parallel()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()

	// 録画 1..5（event_id を 1..5 にずらす。同一キーのアクティブ行は一意制約に当たる）。
	ids := make([]int64, 0, 5)
	for i := int32(1); i <= 5; i++ {
		ids = append(ids, dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) { fixtureParams.EventID = i }))
	}
	for i, id := range ids {
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cursor/%d.m2ts", i+1), []byte("fake-ts"))
	}
	// thumbnail があるのは録画 3・4・5 → thumbnail の候補は 1・2。
	for _, id := range ids[2:] {
		seedMediaAsset(t, pool, mediaDir, id, store.AssetKindThumbnail, nil, fmt.Sprintf("cursor/t%d.jpg", id), []byte("jpg"))
	}
	// seek_tiles があるのは録画 1・2 → seek_tiles の候補は 3・4・5。
	for _, id := range ids[:2] {
		seedMediaAsset(t, pool, mediaDir, id, store.AssetKindSeekTiles, nil, fmt.Sprintf("cursor/s%d.jpg", id), []byte("jpg"))
	}

	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	firstWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 2}
	runThumbnailReconcilePass(t, firstWorker)
	continuations := recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
	if len(continuations) != 1 || continuations[0].AfterSeekTilesRecordingID != ids[3] {
		t.Fatalf("first seek_tiles continuation = %+v, want cursor after recording %d", continuations, ids[3])
	}
	secondWorker := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 2}
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

// 本番の既存サムネイルには衛星表の行が無い。ここでは、検出済みの録画が作り直しの
// 候補として積まれることと、チャプターも検出も無い録画で ThumbnailWorker が ffmpeg を
// 起動しないことを見る。チャプターも検出も無い録画が候補から外れることは、internal/db の
// TestListThumbnailReselectCandidates が検査する。
func TestThumbnailReconcile_RowlessThumbnailOnlyReselectsWithChapters(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)
	seed := func(site int32) int64 {
		id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
			fixtureParams.Site, fixtureParams.EventID = "default", site
		})
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("rowless/%d.ts", id), []byte("fake-ts"))
		seedMediaAsset(t, pool, mediaDir, id, store.AssetKindThumbnail, nil,
			thumbnailRelPath(id, 0), []byte("old-thumbnail"))
		return id
	}
	noChapters := seed(951)
	detected := seed(952)
	testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: detected, CmRanges: "{[0,60000)}",
	}), "q.SaveCMDetection")

	reconcileWorker := &ThumbnailReconcileWorker{Media: pgstoretest.Media(pool), Enqueuer: jobqfaketest.NewRecordingEnqueuer()}
	runThumbnailReconcilePass(t, reconcileWorker)
	if got := countThumbnailJobs(t, reconcileWorker.Enqueuer.(*jobqfaketest.RecordingEnqueuer), detected); got != 1 {
		t.Errorf("thumbnail jobs with CM detection = %d, want 1", got)
	}

	w := &ThumbnailWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(),
		runCmd: func(context.Context, string, ...string) ([]byte, error) {
			t.Error("recording without chapters must not run ffmpeg/ffprobe")
			return nil, fmt.Errorf("unexpected command")
		},
	}
	job := jobs.ThumbnailJobArgs{RecordingID: noChapters}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, job), "Work() error")
	cur, err := q.LockActiveThumbnailMediaAsset(ctx, sqlcgen.LockActiveThumbnailMediaAssetParams{RecordingID: noChapters, StoreName: dbtest.StoreName})
	testutil.RequireNoError(t, err, "q.LockActiveThumbnailMediaAsset")
	if cur.RelPath != thumbnailRelPath(noChapters, 0) {
		t.Errorf("rel_path = %q, want unchanged", cur.RelPath)
	}
}

// 使える入力が無い録画（原本は missing、encoded 無し）は、ジョブがエラーにならず
// サムネイルも変えず、reconcile も投入しない。
func TestThumbnailWorker_NoUsableInputKeepsThumbnailWithoutError(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)
	id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.Site, fixtureParams.EventID = "default", 961
	})
	originalID := seedOriginalAsset(t, pool, mediaDir, id, "noinput/original.ts", []byte("fake-ts"))
	thumbID := seedMediaAsset(t, pool, mediaDir, id, store.AssetKindThumbnail, nil,
		thumbnailRelPath(id, 0), []byte("old-thumbnail"))
	testutil.RequireNoError(t, q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: thumbID, SeekMs: 30000,
	}), "q.UpsertMediaAssetThumbnailSeek")
	testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: id, CmRanges: "{[0,60000)}",
	}), "q.SaveCMDetection")
	testutil.MustExec(t, pool, "marking original as missing", `INSERT INTO missing_media_assets (media_asset_copy_id) SELECT id FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'test-store'`, originalID)

	w := &ThumbnailWorker{
		Objects:         testFSObjectStore(mediaDir),
		ObjectPublisher: pgstoretest.Media(pool),
		Media:           pgstoretest.Media(pool), MediaDir: mediaDir, ScratchDir: t.TempDir(),
		runCmd: func(context.Context, string, ...string) ([]byte, error) {
			t.Error("no usable input must not run ffmpeg/ffprobe")
			return nil, fmt.Errorf("unexpected command")
		},
	}
	job := jobs.ThumbnailJobArgs{RecordingID: id}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, job), "Work() error")
	cur, err := q.LockActiveThumbnailMediaAsset(ctx, sqlcgen.LockActiveThumbnailMediaAssetParams{RecordingID: id, StoreName: dbtest.StoreName})
	testutil.RequireNoError(t, err, "q.LockActiveThumbnailMediaAsset")
	if cur.RelPath != thumbnailRelPath(id, 0) || cur.SeekMs == nil || *cur.SeekMs != 30000 {
		t.Errorf("thumbnail = %q seek %v, want unchanged %q / 30000", cur.RelPath, cur.SeekMs, thumbnailRelPath(id, 0))
	}
	reconcileWorker := &ThumbnailReconcileWorker{Media: pgstoretest.Media(pool), Enqueuer: jobqfaketest.NewRecordingEnqueuer()}
	runThumbnailReconcilePass(t, reconcileWorker)
	if got := countThumbnailJobs(t, reconcileWorker.Enqueuer.(*jobqfaketest.RecordingEnqueuer), id); got != 0 {
		t.Errorf("thumbnail jobs = %d, want 0", got)
	}
}
