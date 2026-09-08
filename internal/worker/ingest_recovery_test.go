package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestRecordSweepRecovery_ReplacesStaleRunningIngest はプロセス死を running 行だけ
// が残った状態で再現する。回収前は record_sweep 相当の同じ ingest 投入がその行へ
// 合流し、未 ingest backlog も減らない。回収後は古い行を終端化して別 ID の試行を
// 作るため、次の record_sweep で再び拾える状態になる。
func TestRecordSweepRecovery_ReplacesStaleRunningIngest(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	recordingID := insertTestRecording(t, pool)
	const recordID = "rec-stale-running-ingest"
	insertTestRecordSync(t, pool, recordingID, recordID)
	oldJobID, attemptedAt := insertStaleRunningIngestJob(t, pool, testSite, recordID)

	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	duplicate, err := client.Insert(ctx, jobs.IngestJobArgs{Site: testSite, RecordID: recordID}, nil)
	if err != nil {
		t.Fatalf("inserting duplicate ingest before recovery: %v", err)
	}
	if !duplicate.UniqueSkippedAsDuplicate {
		t.Fatal("stale running ingest was not the unique duplicate before recovery")
	}

	state, finalizedAt := ingestJobStateAndFinalizedAt(t, pool, oldJobID)
	if state != string(rivertype.JobStateRunning) {
		t.Fatalf("stale ingest state before recovery = %q, want running", state)
	}
	if finalizedAt != nil {
		t.Fatal("stale ingest finalized_at was set before recovery")
	}
	var gotAttemptedAt time.Time
	if err := pool.QueryRow(ctx, "SELECT attempted_at FROM river_job WHERE id = $1", oldJobID).Scan(&gotAttemptedAt); err != nil {
		t.Fatalf("reading stale ingest attempted_at: %v", err)
	}
	if !gotAttemptedAt.Equal(attemptedAt) {
		t.Fatalf("stale ingest attempted_at = %v, want %v", gotAttemptedAt, attemptedAt)
	}
	if got := unIngestedBacklogCount(t, pool, testSite); got != 1 {
		t.Fatalf("un-ingested backlog before recovery = %d, want 1", got)
	}

	srv := newRecordSweepStub(t, nil)
	defer srv.Close()
	w := &RecordSweepWorker{
		MirakcClients: singleSiteClients(testSite, mirakc.NewClient(srv.URL, nil)),
		Pool:          pool,
	}
	job := &river.Job[jobs.RecordSweepArgs]{
		JobRow: &rivertype.JobRow{ID: 901},
		Args:   jobs.RecordSweepArgs{Site: testSite},
	}
	if err := w.Work(riverWorkContext(t, pool), job); err != nil {
		t.Fatalf("RecordSweepWorker.Work: %v", err)
	}

	var replacementID int64
	if err := pool.QueryRow(ctx, `
		SELECT id
		FROM river_job
		WHERE kind = 'ingest' AND args->>'site' = $1 AND args->>'record_id' = $2 AND state <> 'discarded'
		ORDER BY id DESC
		LIMIT 1`, testSite, recordID).Scan(&replacementID); err != nil {
		t.Fatalf("finding replacement ingest job: %v", err)
	}
	if replacementID == oldJobID {
		t.Fatalf("replacement ingest reused old job ID %d", oldJobID)
	}

	state, finalizedAt = ingestJobStateAndFinalizedAt(t, pool, oldJobID)
	if state != string(rivertype.JobStateDiscarded) {
		t.Fatalf("old ingest state after recovery = %q, want discarded", state)
	}
	if finalizedAt == nil {
		t.Fatal("old ingest finalized_at is NULL after recovery")
	}
	var errorsText, metadataText string
	if err := pool.QueryRow(ctx,
		"SELECT errors::text, metadata::text FROM river_job WHERE id = $1", oldJobID,
	).Scan(&errorsText, &metadataText); err != nil {
		t.Fatalf("reading old ingest recovery details: %v", err)
	}
	if !strings.Contains(errorsText, ingestRecoveryReason) {
		t.Fatalf("old ingest errors = %q, want recovery reason %q", errorsText, ingestRecoveryReason)
	}
	if !strings.Contains(metadataText, ingestRecoveryReason) {
		t.Fatalf("old ingest metadata = %q, want recovery reason %q", metadataText, ingestRecoveryReason)
	}

	var replacementState string
	var replacementAttemptedAt, replacementFinalizedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT state, attempted_at, finalized_at
		FROM river_job WHERE id = $1`, replacementID,
	).Scan(&replacementState, &replacementAttemptedAt, &replacementFinalizedAt); err != nil {
		t.Fatalf("reading replacement ingest state: %v", err)
	}
	if replacementState != string(rivertype.JobStateAvailable) {
		t.Fatalf("replacement ingest state = %q, want available", replacementState)
	}
	if replacementAttemptedAt != nil || replacementFinalizedAt != nil {
		t.Fatalf("replacement ingest timestamps = attempted_at=%v finalized_at=%v, want both NULL", replacementAttemptedAt, replacementFinalizedAt)
	}
	if got := unIngestedBacklogCount(t, pool, testSite); got != 1 {
		t.Fatalf("un-ingested backlog after recovery = %d, want 1 until replacement ingest runs", got)
	}
}

// TestRecordSweepRecovery_RecentProgressIsNotStale は attempted_at だけが古くても、
// 転送中に更新される recording_ingest_progress が新しければ回収しないことを固定する。
// 候補を attempted_at のみで選ぶ変異は、このテストで running 行を discarded にして落ちる。
func TestRecordSweepRecovery_RecentProgressIsNotStale(t *testing.T) {
	pool := testutil.SetupDB(t)

	recordingID := insertTestRecording(t, pool)
	const recordID = "rec-recent-ingest-progress"
	insertTestRecordSync(t, pool, recordingID, recordID)
	oldJobID, _ := insertStaleRunningIngestJob(t, pool, testSite, recordID)
	setIngestProgressObservedAt(t, pool, recordingID, time.Now().UTC())

	w := newEmptyRecordSweepWorker(t, pool)
	job := &river.Job[jobs.RecordSweepArgs]{
		JobRow: &rivertype.JobRow{ID: 902},
		Args:   jobs.RecordSweepArgs{Site: testSite},
	}
	if err := w.Work(riverWorkContext(t, pool), job); err != nil {
		t.Fatalf("RecordSweepWorker.Work: %v", err)
	}

	state, finalizedAt := ingestJobStateAndFinalizedAt(t, pool, oldJobID)
	if state != string(rivertype.JobStateRunning) || finalizedAt != nil {
		t.Fatalf("running ingest with recent progress = state %q finalized_at=%v, want running/NULL", state, finalizedAt)
	}
	assertIngestJobCount(t, pool, testSite, recordID, 1)
}

// TestRecordSweepRecovery_DoesNotTakeLiveJobWhenProgressIsStale は進捗時刻が古くても、
// 生きている ingest が保持するジョブ advisory lock を奪わないことを固定する。これは
// HEAD / fsync / commit 中など、最後の DB 進捗が一時的に古く見える live transfer を
// 時刻だけで殺さないための回帰テストである。
func TestRecordSweepRecovery_DoesNotTakeLiveJobWhenProgressIsStale(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	recordingID := insertTestRecording(t, pool)
	const recordID = "rec-live-ingest-lock"
	insertTestRecordSync(t, pool, recordingID, recordID)
	oldJobID, attemptedAt := insertStaleRunningIngestJob(t, pool, testSite, recordID)
	setIngestProgressObservedAt(t, pool, recordingID, attemptedAt)

	lock, acquired, err := acquireIngestJobLock(ctx, pool, oldJobID, time.Second)
	if err != nil {
		t.Fatalf("acquiring live ingest job lock: %v", err)
	}
	if !acquired {
		t.Fatal("live ingest job lock was not acquired")
	}
	t.Cleanup(lock.release)

	w := newEmptyRecordSweepWorker(t, pool)
	job := &river.Job[jobs.RecordSweepArgs]{
		JobRow: &rivertype.JobRow{ID: 903},
		Args:   jobs.RecordSweepArgs{Site: testSite},
	}
	if err := w.Work(riverWorkContext(t, pool), job); err != nil {
		t.Fatalf("RecordSweepWorker.Work: %v", err)
	}

	state, finalizedAt := ingestJobStateAndFinalizedAt(t, pool, oldJobID)
	if state != string(rivertype.JobStateRunning) || finalizedAt != nil {
		t.Fatalf("live ingest with stale progress = state %q finalized_at=%v, want running/NULL", state, finalizedAt)
	}
	assertIngestJobCount(t, pool, testSite, recordID, 1)
}

func insertStaleRunningIngestJob(t *testing.T, pool *pgxpool.Pool, site, recordID string) (int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	result, err := client.Insert(ctx, jobs.IngestJobArgs{Site: site, RecordID: recordID}, nil)
	if err != nil {
		t.Fatalf("inserting ingest fixture: %v", err)
	}
	attemptedAt := time.Now().UTC().Add(-2 * ingestRecoveryStaleAfter).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		UPDATE river_job
		SET state = 'running', attempt = 1, attempted_at = $2, attempted_by = ARRAY['dead-process']::text[]
		WHERE id = $1`, result.Job.ID, attemptedAt); err != nil {
		t.Fatalf("making ingest fixture running: %v", err)
	}
	return result.Job.ID, attemptedAt
}

