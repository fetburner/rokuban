package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store/pgstore"
)

// TestCMDetectReconcile_ContinuationSurvivesFreshWorker mirrors --once: both
// the recording window and station-keyed logo-candidate window continue after
// a new worker instance starts.
func TestCMDetectReconcile_ContinuationSurvivesFreshWorker(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	firstID := seedCMRecording(t, pool, t.TempDir(), 15101)
	secondID := seedCMRecording(t, pool, t.TempDir(), 15102)
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET service_id = 1025 WHERE id = $1`, secondID); err != nil {
		t.Fatalf("moving second recording to another service: %v", err)
	}
	seedTaughtAreaForService(t, pool, 1024, 1180)
	seedTaughtAreaForService(t, pool, 1025, 1170)

	enqueuer := newRecordingEnqueuer()
	store := pgstore.NewAnalysis(pool)
	firstWorker := &CMDetectReconcileWorker{Enqueuer: enqueuer, DetectionStore: store, RowLimit: 1}
	if err := runWorkerViaJobQFake(context.Background(), firstWorker, jobs.CMDetectReconcileArgs{}); err != nil {
		t.Fatalf("first reconcile page: %v", err)
	}

	detectionJobs := enqueuedArgs[jobs.CMDetectJobArgs](enqueuer, jobs.CMDetectJobArgs{}.Kind())
	if len(detectionJobs) != 1 || detectionJobs[0].RecordingID != firstID {
		t.Fatalf("detection jobs after first page = %+v, want only recording %d", detectionJobs, firstID)
	}
	candidateJobs := enqueuedArgs[jobs.CMLogoCandidateJobArgs](enqueuer, jobs.CMLogoCandidateJobArgs{}.Kind())
	if len(candidateJobs) != 1 || candidateJobs[0].ServiceID != 1024 {
		t.Fatalf("logo-candidate jobs after first page = %+v, want only service 1024", candidateJobs)
	}
	continuations := enqueuedArgs[jobs.CMDetectReconcileArgs](enqueuer, jobs.CMDetectReconcileArgs{}.Kind())
	if len(continuations) != 1 || continuations[0].AfterRecordingID != firstID ||
		continuations[0].CandidateAfterNetworkID != 32736 || continuations[0].CandidateAfterServiceID != 1024 {
		t.Fatalf("first continuation = %+v, want recording cursor %d and station cursor (32736, 1024)", continuations, firstID)
	}

	secondWorker := &CMDetectReconcileWorker{Enqueuer: enqueuer, DetectionStore: store, RowLimit: 1}
	if err := runWorkerViaJobQFake(context.Background(), secondWorker, continuations[0]); err != nil {
		t.Fatalf("second reconcile page with a fresh worker: %v", err)
	}
	detectionJobs = enqueuedArgs[jobs.CMDetectJobArgs](enqueuer, jobs.CMDetectJobArgs{}.Kind())
	if len(detectionJobs) != 2 || detectionJobs[1].RecordingID != secondID {
		t.Errorf("detection jobs after fresh worker = %+v, want second recording %d on page two", detectionJobs, secondID)
	}
	candidateJobs = enqueuedArgs[jobs.CMLogoCandidateJobArgs](enqueuer, jobs.CMLogoCandidateJobArgs{}.Kind())
	if len(candidateJobs) != 2 || candidateJobs[1].ServiceID != 1025 {
		t.Errorf("logo-candidate jobs after fresh worker = %+v, want service 1025 on page two", candidateJobs)
	}
	continuations = enqueuedArgs[jobs.CMDetectReconcileArgs](enqueuer, jobs.CMDetectReconcileArgs{}.Kind())
	if len(continuations) != 2 || continuations[1].AfterRecordingID != secondID ||
		continuations[1].CandidateAfterNetworkID != 32736 || continuations[1].CandidateAfterServiceID != 1025 {
		t.Fatalf("second continuation = %+v, want recording cursor %d and station cursor (32736, 1025)", continuations, secondID)
	}
	thirdWorker := &CMDetectReconcileWorker{Enqueuer: enqueuer, DetectionStore: store, RowLimit: 1}
	if err := runWorkerViaJobQFake(context.Background(), thirdWorker, continuations[1]); err != nil {
		t.Fatalf("page after final candidates: %v", err)
	}
	if got := len(enqueuedArgs[jobs.CMDetectReconcileArgs](enqueuer, jobs.CMDetectReconcileArgs{}.Kind())); got != 2 {
		t.Errorf("continuation jobs after both windows reached the end = %d, want 2", got)
	}
}

func TestCMDetectReconcile_ExhaustedWindowNotRestarted(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	mediaDir := t.TempDir()
	firstID := seedCMRecording(t, pool, mediaDir, 15111)
	secondID := seedCMRecording(t, pool, mediaDir, 15112)
	seedTaughtAreaForService(t, pool, 1024, 1180)
	enqueuer := newRecordingEnqueuer()
	detectionStore := pgstore.NewAnalysis(pool)

	continuationIndex := 0
	for continuationIndex == 0 || continuationIndex <= len(enqueuedArgs[jobs.CMDetectReconcileArgs](enqueuer, jobs.CMDetectReconcileArgs{}.Kind())) {
		continuations := enqueuedArgs[jobs.CMDetectReconcileArgs](enqueuer, jobs.CMDetectReconcileArgs{}.Kind())
		args := jobs.CMDetectReconcileArgs{}
		if continuationIndex > 0 {
			args = continuations[continuationIndex-1]
		}
		worker := &CMDetectReconcileWorker{Enqueuer: enqueuer, DetectionStore: detectionStore, RowLimit: 1}
		if err := runWorkerViaJobQFake(context.Background(), worker, args); err != nil {
			t.Fatalf("reconcile page %d: %v", continuationIndex+1, err)
		}
		continuationIndex++
	}

	detectionJobs := enqueuedArgs[jobs.CMDetectJobArgs](enqueuer, jobs.CMDetectJobArgs{}.Kind())
	if len(detectionJobs) != 2 || detectionJobs[0].RecordingID != firstID || detectionJobs[1].RecordingID != secondID {
		t.Errorf("detection jobs = %+v, want recordings %d and %d once each", detectionJobs, firstID, secondID)
	}
	candidateJobs := enqueuedArgs[jobs.CMLogoCandidateJobArgs](enqueuer, jobs.CMLogoCandidateJobArgs{}.Kind())
	if len(candidateJobs) != 1 || candidateJobs[0].ServiceID != 1024 {
		t.Errorf("logo-candidate jobs = %+v, want service 1024 once", candidateJobs)
	}
	continuations := enqueuedArgs[jobs.CMDetectReconcileArgs](enqueuer, jobs.CMDetectReconcileArgs{}.Kind())
	if len(continuations) != 2 {
		t.Fatalf("continuation count = %d, want 2 for unequal windows", len(continuations))
	}
	if continuations[0].CandidateDone || continuations[0].DetectionDone {
		t.Errorf("first continuation done flags = detection:%v candidate:%v, want false:false",
			continuations[0].DetectionDone, continuations[0].CandidateDone)
	}
	if continuations[1].DetectionDone || !continuations[1].CandidateDone {
		t.Errorf("second continuation done flags = detection:%v candidate:%v, want false:true",
			continuations[1].DetectionDone, continuations[1].CandidateDone)
	}
}

type cmDetectContinuationFailingEnqueuer struct {
	*recordingEnqueuer
	continuationAttempts int
}

func (e *cmDetectContinuationFailingEnqueuer) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	if args.Kind() == (jobs.CMDetectReconcileArgs{}).Kind() {
		e.continuationAttempts++
		return jobq.Result{}, errors.New("continuation unavailable")
	}
	return e.recordingEnqueuer.Enqueue(ctx, args, opts...)
}

func TestCMDetectReconcile_ContinuationEnqueueFailureReturnsSuccess(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}

	seedCMRecording(t, pool, t.TempDir(), 15103)
	seedCMRecording(t, pool, t.TempDir(), 15104)
	enqueuer := &cmDetectContinuationFailingEnqueuer{recordingEnqueuer: newRecordingEnqueuer()}
	w := &CMDetectReconcileWorker{Enqueuer: enqueuer, DetectionStore: pgstore.NewAnalysis(pool), RowLimit: 1}
	if err := runWorkerViaJobQFake(context.Background(), w, jobs.CMDetectReconcileArgs{}); err != nil {
		t.Fatalf("Work after continuation enqueue failure = %v, want nil", err)
	}
	if got := len(enqueuedArgs[jobs.CMDetectJobArgs](enqueuer.recordingEnqueuer, jobs.CMDetectJobArgs{}.Kind())); got != 1 {
		t.Errorf("detection jobs after continuation enqueue failure = %d, want 1", got)
	}
	if enqueuer.continuationAttempts != 1 {
		t.Errorf("continuation enqueue attempts = %d, want 1", enqueuer.continuationAttempts)
	}
}
