package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

func newCandidateTestWorker(pool *pgxpool.Pool, mediaDir string, tools cmToolset) *CMLogoCandidateWorker {
	return &CMLogoCandidateWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: filepath.Join(mediaDir, "scratch"),
		CMDetect: config.CMDetectConfig{Enabled: true, BinaryDir: tools.binDir}, FFprobe: tools.ffprobe,
	}
}

// seedTaughtArea は局 32736/1024 に枠を置き、その updated_at を返す。
func seedTaughtArea(t *testing.T, pool *pgxpool.Pool, x int) time.Time {
	t.Helper()
	q := sqlcgen.New(pool)
	if err := q.UpsertCMLogoArea(context.Background(), sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: 32736, ServiceID: 1024, X: int32(x), Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	area, err := q.GetCMLogoArea(context.Background(), sqlcgen.GetCMLogoAreaParams{NetworkID: 32736, ServiceID: 1024})
	if err != nil {
		t.Fatal(err)
	}
	return area.UpdatedAt
}

func candidateRow(t *testing.T, pool *pgxpool.Pool) (state, stage string, ok bool) {
	t.Helper()
	c, err := sqlcgen.New(pool).GetCMLogoCandidate(context.Background(), sqlcgen.GetCMLogoCandidateParams{NetworkID: 32736, ServiceID: 1024})
	if errors.Is(err, pgx5.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	if c.Stage != nil {
		stage = *c.Stage
	}
	return c.State, stage, true
}

// candidateWorkHeld は logoframe のダミーが止まっている間に during を実行してから Work を終わらせる。
func candidateWorkHeld(t *testing.T, w *CMLogoCandidateWorker, tools cmToolset, recordingID, jobID int64, areaUpdatedAt time.Time, during func()) error {
	t.Helper()
	if err := os.WriteFile(tools.hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- w.Work(context.Background(), cmLogoCandidateJob(recordingID, jobID, areaUpdatedAt))
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(tools.started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("logoframe did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	during()
	if err := os.Remove(tools.hold); err != nil {
		t.Fatal(err)
	}
	return <-done
}

// 解析の失敗（logoframe の失敗・一致率不足・原本なし）はすべて failed の候補行になり、
// 定期パスの desired（ListMissingCMLogoCandidates）に戻らない。
func TestCMLogoCandidateWorkerFailuresBecomeFailedRowsAndAreNotRequeued(t *testing.T) {
	cases := []struct {
		name  string
		stage string
		setup func(t *testing.T, pool *pgxpool.Pool, mediaDir string) (cmToolset, int64)
	}{
		{"logoframe fails", "logo", func(t *testing.T, pool *pgxpool.Pool, mediaDir string) (cmToolset, int64) {
			tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
			writeExecutable(t, filepath.Join(tools.binDir, "logoframe"), "exit 1\n")
			return tools, seedCMRecording(t, pool, mediaDir, 940)
		}},
		{"low match", "match", func(t *testing.T, pool *pgxpool.Pool, mediaDir string) (cmToolset, int64) {
			tools := newFakeCMToolsWithSizeAndReport(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000",
				"1440x1080", "managed logo: v0001 match=9.99% threshold=0%")
			return tools, seedCMRecording(t, pool, mediaDir, 941)
		}},
		{"original missing", "setup", func(t *testing.T, pool *pgxpool.Pool, mediaDir string) (cmToolset, int64) {
			tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
			seedCMRecording(t, pool, mediaDir, 942) // desired を真に保つ別の原本
			gone := seedCMRecording(t, pool, mediaDir, 943)
			if _, err := pool.Exec(context.Background(), `
				INSERT INTO missing_media_assets (media_asset_id)
				SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, gone); err != nil {
				t.Fatal(err)
			}
			return tools, gone
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()
			mediaDir := t.TempDir()
			tools, recordingID := tc.setup(t, pool, mediaDir)
			areaAt := seedTaughtArea(t, pool, 1180)

			err := newCandidateTestWorker(pool, mediaDir, tools).Work(ctx, cmLogoCandidateJob(recordingID, 4400, areaAt))
			if err == nil {
				t.Fatal("Work succeeded, want the failure returned")
			}
			state, stage, ok := candidateRow(t, pool)
			if !ok || state != "failed" || stage != tc.stage {
				t.Fatalf("candidate = %q/%q (exists %v), want failed/%s", state, stage, ok, tc.stage)
			}
			rows, err := sqlcgen.New(pool).ListMissingCMLogoCandidates(ctx, sqlcgen.ListMissingCMLogoCandidatesParams{RowLimit: 100})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Errorf("ListMissingCMLogoCandidates = %v, want none after a failed analysis", rows)
			}
		})
	}
}

// 保存の失敗（stage = save）も failed の行として書ける。CHECK に save が無いと
// MarkCMLogoCandidateFailure 自体が 23514 で落ち、running のまま固まる。
func TestMarkCMLogoCandidateFailureAcceptsSaveStage(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := seedCMRecording(t, pool, t.TempDir(), 944)
	areaAt := seedTaughtArea(t, pool, 1180)
	attemptedAt := insertRunningCandidate(t, pool, id, areaAt)
	stage, message := "save", "saving failed"
	if _, err := sqlcgen.New(pool).MarkCMLogoCandidateFailure(ctx, sqlcgen.MarkCMLogoCandidateFailureParams{
		NetworkID: 32736, ServiceID: 1024, AreaUpdatedAt: areaAt,
		AttemptedAt: attemptedAt, Stage: &stage, Error: &message,
	}); err != nil {
		t.Fatalf("MarkCMLogoCandidateFailure(save): %v", err)
	}
	if state, got, _ := candidateRow(t, pool); state != "failed" || got != "save" {
		t.Errorf("candidate = %q/%q, want failed/save", state, got)
	}
}

// 解析中に枠が変わったら ready は書かれず、この版の running 行は消える。
func TestCMLogoCandidateWorkerDoesNotWriteReadyAfterAreaChanged(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 945)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
	areaAt := seedTaughtArea(t, pool, 1180)
	w := newCandidateTestWorker(pool, mediaDir, tools)

	err := candidateWorkHeld(t, w, tools, id, 4401, areaAt, func() { seedTaughtArea(t, pool, 1100) })
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if state, stage, ok := candidateRow(t, pool); ok {
		t.Errorf("candidate = %q/%q, want none: a result for the old area must not be stored", state, stage)
	}
}

func insertRunningCandidate(t *testing.T, pool *pgxpool.Pool, recordingID int64, areaAt time.Time) time.Time {
	t.Helper()
	attemptedAt, err := sqlcgen.New(pool).InsertCMLogoCandidateRunning(context.Background(), sqlcgen.InsertCMLogoCandidateRunningParams{
		NetworkID: 32736, ServiceID: 1024, RecordingID: recordingID, AreaUpdatedAt: areaAt,
	})
	if err != nil {
		t.Fatalf("InsertCMLogoCandidateRunning: %v", err)
	}
	return attemptedAt
}

func TestCMLogoCandidateLateAttemptCannotChangeRecreatedCandidate(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := seedCMRecording(t, pool, t.TempDir(), 947)
	areaAt := seedTaughtArea(t, pool, 1180)
	q := sqlcgen.New(pool)
	oldAttempt := insertRunningCandidate(t, pool, id, areaAt)
	if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{NetworkID: 32736, ServiceID: 1024}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	newAttempt := insertRunningCandidate(t, pool, id, areaAt)

	updated, err := q.MarkCMLogoCandidateReady(ctx, sqlcgen.MarkCMLogoCandidateReadyParams{
		NetworkID: 32736, ServiceID: 1024, AreaUpdatedAt: areaAt,
		AttemptedAt: oldAttempt, Lgd: []byte("late logo"),
	})
	if err != nil || updated != 0 {
		t.Fatalf("old ready update = %d, %v; want no row", updated, err)
	}
	stage, message := "logo", "late failure"
	failed, err := q.MarkCMLogoCandidateFailure(ctx, sqlcgen.MarkCMLogoCandidateFailureParams{
		NetworkID: 32736, ServiceID: 1024, AreaUpdatedAt: areaAt,
		AttemptedAt: oldAttempt, Stage: &stage, Error: &message,
	})
	if err != nil || failed != 0 {
		t.Fatalf("old failure update = %d, %v; want no row", failed, err)
	}
	deleted, err := q.DeleteCMLogoCandidateForAreaVersion(ctx, sqlcgen.DeleteCMLogoCandidateForAreaVersionParams{
		NetworkID: 32736, ServiceID: 1024, AreaUpdatedAt: areaAt, AttemptedAt: oldAttempt,
	})
	if err != nil || deleted != 0 {
		t.Fatalf("old cleanup = %d, %v; want no row", deleted, err)
	}
	candidate, err := q.GetCMLogoCandidate(ctx, sqlcgen.GetCMLogoCandidateParams{NetworkID: 32736, ServiceID: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != "running" || !candidate.AttemptedAt.Equal(newAttempt) {
		t.Errorf("recreated candidate = %q at %s, want running at %s", candidate.State, candidate.AttemptedAt, newAttempt)
	}
}

// 対応する active River job がある candidate は保ち、job が終端になれば failed にする。
func TestFailOrphanCMLogoCandidates(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx, cancel := context.WithTimeout(riverWorkContext(t, pool), 10*time.Second)
	defer cancel()
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := seedCMRecording(t, pool, t.TempDir(), 948)
	areaAt := seedTaughtArea(t, pool, 1180)
	job, err := client.Insert(ctx, jobs.CMLogoCandidateJobArgs{
		NetworkID: 32736, ServiceID: 1024, RecordingID: id, AreaUpdatedAt: areaAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	insertRunningCandidate(t, pool, id, areaAt)

	if err := failOrphanCMLogoCandidates(ctx, client, pool); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := candidateRow(t, pool); state != "running" {
		t.Fatalf("candidate with a matching active job = %q, want running", state)
	}
	if _, err := client.JobCancel(ctx, job.Job.ID); err != nil {
		t.Fatal(err)
	}
	if err := failOrphanCMLogoCandidates(ctx, client, pool); err != nil {
		t.Fatal(err)
	}
	if state, stage, _ := candidateRow(t, pool); state != "failed" || stage != "stopped" {
		t.Errorf("orphan candidate = %q/%q, want failed/stopped", state, stage)
	}
}

func TestCMLogoCandidateKeyUsesTimestampInstant(t *testing.T) {
	instant := time.Date(2025, time.January, 2, 3, 4, 5, 123456000, time.UTC)
	jobKey := cmLogoCandidateKeyFor(32736, 1024, 948, instant)
	candidateKey := cmLogoCandidateKeyFor(32736, 1024, 948, instant.In(time.FixedZone("offset", 9*60*60)))
	if jobKey != candidateKey {
		t.Fatalf("keys for the same area update instant differ: %#v != %#v", jobKey, candidateKey)
	}
}

// The ingest hint and periodic reconciliation both pass actual recording duration to River jobs.
func TestCMDetectEnqueuePathsUseRecordingDuration(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := riverWorkContext(t, pool)
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		t.Fatal(err)
	}
	setRecordingDuration := func(recordingID int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			UPDATE recordings
			SET started_at = '2025-01-01T00:00:00Z', ended_at = '2025-01-01T00:45:00Z'
			WHERE id = $1`, recordingID); err != nil {
			t.Fatal(err)
		}
	}

	hintRecording := seedCMRecording(t, pool, t.TempDir(), 951)
	setRecordingDuration(hintRecording)
	if err := EnqueueCMDetectionIfNeeded(ctx, pool, client, hintRecording); err != nil {
		t.Fatal(err)
	}
	assertDuration := func(recordingID int64) {
		t.Helper()
		page, err := client.JobList(ctx, river.NewJobListParams().Kinds(jobs.CMDetectJobArgs{}.Kind()).First(10))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Jobs {
			var args jobs.CMDetectJobArgs
			if err := json.Unmarshal(row.EncodedArgs, &args); err != nil {
				t.Fatal(err)
			}
			if args.RecordingID == recordingID {
				if args.RecordingDurationMs != 45*60*1000 {
					t.Errorf("recording %d job duration = %dms, want 2700000ms", recordingID, args.RecordingDurationMs)
				}
				return
			}
		}
		t.Errorf("no CM detection job for recording %d", recordingID)
	}
	assertDuration(hintRecording)

	reconcileRecording := seedCMRecording(t, pool, t.TempDir(), 952)
	setRecordingDuration(reconcileRecording)
	if err := (&CMDetectReconcileWorker{Pool: pool}).Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	assertDuration(reconcileRecording)
}

// 定期パスは、枠があって候補の無い局に解析ジョブを積む。失敗した候補がある局には積まない。
func TestCMDetectReconcileEnqueuesCandidateOnlyWithoutCandidateRow(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := riverWorkContext(t, pool)
	client, err := river.ClientFromContextSafely[pgx5.Tx](ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := seedCMRecording(t, pool, t.TempDir(), 949)
	if _, err := pool.Exec(context.Background(), `
		UPDATE recordings
		SET started_at = '2025-01-01T00:00:00Z', ended_at = '2025-01-01T00:45:00Z'
		WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	areaAt := seedTaughtArea(t, pool, 1180)
	count := func() int { return len(listCandidateJobs(t, ctx, client)) }
	w := &CMDetectReconcileWorker{Pool: pool}
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Fatalf("candidate jobs after the first pass = %d, want 1", got)
	}
	args := listCandidateJobs(t, ctx, client)[0]
	var candidateArgs jobs.CMLogoCandidateJobArgs
	if err := json.Unmarshal(args.EncodedArgs, &candidateArgs); err != nil {
		t.Fatal(err)
	}
	if candidateArgs.RecordingID != id || candidateArgs.NetworkID != 32736 || candidateArgs.RecordingDurationMs != 45*60*1000 {
		t.Errorf("candidate args = %#v, want station 32736, recording %d, and 2700000ms", candidateArgs, id)
	}

	attemptedAt := insertRunningCandidate(t, pool, id, areaAt)
	stage, message := "logo", "failed"
	if _, err := sqlcgen.New(pool).MarkCMLogoCandidateFailure(context.Background(), sqlcgen.MarkCMLogoCandidateFailureParams{
		NetworkID: 32736, ServiceID: 1024, AreaUpdatedAt: areaAt,
		AttemptedAt: attemptedAt, Stage: &stage, Error: &message,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Errorf("candidate jobs after a failed candidate = %d, want still 1", got)
	}
}

func listCandidateJobs(t *testing.T, ctx context.Context, client *river.Client[pgx5.Tx]) []*rivertype.JobRow {
	t.Helper()
	page, err := client.JobList(ctx, activeCMLogoCandidateJobListParams(nil))
	if err != nil {
		t.Fatal(err)
	}
	return page.Jobs
}

// 採用の直前に始まった旧ロゴのジョブの結果は保存しない。実行中に採用（learned_at の更新）が
// あれば SaveCMDetection せず、attempt も消して次の定期パスに任せる。
func TestCMDetectWorkDiscardsResultWhenLogoAdoptedDuringJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 950)
	tools := newFakeCMTools(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000")
	upsert := func() {
		if err := sqlcgen.New(pool).UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
			NetworkID: 32736, ServiceID: 1024, Lgd: buildTestLGD(4, 3, 1000, 4080), LearnedFrom: &id,
			CodedWidth: 1440, CodedHeight: 1080,
		}); err != nil {
			t.Error(err)
		}
	}
	upsert()

	if err := workHeld(t, pool, mediaDir, tools, id, upsert); err != nil {
		t.Fatalf("Work: %v", err)
	}
	var detections, attempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_detections`).Scan(&detections); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if detections != 0 || attempts != 0 {
		t.Errorf("detections = %d, attempts = %d, want 0 / 0: the old-logo result must be dropped", detections, attempts)
	}
}

// 枠あり・ロゴなし・候補ありの局の新しい録画でも、検出は desired のまま走り、logoframe を
// 呼ばずに failed / adopt の attempt を書いて止まる。
func TestCMDetectWorkWritesAdoptAttemptWhileCandidateExists(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 951)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
	areaAt := seedTaughtArea(t, pool, 1180)
	if _, err := pool.Exec(ctx, `
		INSERT INTO cm_logo_candidates (
			network_id, service_id, state, x, y, w, h, coded_width, coded_height,
			recording_id, observed_area_updated_at, lgd
		) VALUES (32736, 1024, 'ready', 1180, 24, 240, 96, 1440, 1080, $1, $2, 'lgd')`, id, areaAt); err != nil {
		t.Fatal(err)
	}
	q := sqlcgen.New(pool)
	if desired, err := q.IsCMDetectionDesired(ctx, id); err != nil || !desired {
		t.Fatalf("IsCMDetectionDesired = %v, %v; want true so the adopt attempt can be written", desired, err)
	}

	if err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	var state, stage string
	if err := pool.QueryRow(ctx, `SELECT state, stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &stage); err != nil {
		t.Fatalf("attempt row: %v", err)
	}
	if state != "failed" || stage != "adopt" {
		t.Errorf("attempt = %q/%q, want failed/adopt", state, stage)
	}
	if _, err := os.Stat(tools.logoframeArgs); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("logoframe ran (stat %v) for a station without an adopted logo", err)
	}
	if desired, err := q.IsCMDetectionDesired(ctx, id); err != nil || desired {
		t.Errorf("IsCMDetectionDesired after the adopt attempt = %v, %v; want false", desired, err)
	}
}
