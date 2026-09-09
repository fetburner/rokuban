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
//
// mirakc スタブは対象 recordID を status='finished' の record として返す
// （production では回収の直後に同じ Work の中で wt.Sweep が走るため、
// 「回収が投入した available 行」と「Sweep が InsertTx しようとする行」が
// 必ず並ぶ。record 0 件のスタブ（旧テスト）ではこの並びが再現されず、
// 「回収した結果として同じ recording の ingest が二重に走らないこと」
// （issue #690）が測れていなかった）。
func TestRecordSweepRecovery_ReplacesStaleRunningIngest(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	recordingID := insertTestRecording(t, pool)
	const recordID = "rec-stale-running-ingest"
	insertTestRecordSync(t, pool, recordingID, recordID)
	oldJobID, attemptedAt := insertStaleRunningIngestJob(t, pool, recordID)
	setIngestProgressObservedAt(t, pool, recordingID, attemptedAt)

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

	startAt := mirakc.Milliseconds(time.Now().Add(-time.Hour))
	recStart := mirakc.Milliseconds(time.Now().Add(-time.Hour))
	endTime := mirakc.Milliseconds(time.Now())
	duration := int64(1800000)
	name := "record_sweep recovery テスト番組"
	record := mirakc.Record{
		ID: recordID,
		Program: mirakc.Program{
			ID: 700000600079999, EventID: 1, ServiceID: 1024, NetworkID: 32736,
			StartAt: &startAt, Duration: &duration, IsFree: true, Name: &name,
		},
		Service:   mirakc.Service{Name: "テスト局", Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"}},
		Tags:      []string{},
		Recording: mirakc.RecordInfo{Status: "finished", StartTime: recStart, EndTime: &endTime},
		Content:   mirakc.ContentInfo{Path: "test.m2ts"},
	}

	srv := newRecordSweepStub(t, []mirakc.Record{record})
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

	// 二重実行が起きていないこと: mirakc が同じ record_id を finished record として
	// 返し続けても（このテストのスタブ）、wt.Sweep の InsertTx は回収が投入した
	// available 行へ UniqueOpts で合流するだけで、実際に走りうる（discarded ではない）
	// ingest 行はちょうど 1 本のまま。
	assertNonDiscardedIngestJobCount(t, pool, testSite, recordID, 1)

	// 死んだ attempt が残した recording_ingest_progress 行は回収と同じ tx で
	// 消える。消し忘れると、代替 ingest が commit するまで API の進捗表示が
	// 古い値のまま止まって見える。
	if recordingIngestProgressExists(t, pool, recordingID) {
		t.Fatal("recording_ingest_progress row still exists after recovery, want it cleared in the same tx")
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
	oldJobID, _ := insertStaleRunningIngestJob(t, pool, recordID)
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

// TestRecordSweepRecovery_FreshRetryAfterStaleProgressIsNotStale は、失敗した
// attempt が古い recording_ingest_progress 行を残したまま River のバックオフで
// 再試行が始まり、attempted_at だけが新しくなった running 行を回収しないことを
// 固定する。DeleteRecordingIngestProgress（internal/worker/ingest.go）は成功した
// attempt（commit / already-committed 経路）でしか呼ばれないため、失敗して
// 再試行した attempt は古い進捗行を残したまま state='running', attempted_at=now()
// になる --- 「最後の活動」を観測時刻の COALESCE（進捗が無ければ attempted_at）で
// 決めると、新しい attempted_at より古い observed_at が優先されてしまい、
// 再試行してまだ acquireIngestJobLock にすら到達していない生きたジョブを
// 即座に候補にしてしまう。
//
// 壊し方: listStaleIngestJobsQuery の GREATEST(p.observed_at, j.attempted_at) を
// COALESCE(p.observed_at, j.attempted_at) に戻すと、新しい attempted_at を無視して
// 古い observed_at を「最後の活動」として採用し、生きたばかりの running 行を
// discarded にしてこのテストが落ちる。
func TestRecordSweepRecovery_FreshRetryAfterStaleProgressIsNotStale(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	recordingID := insertTestRecording(t, pool)
	const recordID = "rec-fresh-retry-stale-progress"
	insertTestRecordSync(t, pool, recordingID, recordID)
	oldJobID, _ := insertStaleRunningIngestJob(t, pool, recordID)
	setIngestProgressObservedAt(t, pool, recordingID, time.Now().UTC().Add(-2*ingestRecoveryStaleAfter))

	freshAttemptedAt := time.Now().UTC()
	if _, err := pool.Exec(ctx, "UPDATE river_job SET attempted_at = $2 WHERE id = $1", oldJobID, freshAttemptedAt); err != nil {
		t.Fatalf("refreshing attempted_at to simulate a live retry: %v", err)
	}

	w := newEmptyRecordSweepWorker(t, pool)
	job := &river.Job[jobs.RecordSweepArgs]{
		JobRow: &rivertype.JobRow{ID: 904},
		Args:   jobs.RecordSweepArgs{Site: testSite},
	}
	if err := w.Work(riverWorkContext(t, pool), job); err != nil {
		t.Fatalf("RecordSweepWorker.Work: %v", err)
	}

	state, finalizedAt := ingestJobStateAndFinalizedAt(t, pool, oldJobID)
	if state != string(rivertype.JobStateRunning) || finalizedAt != nil {
		t.Fatalf("freshly-retried ingest with stale progress = state %q finalized_at=%v, want running/NULL", state, finalizedAt)
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
	oldJobID, attemptedAt := insertStaleRunningIngestJob(t, pool, recordID)
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

// insertStaleRunningIngestJob は常に testSite の下でフィクスチャを作る
// （呼び出し側は全てそうしている。golangci-lint の unparam 参照）。
func insertStaleRunningIngestJob(t *testing.T, pool *pgxpool.Pool, recordID string) (int64, time.Time) {
	t.Helper()
	ctx := context.Background()
	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyClient: %v", err)
	}
	result, err := client.Insert(ctx, jobs.IngestJobArgs{Site: testSite, RecordID: recordID}, nil)
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

// assertNonDiscardedIngestJobCount は assertIngestJobCount と異なり discarded を
// 除外して数える。回収が旧行を discarded にした直後は assertIngestJobCount では
// 「旧 1 本 + 代替 1 本 = 2 本」になり得るため、二重実行（実際に走りうる ingest
// 行が 2 本になっていないこと）を見るには discarded を除いた母数が要る。
func assertNonDiscardedIngestJobCount(t *testing.T, pool *pgxpool.Pool, site, recordID string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM river_job
		WHERE kind = 'ingest' AND args->>'site' = $1 AND args->>'record_id' = $2 AND state <> 'discarded'`, site, recordID,
	).Scan(&got); err != nil {
		t.Fatalf("counting non-discarded ingest jobs: %v", err)
	}
	if got != want {
		t.Fatalf("non-discarded ingest job count = %d, want %d", got, want)
	}
}

// recordingIngestProgressExists は recording_ingest_progress にその recording の
// 行が残っているかを返す。回収が同じ tx で進捗行を消し忘れていないかを見る。
func recordingIngestProgressExists(t *testing.T, pool *pgxpool.Pool, recordingID int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM recording_ingest_progress WHERE recording_id = $1)", recordingID,
	).Scan(&exists); err != nil {
		t.Fatalf("checking recording_ingest_progress existence: %v", err)
	}
	return exists
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
