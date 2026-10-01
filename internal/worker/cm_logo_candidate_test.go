package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
	insertRunningCandidate(t, pool, id, areaAt)
	stage, message := "save", "saving failed"
	if err := sqlcgen.New(pool).MarkCMLogoCandidateFailure(ctx, sqlcgen.MarkCMLogoCandidateFailureParams{
		NetworkID: 32736, ServiceID: 1024, AreaUpdatedAt: areaAt, Stage: &stage, Error: &message,
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

// 動いている解析は回収されない（Work が job lock を保持する）。回収すると running が
// failed にされ、成功時の ready が 0 行になって候補が失われる。
func TestRecoverStaleCMLogoCandidateJobsSparesLiveWorker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 946)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
	areaAt := seedTaughtArea(t, pool, 1180)
	jobID := insertCandidateJobRow(t, pool, id, areaAt, "running", 1, 1)
	w := newCandidateTestWorker(pool, mediaDir, tools)

	err := candidateWorkHeld(t, w, tools, id, jobID, areaAt, func() {
		if err := recoverStaleCMLogoCandidateJobs(ctx, pool); err != nil {
			t.Error(err)
		}
		// 生きた River ジョブ（running）を持つ行は orphan 回収も触らない。
		if err := failOrphanCMLogoCandidates(ctx, pool); err != nil {
			t.Error(err)
		}
		if state, _, _ := candidateRow(t, pool); state != "running" {
			t.Errorf("candidate during a live analysis = %q, want running", state)
		}
	})
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if state, _, _ := candidateRow(t, pool); state != "ready" {
		t.Errorf("candidate after the analysis = %q, want ready", state)
	}
}

func insertCandidateJobRow(t *testing.T, pool *pgxpool.Pool, recordingID int64, areaAt time.Time, state string, attempt, maxAttempts int) int64 {
	t.Helper()
	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Insert(context.Background(), jobs.CMLogoCandidateJobArgs{
		NetworkID: 32736, ServiceID: 1024, RecordingID: recordingID, AreaUpdatedAt: areaAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE river_job SET state = $2::river_job_state, attempt = $3, max_attempts = $4,
		       attempted_at = now() - interval '1 hour'
		WHERE id = $1`, res.Job.ID, state, attempt, maxAttempts); err != nil {
		t.Fatal(err)
	}
	return res.Job.ID
}

func insertRunningCandidate(t *testing.T, pool *pgxpool.Pool, recordingID int64, areaAt time.Time) {
	t.Helper()
	if n, err := sqlcgen.New(pool).InsertCMLogoCandidateRunning(context.Background(), sqlcgen.InsertCMLogoCandidateRunningParams{
		NetworkID: 32736, ServiceID: 1024, RecordingID: recordingID, AreaUpdatedAt: areaAt,
	}); err != nil || n != 1 {
		t.Fatalf("InsertCMLogoCandidateRunning = %d, %v", n, err)
	}
}

// 死んだ解析（lock が取れる古い running）は回収する。試行が残っていれば行を消して
// 再投入に任せ、使い切っていれば failed / stopped にする。
func TestRecoverStaleCMLogoCandidateJobsRecoversDeadWorker(t *testing.T) {
	for _, tc := range []struct {
		name         string
		attempt, max int
		wantJob      string
		wantRow      string
	}{
		{"last attempt", 1, 1, "discarded", "failed/stopped"},
		{"attempts left", 1, 3, "retryable", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()
			id := seedCMRecording(t, pool, t.TempDir(), 947)
			areaAt := seedTaughtArea(t, pool, 1180)
			jobID := insertCandidateJobRow(t, pool, id, areaAt, "running", tc.attempt, tc.max)
			insertRunningCandidate(t, pool, id, areaAt)

			if err := recoverStaleCMLogoCandidateJobs(ctx, pool); err != nil {
				t.Fatal(err)
			}
			var jobState string
			if err := pool.QueryRow(ctx, `SELECT state::text FROM river_job WHERE id = $1`, jobID).Scan(&jobState); err != nil {
				t.Fatal(err)
			}
			if jobState != tc.wantJob {
				t.Errorf("river job = %q, want %q", jobState, tc.wantJob)
			}
			state, stage, ok := candidateRow(t, pool)
			got := "none"
			if ok {
				got = state + "/" + stage
			}
			if got != tc.wantRow {
				t.Errorf("candidate = %s, want %s", got, tc.wantRow)
			}
		})
	}
}

// 対応する未完了ジョブが無い running 行（River が discarded にした・fail() が書けなかった）は
// River の状態に頼らず failed / stopped にする。未完了ジョブがあるものは触らない。
func TestFailOrphanCMLogoCandidates(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := seedCMRecording(t, pool, t.TempDir(), 948)
	areaAt := seedTaughtArea(t, pool, 1180)
	insertRunningCandidate(t, pool, id, areaAt)

	insertCandidateJobRow(t, pool, id, areaAt, "available", 0, 1)
	if err := failOrphanCMLogoCandidates(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := candidateRow(t, pool); state != "running" {
		t.Fatalf("candidate with a live job = %q, want running", state)
	}

	if _, err := pool.Exec(ctx, `UPDATE river_job SET state = 'discarded', finalized_at = now() WHERE kind = 'cm_logo_candidate'`); err != nil {
		t.Fatal(err)
	}
	if err := failOrphanCMLogoCandidates(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if state, stage, _ := candidateRow(t, pool); state != "failed" || stage != "stopped" {
		t.Errorf("orphan candidate = %q/%q, want failed/stopped", state, stage)
	}
}

// 定期パスは、枠があって候補の無い局に解析ジョブを積む。失敗した候補がある局には積まない。
func TestCMDetectReconcileEnqueuesCandidateOnlyWithoutCandidateRow(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := riverWorkContext(t, pool)
	id := seedCMRecording(t, pool, t.TempDir(), 949)
	areaAt := seedTaughtArea(t, pool, 1180)
	count := func() int {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM river_job WHERE kind = 'cm_logo_candidate'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	w := &CMDetectReconcileWorker{Pool: pool}
	if err := w.Work(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Fatalf("candidate jobs after the first pass = %d, want 1", got)
	}
	var args string
	if err := pool.QueryRow(context.Background(), `SELECT args::text FROM river_job WHERE kind = 'cm_logo_candidate'`).Scan(&args); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(args, `"recording_id"`) || !strings.Contains(args, `"network_id": 32736`) {
		t.Errorf("candidate job args = %s, want station 32736 and a recording", args)
	}

	insertRunningCandidate(t, pool, id, areaAt)
	stage, message := "logo", "failed"
	if err := sqlcgen.New(pool).MarkCMLogoCandidateFailure(context.Background(), sqlcgen.MarkCMLogoCandidateFailureParams{
		NetworkID: 32736, ServiceID: 1024, AreaUpdatedAt: areaAt, Stage: &stage, Error: &message,
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

	if err := workHeld(t, pool, mediaDir, tools, id, 1, upsert); err != nil {
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
