package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestEncodeRecovery_ReplacesStaleRunningJobAndEncodeCompletes はプロセス死を
// running 行だけが残った状態で再現する。回収前は同じ encode 投入が旧行へ合流するが、
// 回収後は旧行を discarded に終端化して別 ID の試行を作り、その試行が実際に
// EncodeWorker で完了することまで確認する。
func TestEncodeRecovery_ReplacesStaleRunningJobAndEncodeCompletes(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "recovery/stale.m2ts", []string{"h264"}, []byte("payload"))
	oldJobID := insertStaleRunningEncodeJob(t, pool, recordingID, "h264")

	if _, err := pool.Exec(ctx, `
		INSERT INTO recording_encode_attempts (recording_id, profile, state)
		VALUES ($1, $2, 'running')`, recordingID, "h264"); err != nil {
		t.Fatalf("inserting running encode attempt: %v", err)
	}

	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	// The stale running row is the unique pending job, so this insert must be
	// skipped. Keeping this assertion makes the precondition explicit.
	duplicate, err := client.Insert(ctx, jobs.EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}, nil)
	if err != nil {
		t.Fatalf("inserting duplicate encode job: %v", err)
	}
	if !duplicate.UniqueSkippedAsDuplicate {
		t.Fatal("stale running encode was not the unique duplicate before recovery")
	}

	runEncodeReconcilePass(t, pool, &EncodeReconcileWorker{Pool: pool, Profiles: encodeConfigWith("h264")})

	oldJob := testutil.MustGetRiverJob(t, ctx, testutil.NewRiverClient(t, pool), oldJobID)
	oldErrors, oldMetadata := fmt.Sprintf("%+v", oldJob.Errors), string(oldJob.Metadata)
	if oldJob.State != rivertype.JobStateDiscarded {
		t.Fatalf("old encode state = %q, want discarded", oldJob.State)
	}
	if oldJob.FinalizedAt == nil {
		t.Fatal("old encode finalized_at is NULL after recovery")
	}
	if !strings.Contains(oldErrors, encodeRecoveryReason) {
		t.Fatalf("old encode errors = %q, want recovery reason %q", oldErrors, encodeRecoveryReason)
	}
	if !strings.Contains(oldMetadata, encodeRecoveryReason) {
		t.Fatalf("old encode metadata = %q, want recovery reason %q", oldMetadata, encodeRecoveryReason)
	}

	var replacementID int64
	for _, candidate := range testutil.MustListRiverJobsOfKind(t, ctx, pool, (jobs.EncodeJobArgs{}).Kind()) {
		args := testutil.MustDecodeRiverJobArgs[jobs.EncodeJobArgs](t, candidate)
		if args.RecordingID == recordingID && args.Profile == "h264" && candidate.State != rivertype.JobStateDiscarded && candidate.ID > replacementID {
			replacementID = candidate.ID
		}
	}
	if replacementID == 0 {
		t.Fatal("replacement encode job not found")
	}
	if replacementID == oldJobID {
		t.Fatalf("replacement encode reused old job ID %d", oldJobID)
	}

	replacement := testutil.MustGetRiverJob(t, ctx, testutil.NewRiverClient(t, pool), replacementID)
	if replacement.State != rivertype.JobStateAvailable {
		t.Fatalf("replacement encode state = %q, want available", replacement.State)
	}

	if state, ok := encodeAttemptState(t, pool, recordingID, "h264"); !ok || state != "running" {
		t.Fatalf("recording_encode_attempts after recovery = %q, exists=%v; recovery must not touch it", state, ok)
	}

	ffmpegPath := installFakeFFmpeg(t)
	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: t.TempDir(),
		FFmpeg:     ffmpegPath,
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{{
			Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		}}},
	}
	if err := w.Work(ctx, &river.Job[jobs.EncodeJobArgs]{
		JobRow: &rivertype.JobRow{ID: replacementID, Attempt: 1, MaxAttempts: 25},
		Args:   jobs.EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
	}); err != nil {
		t.Fatalf("replacement EncodeWorker.Work: %v", err)
	}

	var encodedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM media_assets
		WHERE recording_id = $1 AND kind = 'encoded' AND profile = $2 AND state = 'active'`, recordingID, "h264").Scan(&encodedCount); err != nil {
		t.Fatalf("counting encoded assets: %v", err)
	}
	if encodedCount != 1 {
		t.Fatalf("active encoded assets = %d, want 1", encodedCount)
	}
	if _, ok := encodeAttemptState(t, pool, recordingID, "h264"); ok {
		t.Fatal("recording_encode_attempts row remains after replacement encode completed")
	}
}

// TestEncodeRecovery_DoesNotDiscardLiveStaleJob は attempted_at が古くても、実行中の
// encode が保持する job-id advisory lock を奪わないことを固定する。時刻だけで
// running 行を discarded にする変異はこのテストで落ちる。
func TestEncodeRecovery_DoesNotDiscardLiveStaleJob(t *testing.T) {
	pool := setupTestPool(t)
	ctx := context.Background()
	recordingID := seedRecordingWithOriginal(t, pool, t.TempDir(), "recovery/live.m2ts", []string{"h264"}, []byte("payload"))
	oldJobID := insertStaleRunningEncodeJob(t, pool, recordingID, "h264")

	lock, acquired, err := acquireEncodeJobLock(ctx, pool, oldJobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring live encode job lock: %v", err)
	}
	if !acquired {
		t.Fatal("live encode job lock was not acquired")
	}
	t.Cleanup(lock.release)

	runEncodeReconcilePass(t, pool, &EncodeReconcileWorker{Pool: pool, Profiles: encodeConfigWith("h264")})

	job := testutil.MustGetRiverJob(t, ctx, testutil.NewRiverClient(t, pool), oldJobID)
	if job.State != rivertype.JobStateRunning || job.FinalizedAt != nil {
		t.Fatalf("live stale encode = state %q finalized_at=%v, want running/NULL", job.State, job.FinalizedAt)
	}
	assertNonDiscardedEncodeJobCount(t, pool, recordingID, "h264", 1)
}

// TestEncodeRecoveryFailure_DoesNotStopGapFill は回収が失敗しても、encode_reconcile
// の本来の gap-fill が続くことを固定する。
func TestEncodeRecoveryFailure_DoesNotStopGapFill(t *testing.T) {
	pool := setupTestPool(t)
	recordingID := seedRecordingWithOriginal(t, pool, t.TempDir(), "recovery/failure.m2ts", []string{"h264"}, []byte("payload"))

	original := recoverStaleEncodeJobsFunc
	recoverStaleEncodeJobsFunc = func(context.Context, *pgxpool.Pool, *river.Client[pgx.Tx]) error {
		return errors.New("forced encode recovery failure (test)")
	}
	t.Cleanup(func() { recoverStaleEncodeJobsFunc = original })

	runEncodeReconcilePass(t, pool, &EncodeReconcileWorker{Pool: pool, Profiles: encodeConfigWith("h264")})
	if got := countEncodeJobs(t, pool, recordingID, "h264"); got != 1 {
		t.Fatalf("encode jobs after recovery failure = %d, want 1", got)
	}
}

// TestEncodeWorker_HoldsJobLock は EncodeWorker 自身が Work 全体の間 lock を保持し、
// 終了時に解放することを固定する。recovery 側だけが lock を使っていて Work が
// 取得しない変異は、live な encode を回収して二重実行を許すため、このテストで落ちる。
func TestEncodeWorker_HoldsJobLock(t *testing.T) {
	pool := setupTestPool(t)
	pool2, err := pgxpool.NewWithConfig(context.Background(), pool.Config())
	if err != nil {
		t.Fatalf("creating second pool: %v", err)
	}
	t.Cleanup(pool2.Close)

	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "recovery/held.m2ts", []string{"h264"}, []byte("payload"))
	setShortWorkerExecWaitDelay(t)
	slowFFmpegSleepSeconds := int(workerExecWaitDelay/time.Second) + 5
	slowFFmpeg, ffmpegStarted, childPIDMarker := installSlowFakeFFmpeg(t, slowFFmpegSleepSeconds)
	sleepStartedAt := time.Now()
	t.Cleanup(func() {
		if time.Since(sleepStartedAt) > time.Duration(slowFFmpegSleepSeconds)*time.Second {
			return
		}
		pidBytes, readErr := os.ReadFile(childPIDMarker)
		if readErr != nil {
			return
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if parseErr != nil {
			return
		}
		if process, findErr := os.FindProcess(pid); findErr == nil {
			_ = process.Kill()
		}
	})

	const jobID int64 = 797001
	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: t.TempDir(),
		FFmpeg:     slowFFmpeg,
		FFprobe:    "/nonexistent/ffprobe",
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{{
			Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		}}},
	}
	workCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workDone := make(chan error, 1)
	go func() {
		workDone <- w.Work(workCtx, &river.Job[jobs.EncodeJobArgs]{
			JobRow: &rivertype.JobRow{ID: jobID, Attempt: 1, MaxAttempts: 25},
			Args:   jobs.EncodeJobArgs{RecordingID: recordingID, Profile: "h264"},
		})
	}()

	waitFor(t, 5*time.Second,
		func() bool {
			_, err := os.Stat(ffmpegStarted)
			return err == nil
		},
		func() string {
			select {
			case err := <-workDone:
				return fmt.Sprintf("fake ffmpeg did not start; EncodeWorker.Work returned: %v", err)
			default:
				return "timed out waiting for fake ffmpeg to start"
			}
		},
	)

	lock, acquired, err := acquireEncodeJobLock(context.Background(), pool2, jobID, time.Second)
	if err != nil {
		t.Fatalf("trying to acquire encode job lock while Work is running: %v", err)
	}
	if lock != nil {
		t.Cleanup(lock.release)
	}
	if acquired {
		t.Fatal("second encode worker acquired the lock while the first Work was running")
	}

	cancel()
	select {
	case <-workDone:
	case <-time.After(2 * workerExecWaitDelay):
		t.Fatal("EncodeWorker.Work did not return after cancellation")
	}

	released, acquired, err := acquireEncodeJobLock(context.Background(), pool2, jobID, time.Second)
	if err != nil {
		t.Fatalf("reacquiring encode job lock after Work returned: %v", err)
	}
	if !acquired {
		t.Fatal("encode job lock remained held after Work returned")
	}
	released.release()
}

func insertStaleRunningEncodeJob(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string) int64 {
	t.Helper()
	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	result, err := client.Insert(context.Background(), jobs.EncodeJobArgs{RecordingID: recordingID, Profile: profile}, nil)
	if err != nil {
		t.Fatalf("inserting encode fixture: %v", err)
	}
	if result.UniqueSkippedAsDuplicate || result.Job == nil {
		t.Fatalf("inserting encode fixture was unexpectedly skipped: %+v", result)
	}
	attemptedAt := time.Now().UTC().Add(-2 * encodeRecoveryStaleAfter).Truncate(time.Microsecond)
	if _, err := pool.Exec(context.Background(), `
		UPDATE river_job
		SET state = 'running', attempt = 1, attempted_at = $2, attempted_by = ARRAY['dead-process']::text[]
		WHERE id = $1`, result.Job.ID, attemptedAt); err != nil {
		t.Fatalf("making encode fixture running: %v", err)
	}
	return result.Job.ID
}

func assertNonDiscardedEncodeJobCount(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile string, want int) {
	t.Helper()
	got := 0
	for _, row := range testutil.MustListRiverJobsOfKind(t, context.Background(), pool, (jobs.EncodeJobArgs{}).Kind()) {
		args := testutil.MustDecodeRiverJobArgs[jobs.EncodeJobArgs](t, row)
		if args.RecordingID == recordingID && args.Profile == profile && row.State != rivertype.JobStateDiscarded {
			got++
		}
	}
	if got != want {
		t.Fatalf("non-discarded encode jobs = %d, want %d", got, want)
	}
}
