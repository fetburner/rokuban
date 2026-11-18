package cmdetect

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

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

	q := sqlcgen.New(pool)
	attempt := startCMDetectionTestAttemptRow(t, ctx, q, firstID)
	markCMDetectionTestFailure(t, ctx, pool, q, firstID, attempt.AttemptCount, "failed", nil, nil)

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
	insertRunningCandidate(t, pool, firstID, firstAreaAt)

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
