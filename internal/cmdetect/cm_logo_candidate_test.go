package cmdetect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func newCandidateTestWorker(pool *pgxpool.Pool, mediaDir string, tools cmToolset) *CMLogoCandidateWorker {
	return &CMLogoCandidateWorker{
		Store: pgstoretest.Analysis(pool), MediaDir: mediaDir, ScratchDir: filepath.Join(mediaDir, "scratch"),
		CMDetect: config.CMDetectConfig{Enabled: true, BinaryDir: tools.binDir}, FFprobe: tools.ffprobe,
	}
}

// seedTaughtArea は局 32736/1024 に枠を置き、その updated_at を返す。
func seedTaughtArea(t *testing.T, pool *pgxpool.Pool, x int) time.Time {
	return seedTaughtAreaForService(t, pool, 1024, x)
}

func seedTaughtAreaForService(t *testing.T, pool *pgxpool.Pool, serviceID int32, x int) time.Time {
	t.Helper()
	dbtest.CMLogoArea(t, pool, func(p *dbtest.CMLogoAreaParams) {
		p.ServiceID = serviceID
		p.X = int32(x)
	})
	area, err := pgstoretest.Analysis(pool).GetLogoArea(context.Background(), store.CMStationRef{NetworkID: 32736, ServiceID: serviceID})
	testutil.RequireNoError(t, err, "GetLogoArea")
	return area.UpdatedAt
}

func candidateRow(t *testing.T, pool *pgxpool.Pool) (state, stage string, ok bool) {
	return candidateRowForService(t, pool, 1024)
}

func candidateRowForService(t *testing.T, pool *pgxpool.Pool, serviceID int32) (state, stage string, ok bool) {
	t.Helper()
	c, err := pgstoretest.Analysis(pool).GetLogoCandidate(context.Background(), store.CMStationRef{NetworkID: 32736, ServiceID: serviceID})
	if errors.Is(err, store.ErrNotFound) {
		return "", "", false
	}
	testutil.RequireNoError(t, err, "GetLogoCandidate")
	if c.Stage != nil {
		stage = *c.Stage
	}
	return c.State, stage, true
}

type candidateRecordingLookupFailureStore struct {
	store.CMStation
	err error
}

func (s candidateRecordingLookupFailureStore) GetCandidateRecording(context.Context, int64) (store.CMLogoCandidateRecording, error) {
	return store.CMLogoCandidateRecording{}, s.err
}

func TestCMLogoCandidateWorkerRecordingLookupFailureUsesSetupStage(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
	id := seedCMDetectionState(t, pool, mediaDir, dbtest.Recording(t, pool, cmRecordingOptions(939)))
	areaAt := seedTaughtArea(t, pool, 1180)
	worker := newCandidateTestWorker(pool, mediaDir, tools)
	worker.Store = candidateRecordingLookupFailureStore{
		CMStation: worker.Store,
		err:       errors.New("injected recording lookup failure"),
	}

	err := jobqfaketest.RunWorkerOnce(context.Background(), worker, cmLogoCandidateJob(id, areaAt))
	if err == nil {
		t.Fatal("Work succeeded, want the recording lookup failure")
	}
	state, stage, ok := candidateRow(t, pool)
	if !ok || state != "failed" || stage != "setup" {
		t.Fatalf("candidate = %q/%q (exists %v), want failed/setup", state, stage, ok)
	}
}

