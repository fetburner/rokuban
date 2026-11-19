package cmdetect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

func startCMDetectionTestAttempt(t *testing.T, ctx context.Context, q *sqlcgen.Queries, recordingID int64) int32 {
	t.Helper()
	attempt, err := q.BeginCMDetectionAttempt(ctx, sqlcgen.BeginCMDetectionAttemptParams{
		RecordingID: recordingID, MaxAttempts: cmDetectMaxTries,
	})
	if err != nil || !attempt.ShouldRun {
		t.Fatalf("BeginCMDetectionAttempt(%d) = %#v, %v", recordingID, attempt, err)
	}
	return attempt.AttemptCount
}

// cmAttemptGeneration は試行行が持つ方針世代を読む。ABA を狙わないテストが
// 「その試行を始めた worker」と同じ世代で書くために使う。
func cmAttemptGeneration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, recordingID int64) int64 {
	t.Helper()
	var generation int64
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT policy_generation FROM recording_cm_attempts WHERE recording_id = $1`, recordingID).Scan(&generation), "reading attempt policy generation")
	return generation
}

func startCMDetectionTestAttemptRow(t *testing.T, ctx context.Context, q *sqlcgen.Queries, recordingID int64) sqlcgen.BeginCMDetectionAttemptRow {
	t.Helper()
	attempt, err := q.BeginCMDetectionAttempt(ctx, sqlcgen.BeginCMDetectionAttemptParams{
		RecordingID: recordingID, MaxAttempts: cmDetectMaxTries,
	})
	if err != nil || !attempt.ShouldRun {
		t.Fatalf("BeginCMDetectionAttempt(%d) = %#v, %v", recordingID, attempt, err)
	}
	return attempt
}

func markCMDetectionTestFailure(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	q *sqlcgen.Queries,
	recordingID int64,
	attemptCount int32,
	state string,
	stage *string,
	message *string,
) {
	t.Helper()
	if message == nil {
		defaultMessage := "test failure"
		message = &defaultMessage
	}
	n, err := q.MarkCMDetectionFailure(ctx, sqlcgen.MarkCMDetectionFailureParams{
		RecordingID: recordingID, AttemptCount: attemptCount,
		State: state, Stage: stage, Error: message,
		PolicyGeneration: cmAttemptGeneration(t, ctx, pool, recordingID),
	})
	if err != nil || n != 1 {
		t.Fatalf("MarkCMDetectionFailure(%d, %d) = %d, %v", recordingID, attemptCount, n, err)
	}
}

func TestCMFrameTimeConversionsUse30000Over1001(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		ms         int64
		frame      int64
		wantMillis int64
	}{
		{name: "zero", ms: 0, frame: 0, wantMillis: 0},
		{name: "exact 30 frames", ms: 1001, frame: 30, wantMillis: 1001},
		{name: "round down", ms: 16, frame: 0, wantMillis: 0},
		{name: "round up", ms: 17, frame: 1, wantMillis: 33},
		{name: "second frame", ms: 2002, frame: 60, wantMillis: 2002},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := chapters.MsToFrame(tt.ms); got != tt.frame {
				t.Errorf("chapters.MsToFrame(%d) = %d, want %d", tt.ms, got, tt.frame)
			}
			if got := chapters.FrameToMs(tt.frame); got != tt.wantMillis {
				t.Errorf("chapters.FrameToMs(%d) = %d, want %d", tt.frame, got, tt.wantMillis)
			}
		})
	}
}

func TestCMRangesFromCutAVSInvertsMainIntervals(t *testing.T) {
	t.Parallel()
	got, err := cmRangesFromCutAVS("Trim(150,269) ++ Trim(30,89)", 10010)
	testutil.RequireNoError(t, err, "cmRangesFromCutAVS")
	want := []frameRange{{start: 0, end: 30}, {start: 90, end: 150}, {start: 270, end: 300}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CM frame ranges = %#v, want %#v", got, want)
	}
	wantMs := []chapters.Range{{StartMs: 0, EndMs: 1001}, {StartMs: 3003, EndMs: 5005}, {StartMs: 9009, EndMs: 10010}}
	if gotMs := chapterRangesFromFrames(got, 10010); !reflect.DeepEqual(gotMs, wantMs) {
		t.Errorf("millisecond ranges = %#v, want %#v", gotMs, wantMs)
	}
}

func TestCMRangesFromCutAVSNoCommercialsAndInvalidOutput(t *testing.T) {
	t.Parallel()
	got, err := cmRangesFromCutAVS("Trim(0,299)", 10010)
	testutil.RequireNoError(t, err, "cmRangesFromCutAVS")
	if len(got) != 0 || len(chapterRangesFromFrames(got, 10010)) != 0 {
		t.Fatalf("full-program Trim produced CM ranges %#v", got)
	}
	if _, err := cmRangesFromCutAVS("# no Trim calls", 10010); err == nil {
		t.Fatal("missing Trim calls must fail rather than store an empty result")
	}
	if _, err := cmRangesFromCutAVS("Trim(0,0)", 0); err == nil {
		t.Fatal("non-positive program duration must fail")
	}
}

func TestCMDetectionTimeoutUsesTwiceDurationWithThirtyMinuteMinimum(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		durationMs int64
		want       time.Duration
	}{
		{durationMs: 0, want: 30 * time.Minute},
		{durationMs: 10 * 60 * 1000, want: 30 * time.Minute},
		{durationMs: 20 * 60 * 1000, want: 40 * time.Minute},
	} {
		args := jobs.CMDetectJobArgs{RecordingDurationMs: tt.durationMs}
		if got := args.Spec().Timeout(args); got != tt.want {
			t.Errorf("CMDetectJobArgs.Spec().Timeout(%d) = %s, want %s", tt.durationMs, got, tt.want)
		}
	}
}

func TestCMWorkerTimeoutsUseDurationFromJobArgs(t *testing.T) {
	t.Parallel()
	cmArgs := cmJob(1)
	cmArgs.RecordingDurationMs = 45 * 60 * 1000
	if got := cmArgs.Spec().Timeout(cmArgs); got != 90*time.Minute {
		t.Errorf("CMDetectJobArgs.Spec timeout = %s, want 90m", got)
	}
	logoArgs := cmLogoCandidateJob(1, time.Now())
	logoArgs.RecordingDurationMs = 45 * 60 * 1000
	if got := logoArgs.Spec().Timeout(logoArgs); got != 90*time.Minute {
		t.Errorf("CMLogoCandidateJobArgs.Spec timeout = %s, want 90m", got)
	}
	logoArgs.RecordingDurationMs = 0 // Old queued args omit this field.
	if got := logoArgs.Spec().Timeout(logoArgs); got != 30*time.Minute {
		t.Errorf("old CMLogoCandidateJobArgs.Spec timeout = %s, want 30m", got)
	}
}

func TestCMFailureStagePreservesWorkerObservation(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"setup", "probe", "area", "logo", "chapter", "join", "parse", "save", "stopped", "resolution", "match"} {
		t.Run(stage, func(t *testing.T) {
			got := cmFailureStage(fmt.Errorf("outer: %w", cmFailure(stage, fmt.Errorf("failure"))))
			if got == nil || *got != stage {
				t.Fatalf("cmFailureStage = %v, want %q", got, stage)
			}
		})
	}
	if got := cmFailureStage(fmt.Errorf("unclassified")); got != nil {
		t.Fatalf("cmFailureStage(unclassified) = %q, want nil", *got)
	}
}

func TestCMLogoMatchPercentParsesLogoframeOutput(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		output  string
		want    float64
		wantErr bool
	}{
		{name: "stderr line", output: "managed logo: v0001 match=12.34% threshold=0%\n", want: 12.34},
		{name: "case and spaces", output: "MANAGED   LOGO: v42 match=100% threshold=20%", want: 100},
		{name: "missing", output: "logoframe completed", wantErr: true},
		{name: "malformed", output: "managed logo: v0001 match=unknown% threshold=0%", wantErr: true},
		{name: "out of range", output: "managed logo: v0001 match=101.00% threshold=0%", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cmLogoMatchPercent([]byte(tt.output))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("cmLogoMatchPercent = %v, want error", got)
				}
				return
			}
			testutil.RequireNoError(t, err, "cmLogoMatchPercent")
			if got != tt.want {
				t.Errorf("cmLogoMatchPercent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStationLogoFilesUseLogoframeLatestConvention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logo := []byte("LGD fixture")
	testutil.RequireNoError(t, writeStationLogo(dir, "n1-s2", logo), "writeStationLogo")
	got, err := readStationLogo(dir, "n1-s2")
	testutil.RequireNoError(t, err, "readStationLogo")
	if string(got) != string(logo) {
		t.Fatalf("logo bytes = %q, want %q", got, logo)
	}
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(dir, "n1-s2.latest"), []byte("../outside.lgd\n"), 0o600), "writing test file")
	if _, err := readStationLogo(dir, "n1-s2"); err == nil {
		t.Fatal("unsafe latest pointer must fail")
	}
}

// 枠を教えただけでは、その局で失敗していた録画は再検出の候補に戻らない（ロゴは変わらない）。
// 枠あり・ロゴなしの局でも新しい録画は desired から外れず（候補が既にあっても）、検出ジョブが
// 書く adopt の attempt で止まる。採用（learned_at）で初めて失敗録画が戻る。
func TestCMDetectionDesiredAfterTaughtLogoArea(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()
	seed := func(eventID int32) int64 {
		id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
			fixtureParams.NetworkID = 32736
			fixtureParams.ServiceID = 1024
			fixtureParams.EventID = eventID
		})
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d.ts", id), []byte("ts"))
		dbtest.ProcessingPolicy(t, pool, id, func(p *dbtest.ProcessingPolicyParams) { p.CMDetect = true })
		return id
	}
	id := seed(830)
	fail := func(t *testing.T, id int64, stage *string) {
		t.Helper()
		message := "failed"
		attemptCount := startCMDetectionTestAttempt(t, ctx, q, id)
		markCMDetectionTestFailure(t, ctx, pool, q, id, attemptCount, "failed", stage, &message)
	}
	wantDesired := func(t *testing.T, id int64, want bool) {
		t.Helper()
		desired, err := q.IsCMDetectionDesired(ctx, id)
		testutil.RequireNoError(t, err, "q.IsCMDetectionDesired")
		if desired != want {
			t.Fatalf("IsCMDetectionDesired(%d) = %v, want %v", id, desired, want)
		}
		ids, err := q.ListMissingCMDetections(ctx, 100)
		testutil.RequireNoError(t, err, "q.ListMissingCMDetections")
		listed := false
		for _, got := range ids {
			listed = listed || got.RecordingID == id
		}
		if listed != want {
			t.Fatalf("ListMissingCMDetections = %v, want recording %d listed = %v", ids, id, want)
		}
	}
	fail(t, id, nil)
	wantDesired(t, id, false)

	testutil.RequireNoError(t, q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: 32736, ServiceID: 1024, X: 1180, Y: 24, W: 240, H: 96,
		CodedWidth: 1440, CodedHeight: 1080,
	}), "q.UpsertCMLogoArea")
	wantDesired(t, id, false) // 枠だけでは再投入しない

	// 候補が存在しても、枠あり・ロゴなしの局の新しい録画は desired のまま（ingest ヒントも
	// Work 冒頭の再評価も IsCMDetectionDesired を通る）。外すと adopt の attempt が書かれず、
	// pending にも failed にも出ない見えない停止になる。
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32736, ServiceID: 1024})
	testutil.RequireNoError(t, err, "q.GetCMLogoArea")
	testutil.MustExec(t, pool, "inserting a ready logo candidate", `
		INSERT INTO cm_logo_candidates (
			network_id, service_id, state, x, y, w, h, coded_width, coded_height,
			recording_id, observed_area_updated_at, lgd
		) VALUES (32736, 1024, 'ready', 1180, 24, 240, 96, 1440, 1080, $1, $2, 'lgd')`, id, area.UpdatedAt)
	fresh := seed(831)
	wantDesired(t, fresh, true)

	// adopt の attempt が書かれた後は、候補が無くなっても再投入しない（reconcile が書き直さない）。
	adopt := "adopt"
	fail(t, fresh, &adopt)
	wantDesired(t, fresh, false)
	testutil.MustExec(t, pool, "clearing cm_logo_candidates", `DELETE FROM cm_logo_candidates`)
	wantDesired(t, fresh, false)

	// 採用でロゴができると、失敗していた録画（adopt 待ちを含む）が desired に戻る。
	testutil.RequireNoError(t, q.UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: 32736, ServiceID: 1024, Lgd: []byte("lgd"), LearnedFrom: &id, CodedWidth: 1440, CodedHeight: 1080,
	}), "q.UpsertCMLogo")
	wantDesired(t, id, true)
	wantDesired(t, fresh, true)
}

func TestUntilEncodedViewWaitsForCMDetectionOrFinalFailure(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()
	id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
		fixtureParams.NetworkID = 32736
		fixtureParams.ServiceID = 1024
		fixtureParams.EventID = 901
	})
	seedUntilEncodedAssets(t, pool, mediaDir, id)
	dbtest.ProcessingPolicy(t, pool, id, func(p *dbtest.ProcessingPolicyParams) {
		p.KeepOriginal = "until_encoded"
		p.EncodeProfiles = []string{"h264"}
		p.CMDetect = true
	})
	count := func() int {
		t.Helper()
		var n int
		testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM until_encoded_deletable_originals('test-store') WHERE recording_id = $1`, id).Scan(&n), "counting until_encoded-eligible originals")
		return n
	}
	if got := count(); got != 0 {
		t.Fatalf("eligible originals before CM completion = %d, want 0", got)
	}
	attemptCount := startCMDetectionTestAttempt(t, ctx, q, id)
	markCMDetectionTestFailure(t, ctx, pool, q, id, attemptCount, "retrying", nil, nil)
	if got := count(); got != 0 {
		t.Fatalf("eligible originals while retrying = %d, want 0", got)
	}
	testutil.MustExec(t, pool, "marking attempt failed", `UPDATE recording_cm_attempts SET state = 'failed' WHERE recording_id = $1`, id)
	if got := count(); got != 1 {
		t.Fatalf("eligible originals after final failure = %d, want 1", got)
	}
	testutil.RequireNoError(t, q.ClearRecordingCMDetection(ctx, id), "q.ClearRecordingCMDetection")
	if rows, err := q.SetRecordingCMDetection(ctx, id); err != nil || rows != 1 {
		t.Fatalf("SetRecordingCMDetection(%d) = %d, %v; want 1 row", id, rows, err)
	}
	if got := count(); got != 0 {
		t.Fatalf("eligible originals after quick re-enable with an old failure = %d, want 0", got)
	}
	testutil.RequireNoError(t, q.DeleteCMDetectionAttempt(ctx, id), "q.DeleteCMDetectionAttempt")
	testutil.RequireNoError(t, q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: id, CmRanges: "{}"}), "q.SaveCMDetection")
	if got := count(); got != 1 {
		t.Fatalf("eligible originals after zero-CM successful result = %d, want 1", got)
	}
}

func TestReadStationLogoLatestFormats(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, latest string
		ok           bool
	}{
		{"two lines (logoframe)", "1\nn1-s2-v0001.lgd\n", true},
		{"one line (rokuban)", "n1-s2-v0001.lgd\n", true},
		{"version only", "1\n", false},
		{"traversal in name line", "1\n../outside.lgd\n", false},
		{"not lgd", "1\nn1-s2-v0001.txt\n", false},
		{"empty", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			testutil.RequireNoError(t, os.WriteFile(filepath.Join(dir, "n1-s2-v0001.lgd"), []byte("LGD"), 0o600), "writing test file")
			testutil.RequireNoError(t, os.WriteFile(filepath.Join(dir, "n1-s2.latest"), []byte(tt.latest), 0o600), "writing test file")
			got, err := readStationLogo(dir, "n1-s2")
			if (err == nil) != tt.ok || (tt.ok && string(got) != "LGD") {
				t.Fatalf("got %q, %v; want ok=%v", got, err, tt.ok)
			}
		})
	}
}