func setIngestProgressObservedAt(t *testing.T, pool *pgxpool.Pool, recordingID int64, observedAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO recording_ingest_progress (recording_id, written_bytes, expected_bytes, observed_at)
		VALUES ($1, 1, 100, $2)
		ON CONFLICT (recording_id) DO UPDATE
		SET written_bytes = EXCLUDED.written_bytes,
		    expected_bytes = EXCLUDED.expected_bytes,
		    observed_at = EXCLUDED.observed_at`, recordingID, observedAt); err != nil {
		t.Fatalf("setting ingest progress observed_at: %v", err)
	}
}

func newEmptyRecordSweepWorker(t *testing.T, pool *pgxpool.Pool) *RecordSweepWorker {
	t.Helper()
	srv := newRecordSweepStub(t, nil)
	t.Cleanup(srv.Close)
	return &RecordSweepWorker{
		MirakcClients: singleSiteClients(testSite, mirakc.NewClient(srv.URL, nil)),
		Pool:          pool,
	}
}

func ingestJobStateAndFinalizedAt(t *testing.T, pool *pgxpool.Pool, jobID int64) (string, *time.Time) {
	t.Helper()
	var state string
	var finalizedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		"SELECT state, finalized_at FROM river_job WHERE id = $1", jobID,
	).Scan(&state, &finalizedAt); err != nil {
		t.Fatalf("reading ingest job state: %v", err)
	}
	return state, finalizedAt
}

func assertIngestJobCount(t *testing.T, pool *pgxpool.Pool, site, recordID string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM river_job
		WHERE kind = 'ingest' AND args->>'site' = $1 AND args->>'record_id' = $2`, site, recordID,
	).Scan(&got); err != nil {
		t.Fatalf("counting ingest jobs: %v", err)
	}
	if got != want {
		t.Fatalf("ingest job count = %d, want %d", got, want)
	}
}

func unIngestedBacklogCount(t *testing.T, pool *pgxpool.Pool, site string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM record_sync AS rs
		WHERE rs.site = $1
		  AND rs.status = 'finished'
		  AND NOT EXISTS (
			  SELECT 1 FROM media_assets AS a
			  WHERE a.recording_id = rs.recording_id
			    AND a.kind = 'original'
			    AND a.state <> 'deleted'
		  )`, site).Scan(&count); err != nil {
		t.Fatalf("counting un-ingested backlog: %v", err)
	}
	return count
}