// candidateWorkHeld は logoframe のダミーが止まっている間に during を実行してから Work を終わらせる。
func candidateWorkHeld(ctx context.Context, t *testing.T, w *CMLogoCandidateWorker, tools cmToolset, recordingID int64, areaUpdatedAt time.Time, during func()) error {
	t.Helper()
	testutil.RequireNoError(t, os.WriteFile(tools.hold, nil, 0o600), "writing test file")
	done := make(chan error, 1)
	go func() {
		done <- jobqfaketest.RunWorkerOnce(ctx, w, cmLogoCandidateJob(recordingID, areaUpdatedAt))
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
	testutil.RequireNoError(t, os.Remove(tools.hold), "removing hold file")
	return <-done
}

// 解析の失敗（logoframe の失敗・一致率不足・原本なし）はすべて failed の候補行になる。
// 行が定期パスの desired に戻らないことは internal/db の TestCMLogoCandidateDesired が見る。
func TestCMLogoCandidateWorkerFailuresBecomeFailedRows(t *testing.T) {
	cases := []struct {
		name  string
		stage string
		setup func(t *testing.T, pool *pgxpool.Pool, mediaDir string) (cmToolset, int64)
	}{
		{"logoframe fails", "logo", func(t *testing.T, pool *pgxpool.Pool, mediaDir string) (cmToolset, int64) {
			tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
			writeExecutable(t, filepath.Join(tools.binDir, "logoframe"), "exit 1\n")
			return tools, seedCMDetectionState(t, pool, mediaDir, dbtest.Recording(t, pool, cmRecordingOptions(940)))
		}},
		{"low match", "match", func(t *testing.T, pool *pgxpool.Pool, mediaDir string) (cmToolset, int64) {
			tools := newFakeCMToolsWithSizeAndReport(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000",
				"1440x1080", "managed logo: v0001 match=9.99% threshold=0%")
			return tools, seedCMDetectionState(t, pool, mediaDir, dbtest.Recording(t, pool, cmRecordingOptions(941)))
		}},
		{"original missing", "setup", func(t *testing.T, pool *pgxpool.Pool, mediaDir string) (cmToolset, int64) {
			tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
			seedCMDetectionState(t, pool, mediaDir, dbtest.Recording(t, pool, cmRecordingOptions(942))) // desired を真に保つ別の原本
			gone := seedCMDetectionState(t, pool, mediaDir, dbtest.Recording(t, pool, cmRecordingOptions(943)))
			testutil.MustExec(t, pool, "inserting missing_media_assets", `
				INSERT INTO missing_media_assets (media_asset_copy_id)
				SELECT copy.id FROM media_asset_copies copy JOIN media_assets asset ON asset.id = copy.media_asset_id WHERE asset.recording_id = $1 AND asset.kind = 'original' AND copy.store = 'test-store'`, gone)
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

			err := jobqfaketest.RunWorkerOnce(ctx, newCandidateTestWorker(pool, mediaDir, tools), cmLogoCandidateJob(recordingID, areaAt))
			if err == nil {
				t.Fatal("Work succeeded, want the failure returned")
			}
			state, stage, ok := candidateRow(t, pool)
			if !ok || state != "failed" || stage != tc.stage {
				t.Fatalf("candidate = %q/%q (exists %v), want failed/%s", state, stage, ok, tc.stage)
			}
		})
	}
}

// 保存の失敗（stage = save）も failed の行として書ける。CHECK に save が無いと
// MarkLogoCandidateFailed 自体が 23514 で落ち、running のまま固まる。
func TestMarkCMLogoCandidateFailureAcceptsSaveStage(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(944)))
	areaAt := seedTaughtArea(t, pool, 1180)
	attemptedAt := insertRunningCandidate(t, pool, 1024, id, areaAt)
	stage, message := "save", "saving failed"
	ok, err := pgstoretest.Analysis(pool).MarkLogoCandidateFailed(ctx, store.CMStationRef{NetworkID: 32736, ServiceID: 1024}, areaAt, attemptedAt, &stage, message)
	if err != nil || !ok {
		t.Fatalf("MarkLogoCandidateFailed(save) = %v, %v; want the running row marked", ok, err)
	}
	if state, got, _ := candidateRow(t, pool); state != "failed" || got != "save" {
		t.Errorf("candidate = %q/%q, want failed/save", state, got)
	}
}

