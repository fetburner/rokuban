package cmdetect

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// vanishingLocator は、候補の列挙（本物）が録画を出した後、置き場の引き当てで原本のコピーを
// 無くす。候補に出てから原本が消える割り込みを、本物の列挙と引き当ての経路で作る。
type vanishingLocator struct {
	store.OriginalLocator
	pool        *pgxpool.Pool
	recordingID int64
	vanished    bool
}

func (m *vanishingLocator) OriginalStores(ctx context.Context, ids []int64) (map[int64]string, error) {
	if !m.vanished && slices.Contains(ids, m.recordingID) {
		m.vanished = true
		if _, err := m.pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleting' WHERE media_asset_id IN (SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original')`, m.recordingID); err != nil {
			return nil, err
		}
	}
	return m.OriginalLocator.OriginalStores(ctx, ids)
}

// 検出の候補に出た録画の原本が、置き場を引く前に消えたら検出ジョブを積まない。
func TestCMDetectReconcile_OriginalGoneAfterListingIsNotEnqueued(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(15121)))
	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	locator := &vanishingLocator{OriginalLocator: pgstoretest.Media(pool), pool: pool, recordingID: id}
	w := &CMDetectReconcileWorker{Enqueuer: enqueuer, Locator: locator, DetectionStore: pgstoretest.Analysis(pool)}

	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, jobs.CMDetectReconcileArgs{}), "CM detection reconcile pass")

	if !locator.vanished {
		t.Fatal("the recording never reached the original lookup; the test does not exercise the path")
	}
	if got := len(recordedArgs[jobs.CMDetectJobArgs](t, enqueuer.CallsOfKind(jobs.CMDetectJobArgs{}.Kind()))); got != 0 {
		t.Errorf("detection jobs for a recording whose original vanished after listing = %d, want 0", got)
	}
}

// logo 候補の列挙に出た録画の原本が、置き場を引く前に消えたら logo 候補ジョブを積まない。
// 録画に cm_detect の方針を付けないので検出の候補にはならず、引き当てを呼ぶのは logo 候補だけになる。
func TestCMDetectReconcile_LogoCandidateOriginalGoneAfterListingIsNotEnqueued(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := dbtest.Recording(t, pool, cmRecordingOptions(15122))
	seedOriginalAsset(t, pool, t.TempDir(), id, "cm/logo-vanished.ts", []byte("ts"))
	seedTaughtArea(t, pool, 1180)
	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	locator := &vanishingLocator{OriginalLocator: pgstoretest.Media(pool), pool: pool, recordingID: id}
	w := &CMDetectReconcileWorker{Enqueuer: enqueuer, Locator: locator, DetectionStore: pgstoretest.Analysis(pool)}

	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, jobs.CMDetectReconcileArgs{}), "CM detection reconcile pass")

	if !locator.vanished {
		t.Fatal("the recording never reached the logo candidate lookup; the test does not exercise the path")
	}
	got := 0
	for _, args := range recordedArgs[jobs.CMLogoCandidateJobArgs](t, enqueuer.CallsOfKind(jobs.CMLogoCandidateJobArgs{}.Kind())) {
		if args.RecordingID == id {
			got++
		}
	}
	if got != 0 {
		t.Errorf("logo candidate jobs for a recording whose original vanished after listing = %d, want 0", got)
	}
}

func TestCMDetectReconcileFailedAttemptMovesPastFirstCandidate(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)

	ctx := context.Background()
	firstID := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(15101)))
	secondID := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(15102)))
	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	detectionStore := pgstoretest.Analysis(pool)

	firstWorker := &CMDetectReconcileWorker{Enqueuer: enqueuer, Locator: pgstoretest.Media(pool), DetectionStore: detectionStore, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, firstWorker, jobs.CMDetectReconcileArgs{}), "first reconcile pass")
	detectionJobs := recordedArgs[jobs.CMDetectJobArgs](t, enqueuer.CallsOfKind(jobs.CMDetectJobArgs{}.Kind()))
	if len(detectionJobs) != 1 || detectionJobs[0].RecordingID != firstID {
		t.Fatalf("detection jobs after first pass = %+v, want only recording %d", detectionJobs, firstID)
	}

	attempt := startCMDetectionTestAttemptRow(t, ctx, detectionStore, firstID)
	markCMDetectionTestFailure(t, ctx, pool, detectionStore, firstID, attempt.AttemptCount, "failed", nil, nil)

	// --once starts a new worker for the next pass. The failed attempt removes the
	// first recording from the view, so the next row is now at the head of the window.
	secondWorker := &CMDetectReconcileWorker{Enqueuer: enqueuer, Locator: pgstoretest.Media(pool), DetectionStore: detectionStore, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, secondWorker, jobs.CMDetectReconcileArgs{}), "second reconcile pass with a fresh worker")
	detectionJobs = recordedArgs[jobs.CMDetectJobArgs](t, enqueuer.CallsOfKind(jobs.CMDetectJobArgs{}.Kind()))
	if len(detectionJobs) != 2 || detectionJobs[1].RecordingID != secondID {
		t.Fatalf("detection jobs after second pass = %+v, want recording %d after failed recording %d", detectionJobs, secondID, firstID)
	}
	if got := len(enqueuer.CallsOfKind(jobs.CMDetectReconcileArgs{}.Kind())); got != 0 {
		t.Errorf("CM reconcile continuation jobs = %d, want 0", got)
	}
}

func TestCMDetectReconcileCandidateRowMovesPastFirstStation(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)

	ctx := context.Background()
	firstID := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(15111)))
	secondID := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(15112)))
	testutil.MustExec(t, pool, "moving second recording to another service", `UPDATE recordings SET service_id = 1025 WHERE id = $1`, secondID)
	firstAreaAt := seedTaughtAreaForService(t, pool, 1024, 1180)
	seedTaughtAreaForService(t, pool, 1025, 1170)
	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	detectionStore := pgstoretest.Analysis(pool)

	firstWorker := &CMDetectReconcileWorker{Enqueuer: enqueuer, Locator: pgstoretest.Media(pool), DetectionStore: detectionStore, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, firstWorker, jobs.CMDetectReconcileArgs{}), "first reconcile pass")
	candidateJobs := recordedArgs[jobs.CMLogoCandidateJobArgs](t, enqueuer.CallsOfKind(jobs.CMLogoCandidateJobArgs{}.Kind()))
	if len(candidateJobs) != 1 || candidateJobs[0].ServiceID != 1024 {
		t.Fatalf("logo-candidate jobs after first pass = %+v, want only service 1024", candidateJobs)
	}

	// A running row is enough to remove the station from cm_logo_candidate_desired.
	insertRunningCandidate(t, pool, 1024, firstID, firstAreaAt)

	secondWorker := &CMDetectReconcileWorker{Enqueuer: enqueuer, Locator: pgstoretest.Media(pool), DetectionStore: detectionStore, RowLimit: 1}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, secondWorker, jobs.CMDetectReconcileArgs{}), "second reconcile pass with a fresh worker")
	candidateJobs = recordedArgs[jobs.CMLogoCandidateJobArgs](t, enqueuer.CallsOfKind(jobs.CMLogoCandidateJobArgs{}.Kind()))
	if len(candidateJobs) != 2 || candidateJobs[1].ServiceID != 1025 || candidateJobs[1].RecordingID != secondID {
		t.Fatalf("logo-candidate jobs after second pass = %+v, want service 1025 recording %d after existing row for service 1024", candidateJobs, secondID)
	}
	if got := len(enqueuer.CallsOfKind(jobs.CMDetectReconcileArgs{}.Kind())); got != 0 {
		t.Errorf("CM reconcile continuation jobs = %d, want 0", got)
	}
}