// 解析中に枠が変わったら ready は書かれず、この版の running 行は消える。
func TestCMLogoCandidateWorkerDoesNotWriteReadyAfterAreaChanged(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := seedCMDetectionState(t, pool, mediaDir, dbtest.Recording(t, pool, cmRecordingOptions(945)))
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
	areaAt := seedTaughtArea(t, pool, 1180)
	w := newCandidateTestWorker(pool, mediaDir, tools)

	err := candidateWorkHeld(context.Background(), t, w, tools, id, areaAt, func() { seedTaughtArea(t, pool, 1100) })
	testutil.RequireNoError(t, err, "Work")
	if state, stage, ok := candidateRow(t, pool); ok {
		t.Errorf("candidate = %q/%q, want none: a result for the old area must not be stored", state, stage)
	}
}

func TestCMLogoCandidateWorkerCancellationClearsRunningRow(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := seedCMDetectionState(t, pool, mediaDir, dbtest.Recording(t, pool, cmRecordingOptions(946)))
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
	areaAt := seedTaughtArea(t, pool, 1180)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := candidateWorkHeld(ctx, t, newCandidateTestWorker(pool, mediaDir, tools), tools, id, areaAt, cancel)
	if err == nil {
		t.Fatal("Work succeeded after its context was canceled")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context error = %v, want cancellation", ctx.Err())
	}
	if state, stage, ok := candidateRow(t, pool); ok {
		t.Errorf("candidate = %q/%q, want no running row after cancellation", state, stage)
	}
}

// seedCandidateRecording は局 32736/serviceID に原本付きの録画を作る。その局の候補の解析元になれる（desired の条件）。
func seedCandidateRecording(t *testing.T, pool *pgxpool.Pool, serviceID int32) int64 {
	t.Helper()
	return seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) {
		p.NetworkID = 32736
		p.ServiceID = serviceID
		p.EventID = serviceID
	}))
}

// insertRunningCandidate は局 32736/serviceID の running 候補を契約の BeginLogoCandidate で作り、試行時刻を返す。
// areaAt はその局の現在の枠の版、recordingID はその局の原本付きの録画を渡す。
func insertRunningCandidate(t *testing.T, pool *pgxpool.Pool, serviceID int32, recordingID int64, areaAt time.Time) time.Time {
	t.Helper()
	attemptedAt, started, err := pgstoretest.Analysis(pool).BeginLogoCandidate(context.Background(), store.CMStationRef{NetworkID: 32736, ServiceID: serviceID}, recordingID, areaAt)
	testutil.RequireNoError(t, err, "BeginLogoCandidate")
	if !started {
		t.Fatalf("BeginLogoCandidate(service %d, recording %d) did not start a running candidate", serviceID, recordingID)
	}
	return attemptedAt
}

func TestFailOrphanCMLogoCandidatesWaitsForDeadlineAndGrace(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	var dbNow time.Time
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow), "reading DB clock")
	grace := 2 * time.Minute
	deadline := cmDetectionTimeout(45 * 60 * 1000)
	if deadline != 90*time.Minute {
		t.Fatalf("cmDetectionTimeout(45m) = %s, want 90m", deadline)
	}
	ages := map[int32]time.Duration{
		1024: deadline + grace + 5*time.Second,
		1025: deadline + grace/2,
		1026: deadline - 15*time.Minute,
	}
	for serviceID, age := range ages {
		id := seedCandidateRecording(t, pool, serviceID)
		testutil.MustExec(t, pool, "updating recordings", `
			UPDATE recordings
			SET started_at = '2025-01-01T00:00:00Z', ended_at = '2025-01-01T00:45:00Z'
			WHERE id = $1`, id)
		areaAt := seedTaughtAreaForService(t, pool, serviceID, 1180)
		insertRunningCandidate(t, pool, serviceID, id, areaAt)
		testutil.MustExec(t, pool, "updating cm_logo_candidates", `
			UPDATE cm_logo_candidates SET attempted_at = $3
			WHERE network_id = 32736 AND service_id = $1 AND recording_id = $2`,
			serviceID, id, dbNow.Add(-age))
	}
	testutil.RequireNoError(t, pgstoretest.Analysis(pool).FailOrphanLogoCandidates(ctx, grace, cmDetectionTimeout), "FailOrphanLogoCandidates")
	for serviceID, want := range map[int32][2]string{
		1024: {"failed", "stopped"}, 1025: {"running", ""}, 1026: {"running", ""},
	} {
		state, stage, ok := candidateRowForService(t, pool, serviceID)
		if !ok || state != want[0] || (want[1] != "" && stage != want[1]) {
			t.Errorf("candidate service %d = %q/%q, want %q/%q", serviceID, state, stage, want[0], want[1])
		}
	}
}

// 録画行が消えて recording_id が NULL になった候補も、最小締切と猶予で回収する。
func TestFailOrphanCMLogoCandidatesWithoutRecordingUsesMinimumDeadline(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	var dbNow time.Time
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow), "reading DB clock")
	grace := 2 * time.Minute
	deadline := cmDetectionTimeout(0)
	ages := map[int32]time.Duration{
		1024: deadline + grace + 5*time.Second,
		1025: deadline + grace/2,
	}
	for serviceID, age := range ages {
		id := seedCandidateRecording(t, pool, serviceID)
		areaAt := seedTaughtAreaForService(t, pool, serviceID, 1180)
		insertRunningCandidate(t, pool, serviceID, id, areaAt)
		testutil.MustExec(t, pool, "updating cm_logo_candidates", `
			UPDATE cm_logo_candidates SET recording_id = NULL, attempted_at = $2
			WHERE network_id = 32736 AND service_id = $1`, serviceID, dbNow.Add(-age))
	}
	testutil.RequireNoError(t, pgstoretest.Analysis(pool).FailOrphanLogoCandidates(ctx, grace, cmDetectionTimeout), "FailOrphanLogoCandidates")
	for serviceID, want := range map[int32][2]string{1024: {"failed", "stopped"}, 1025: {"running", ""}} {
		state, stage, ok := candidateRowForService(t, pool, serviceID)
		if !ok || state != want[0] || (want[1] != "" && stage != want[1]) {
			t.Errorf("candidate service %d = %q/%q, want %q/%q", serviceID, state, stage, want[0], want[1])
		}
	}
}

// The CM detection reconciliation pass passes the actual recording duration in Args.
func TestCMDetectReconcileUsesRecordingDuration(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	analysis := pgstoretest.Analysis(pool)
	setRecordingDuration := func(recordingID int64) {
		t.Helper()
		testutil.MustExec(t, pool, "updating recordings", `
			UPDATE recordings
			SET started_at = '2025-01-01T00:00:00Z', ended_at = '2025-01-01T00:45:00Z'
			WHERE id = $1`, recordingID)
	}

	assertDuration := func(recordingID int64) {
		t.Helper()
		for _, args := range recordedArgs[jobs.CMDetectJobArgs](t, enqueuer.CallsOfKind(jobs.CMDetectJobArgs{}.Kind())) {
			if args.RecordingID == recordingID {
				if args.RecordingDurationMs != 45*60*1000 {
					t.Errorf("recording %d job duration = %dms, want 2700000ms", recordingID, args.RecordingDurationMs)
				}
				return
			}
		}
		t.Errorf("no CM detection job for recording %d", recordingID)
	}
	reconcileRecording := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(952)))
	setRecordingDuration(reconcileRecording)
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, (&CMDetectReconcileWorker{Enqueuer: enqueuer, Locator: pgstoretest.Media(pool), DetectionStore: analysis}), jobs.CMDetectReconcileArgs{}), "jobqfaketest.RunWorkerOnce")
	assertDuration(reconcileRecording)
}

func TestCMDetectReconcileClearsDisabledAndPreTransitionAttempts(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	det := pgstoretest.Analysis(pool)
	enqueuer := jobqfaketest.NewRecordingEnqueuer()

	newFailedRecording := func(eventID int32) int64 {
		t.Helper()
		id := dbtest.Recording(t, pool, cmRecordingOptions(eventID))
		seedCMDetectionState(t, pool, t.TempDir(), id)
		attemptCount := startCMDetectionTestAttempt(t, ctx, det, id)
		markCMDetectionTestFailure(t, ctx, pool, det, id, attemptCount, "failed", nil, nil)
		return id
	}
	attemptRows := func(recordingID int64) int {
		t.Helper()
		var count int
		testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_attempts WHERE recording_id = $1`, recordingID).Scan(&count), "counting recording_cm_attempts")
		return count
	}

	disabled := newFailedRecording(953)
	setCMDetectPolicy(t, ctx, pool, disabled, false)

	quicklyReenabled := newFailedRecording(954)
	setCMDetectPolicy(t, ctx, pool, quicklyReenabled, false)
	setCMDetectPolicy(t, ctx, pool, quicklyReenabled, true)
	if desired, err := det.IsDetectionDesired(ctx, quicklyReenabled); err != nil || !desired {
		t.Fatalf("desired before reconcile for quick toggle = %v, %v; want true (the old failed row is not a current attempt)", desired, err)
	}

	// 再有効化の後に始めた試行は現在の世代で、同値の再送では消えない。
	newAttemptAfterReenable := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(955)))
	setCMDetectPolicy(t, ctx, pool, newAttemptAfterReenable, false)
	setCMDetectPolicy(t, ctx, pool, newAttemptAfterReenable, true)
	attemptCount := startCMDetectionTestAttempt(t, ctx, det, newAttemptAfterReenable)
	markCMDetectionTestFailure(t, ctx, pool, det, newAttemptAfterReenable, attemptCount, "failed", nil, nil)
	setCMDetectPolicy(t, ctx, pool, newAttemptAfterReenable, true) // idempotent

	w := &CMDetectReconcileWorker{Enqueuer: enqueuer, Locator: pgstoretest.Media(pool), DetectionStore: det}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, jobs.CMDetectReconcileArgs{}), "jobqfaketest.RunWorkerOnce")
	if got := attemptRows(disabled); got != 0 {
		t.Errorf("disabled policy attempt rows = %d, want 0", got)
	}
	if got := attemptRows(quicklyReenabled); got != 0 {
		t.Errorf("pre-disable attempt rows after quick re-enable = %d, want 0", got)
	}
	if got := attemptRows(newAttemptAfterReenable); got != 1 {
		t.Errorf("post-disable attempt rows = %d, want 1", got)
	}
	if desired, err := det.IsDetectionDesired(ctx, quicklyReenabled); err != nil || !desired {
		t.Errorf("desired after stale attempt cleanup = %v, %v; want true", desired, err)
	}
	enqueuedJobs := recordedArgs[jobs.CMDetectJobArgs](t, enqueuer.CallsOfKind(jobs.CMDetectJobArgs{}.Kind()))
	if len(enqueuedJobs) != 1 || enqueuedJobs[0].RecordingID != quicklyReenabled {
		t.Errorf("reconciled CM jobs = %#v, want only recording %d", enqueuedJobs, quicklyReenabled)
	}
}

// 定期パスは、枠があって候補の無い局に解析ジョブを積む。失敗した候補がある局には積まない。
func TestCMDetectReconcileEnqueuesCandidateOnlyWithoutCandidateRow(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	id := seedCMDetectionState(t, pool, t.TempDir(), dbtest.Recording(t, pool, cmRecordingOptions(949)))
	testutil.MustExec(t, pool, "updating recordings", `
		UPDATE recordings
		SET started_at = '2025-01-01T00:00:00Z', ended_at = '2025-01-01T00:45:00Z'
		WHERE id = $1`, id)
	areaAt := seedTaughtArea(t, pool, 1180)
	count := func() int {
		return len(enqueuer.CallsOfKind(jobs.CMLogoCandidateJobArgs{}.Kind()))
	}
	w := &CMDetectReconcileWorker{Enqueuer: enqueuer, Locator: pgstoretest.Media(pool), DetectionStore: pgstoretest.Analysis(pool)}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, jobs.CMDetectReconcileArgs{}), "jobqfaketest.RunWorkerOnce")
	if got := count(); got != 1 {
		t.Fatalf("candidate jobs after the first pass = %d, want 1", got)
	}
	if got := len(enqueuer.CallsOfKind(jobs.CMDetectReconcileArgs{}.Kind())); got != 0 {
		t.Errorf("continuation jobs for short detection and candidate pages = %d, want 0", got)
	}
	candidateArgs := recordedArgs[jobs.CMLogoCandidateJobArgs](t, enqueuer.CallsOfKind(jobs.CMLogoCandidateJobArgs{}.Kind()))[0]
	if candidateArgs.RecordingID != id || candidateArgs.NetworkID != 32736 || candidateArgs.RecordingDurationMs != 45*60*1000 {
		t.Errorf("candidate args = %#v, want station 32736, recording %d, and 2700000ms", candidateArgs, id)
	}

	attemptedAt := insertRunningCandidate(t, pool, 1024, id, areaAt)
	stage, message := "logo", "failed"
	if ok, err := pgstoretest.Analysis(pool).MarkLogoCandidateFailed(context.Background(), store.CMStationRef{NetworkID: 32736, ServiceID: 1024}, areaAt, attemptedAt, &stage, message); err != nil || !ok {
		t.Fatalf("MarkLogoCandidateFailed = %v, %v", ok, err)
	}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, w, jobs.CMDetectReconcileArgs{}), "jobqfaketest.RunWorkerOnce")
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
	id := seedCMDetectionState(t, pool, mediaDir, dbtest.Recording(t, pool, cmRecordingOptions(950)))
	tools := newFakeCMTools(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000")
	upsert := func() {
		dbtest.CMLogo(t, pool, func(p *dbtest.CMLogoParams) { p.LearnedFrom = &id })
	}
	upsert()

	testutil.RequireNoError(t, workHeld(t, pool, mediaDir, tools, id, upsert), "Work")
	var detections, attempts int
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_detections`).Scan(&detections), "counting recording_cm_detections")
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_attempts`).Scan(&attempts), "counting recording_cm_attempts")
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
	id := seedCMDetectionState(t, pool, mediaDir, dbtest.Recording(t, pool, cmRecordingOptions(951)))
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
	areaAt := seedTaughtArea(t, pool, 1180)
	testutil.MustExec(t, pool, "inserting cm_logo_candidates", `
		INSERT INTO cm_logo_candidates (
			network_id, service_id, state, x, y, w, h, coded_width, coded_height,
			recording_id, observed_area_updated_at, lgd
		) VALUES (32736, 1024, 'ready', 1180, 24, 240, 96, 1440, 1080, $1, $2, 'lgd')`, id, areaAt)
	det := pgstoretest.Analysis(pool)
	if desired, err := det.IsDetectionDesired(ctx, id); err != nil || !desired {
		t.Fatalf("IsDetectionDesired = %v, %v; want true so the adopt attempt can be written", desired, err)
	}

	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(ctx, newCMDetectTestWorker(pool, mediaDir, tools), cmJob(id)), "Work")
	var state, stage string
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT state, stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &stage), "attempt row")
	if state != "failed" || stage != "adopt" {
		t.Errorf("attempt = %q/%q, want failed/adopt", state, stage)
	}
	if _, err := os.Stat(tools.logoframeArgs); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("logoframe ran (stat %v) for a station without an adopted logo", err)
	}
	if desired, err := det.IsDetectionDesired(ctx, id); err != nil || desired {
		t.Errorf("IsDetectionDesired after the adopt attempt = %v, %v; want false", desired, err)
	}
}
