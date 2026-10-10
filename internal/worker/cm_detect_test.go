package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
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

func markCMDetectionTestFailure(
	t *testing.T,
	ctx context.Context,
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
	})
	if err != nil || n != 1 {
		t.Fatalf("MarkCMDetectionFailure(%d, %d) = %d, %v", recordingID, attemptCount, n, err)
	}
}

func TestCMFrameTimeConversionsUse30000Over1001(t *testing.T) {
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
	got, err := cmRangesFromCutAVS("Trim(150,269) ++ Trim(30,89)", 10010)
	if err != nil {
		t.Fatal(err)
	}
	want := []frameRange{{start: 0, end: 30}, {start: 90, end: 150}, {start: 270, end: 300}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CM frame ranges = %#v, want %#v", got, want)
	}
	if got := encodeInt8Multirange(got, 10010); got != "{[0,1001),[3003,5005),[9009,10010)}" {
		t.Errorf("encoded ranges = %s", got)
	}
}

func TestCMRangesFromCutAVSNoCommercialsAndInvalidOutput(t *testing.T) {
	got, err := cmRangesFromCutAVS("Trim(0,299)", 10010)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || encodeInt8Multirange(got, 10010) != "{}" {
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
	for _, tt := range []struct {
		durationMs int64
		want       time.Duration
	}{
		{durationMs: 0, want: 30 * time.Minute},
		{durationMs: 10 * 60 * 1000, want: 30 * time.Minute},
		{durationMs: 20 * 60 * 1000, want: 40 * time.Minute},
	} {
		if got := cmDetectionTimeout(tt.durationMs); got != tt.want {
			t.Errorf("cmDetectionTimeout(%d) = %s, want %s", tt.durationMs, got, tt.want)
		}
	}
}

func TestCMWorkerTimeoutsUseDurationFromJobArgs(t *testing.T) {
	cmJob := cmJob(1, 1)
	cmJob.Args.RecordingDurationMs = 45 * 60 * 1000
	if got := (&CMDetectWorker{}).Timeout(cmJob); got != 90*time.Minute {
		t.Errorf("CMDetectWorker.Timeout = %s, want 90m", got)
	}
	logoJob := cmLogoCandidateJob(1, 2, time.Now())
	logoJob.Args.RecordingDurationMs = 45 * 60 * 1000
	if got := (&CMLogoCandidateWorker{}).Timeout(logoJob); got != 90*time.Minute {
		t.Errorf("CMLogoCandidateWorker.Timeout = %s, want 90m", got)
	}
	logoJob.Args.RecordingDurationMs = 0 // Old queued args omit this field.
	if got := (&CMLogoCandidateWorker{}).Timeout(logoJob); got != 30*time.Minute {
		t.Errorf("old CMLogoCandidateJobArgs timeout = %s, want 30m", got)
	}
}

func TestCMRecordingDurationQueriesUseStartedAndEndedAt(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := seedCMRecording(t, pool, t.TempDir(), 1101)
	startedAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(45 * time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE recordings SET started_at = $2, ended_at = $3, program_duration_ms = 1 WHERE id = $1`, id, startedAt, endedAt); err != nil {
		t.Fatal(err)
	}
	q := sqlcgen.New(pool)
	duration, err := q.GetCMRecordingDuration(ctx, id)
	if err != nil || duration != 45*60*1000 {
		t.Fatalf("GetCMRecordingDuration = %d, %v; want 2700000", duration, err)
	}
	detections, err := q.ListMissingCMDetections(ctx, sqlcgen.ListMissingCMDetectionsParams{RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(detections) != 1 || detections[0].RecordingID != id || detections[0].RecordingDurationMs != duration {
		t.Errorf("ListMissingCMDetections = %#v, want recording %d at %dms", detections, id, duration)
	}
	seedTaughtArea(t, pool, 1180)
	candidates, err := q.ListMissingCMLogoCandidates(ctx, sqlcgen.ListMissingCMLogoCandidatesParams{RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RecordingID != id || candidates[0].RecordingDurationMs != duration {
		t.Errorf("ListMissingCMLogoCandidates = %#v, want recording %d at %dms", candidates, id, duration)
	}
	if _, err := pool.Exec(ctx, `UPDATE recordings SET started_at = NULL, ended_at = NULL, program_duration_ms = 2400000 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if duration, err := q.GetCMRecordingDuration(ctx, id); err != nil || duration != 2400000 {
		t.Errorf("duration with unconfirmed recording times = %d, %v; want program_duration_ms 2400000", duration, err)
	}
	detections, err = q.ListMissingCMDetections(ctx, sqlcgen.ListMissingCMDetectionsParams{RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(detections) != 1 || detections[0].RecordingID != id || detections[0].RecordingDurationMs != 2400000 {
		t.Errorf("ListMissingCMDetections with unconfirmed times = %#v, want recording %d at 2400000ms (program_duration_ms fallback)", detections, id)
	}
	candidates, err = q.ListMissingCMLogoCandidates(ctx, sqlcgen.ListMissingCMLogoCandidatesParams{RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RecordingID != id || candidates[0].RecordingDurationMs != 2400000 {
		t.Errorf("ListMissingCMLogoCandidates with unconfirmed times = %#v, want recording %d at 2400000ms (program_duration_ms fallback)", candidates, id)
	}
}

func TestCMFailureStagePreservesWorkerObservation(t *testing.T) {
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
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("cmLogoMatchPercent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStationLogoFilesUseLogoframeLatestConvention(t *testing.T) {
	dir := t.TempDir()
	logo := []byte("LGD fixture")
	if err := writeStationLogo(dir, "n1-s2", logo); err != nil {
		t.Fatal(err)
	}
	got, err := readStationLogo(dir, "n1-s2")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(logo) {
		t.Fatalf("logo bytes = %q, want %q", got, logo)
	}
	if err := os.WriteFile(filepath.Join(dir, "n1-s2.latest"), []byte("../outside.lgd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readStationLogo(dir, "n1-s2"); err == nil {
		t.Fatal("unsafe latest pointer must fail")
	}
}

func TestCMDetectionDesiredPredicateAndFreshLogoReset(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()
	ids := make([]int64, 7)
	for i := range ids {
		ids[i] = insertTestRecordingWithEventID(t, pool, int32(800+i))
		seedOriginalAsset(t, pool, mediaDir, ids[i], fmt.Sprintf("cm/%d.ts", ids[i]), []byte("ts"))
		if _, err := pool.Exec(ctx, `
			INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
			VALUES ($1, 'always', '{}', true)`, ids[i]); err != nil {
			t.Fatalf("enabling CM detection for %d: %v", ids[i], err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE recording_encode_policy SET cm_detect = false WHERE recording_id = $1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO missing_media_assets (media_asset_id)
		SELECT id FROM media_assets WHERE recording_id = $1 AND kind = 'original'`, ids[3]); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: ids[4], CmRanges: "{}"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{ids[5], ids[6]} {
		attemptCount := startCMDetectionTestAttempt(t, ctx, q, id)
		state := "failed"
		if id == ids[6] {
			state = "retrying"
		}
		markCMDetectionTestFailure(t, ctx, q, id, attemptCount, state, nil, nil)
	}
	if _, err := pool.Exec(ctx, `UPDATE recording_cm_attempts SET attempted_at = now() - interval '1 hour' WHERE recording_id = $1`, ids[5]); err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{ids[0], ids[6]} {
		desired, err := q.IsCMDetectionDesired(ctx, id)
		if err != nil || !desired {
			t.Errorf("IsCMDetectionDesired(%d) = %v, %v; want true", id, desired, err)
		}
	}
	for _, id := range []int64{ids[1], ids[2], ids[3], ids[4], ids[5]} {
		desired, err := q.IsCMDetectionDesired(ctx, id)
		if err != nil || desired {
			t.Errorf("IsCMDetectionDesired(%d) = %v, %v; want false", id, desired, err)
		}
	}

	logo := sqlcgen.UpsertCMLogoParams{
		NetworkID: 32736, ServiceID: 1024, Lgd: []byte("lgd"), LearnedFrom: &ids[5], CodedWidth: 1440, CodedHeight: 1080,
	}
	if err := q.UpsertCMLogo(ctx, logo); err != nil {
		t.Fatal(err)
	}
	desired, err := q.IsCMDetectionDesired(ctx, ids[5])
	if err != nil || !desired {
		t.Errorf("failed recording with a newly learned station logo: desired = %v, err = %v; want true", desired, err)
	}
	updatedAttempt, err := q.BeginCMDetectionAttempt(ctx, sqlcgen.BeginCMDetectionAttemptParams{
		RecordingID: ids[5], MaxAttempts: cmDetectMaxTries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updatedAttempt.ShouldRun || updatedAttempt.AttemptCount != 1 {
		t.Errorf("BeginCMDetectionAttempt after a new logo = %#v, want running attempt 1 (a new desire starts a new budget)", updatedAttempt)
	}
	if err := q.DeleteCMDetectionAttempt(ctx, ids[5]); err != nil {
		t.Fatal(err)
	}
	rows, err := q.ListMissingCMDetections(ctx, sqlcgen.ListMissingCMDetectionsParams{AfterRecordingID: 0, RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{ids[0], ids[5], ids[6]}
	got := make([]int64, len(rows))
	for i := range rows {
		got[i] = rows[i].RecordingID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing CM detection IDs = %v, want %v", got, want)
	}
}

// 枠を教えただけでは、その局で失敗していた録画は再検出の候補に戻らない（ロゴは変わらない）。
// 枠あり・ロゴなしの局でも新しい録画は desired から外れず（候補が既にあっても）、検出ジョブが
// 書く adopt の attempt で止まる。採用（learned_at）で初めて失敗録画が戻る。
func TestCMDetectionDesiredAfterTaughtLogoArea(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()
	seed := func(eventID int32) int64 {
		id := insertTestRecordingWithEventID(t, pool, eventID)
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d.ts", id), []byte("ts"))
		if _, err := pool.Exec(ctx, `
			INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
			VALUES ($1, 'always', '{}', true)`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := seed(830)
	fail := func(t *testing.T, id int64, stage *string) {
		t.Helper()
		message := "failed"
		attemptCount := startCMDetectionTestAttempt(t, ctx, q, id)
		markCMDetectionTestFailure(t, ctx, q, id, attemptCount, "failed", stage, &message)
	}
	wantDesired := func(t *testing.T, id int64, want bool) {
		t.Helper()
		desired, err := q.IsCMDetectionDesired(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if desired != want {
			t.Fatalf("IsCMDetectionDesired(%d) = %v, want %v", id, desired, want)
		}
		ids, err := q.ListMissingCMDetections(ctx, sqlcgen.ListMissingCMDetectionsParams{AfterRecordingID: 0, RowLimit: 100})
		if err != nil {
			t.Fatal(err)
		}
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

	if err := q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: 32736, ServiceID: 1024, X: 1180, Y: 24, W: 240, H: 96,
		CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	wantDesired(t, id, false) // 枠だけでは再投入しない

	// 候補が存在しても、枠あり・ロゴなしの局の新しい録画は desired のまま（ingest ヒントも
	// Work 冒頭の再評価も IsCMDetectionDesired を通る）。外すと adopt の attempt が書かれず、
	// pending にも failed にも出ない見えない停止になる。
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32736, ServiceID: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cm_logo_candidates (
			network_id, service_id, state, x, y, w, h, coded_width, coded_height,
			recording_id, observed_area_updated_at, lgd
		) VALUES (32736, 1024, 'ready', 1180, 24, 240, 96, 1440, 1080, $1, $2, 'lgd')`, id, area.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	fresh := seed(831)
	wantDesired(t, fresh, true)

	// adopt の attempt が書かれた後は、候補が無くなっても再投入しない（reconcile が書き直さない）。
	adopt := "adopt"
	fail(t, fresh, &adopt)
	wantDesired(t, fresh, false)
	if _, err := pool.Exec(ctx, `DELETE FROM cm_logo_candidates`); err != nil {
		t.Fatal(err)
	}
	wantDesired(t, fresh, false)

	// 採用でロゴができると、失敗していた録画（adopt 待ちを含む）が desired に戻る。
	if err := q.UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: 32736, ServiceID: 1024, Lgd: []byte("lgd"), LearnedFrom: &id, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	wantDesired(t, id, true)
	wantDesired(t, fresh, true)
}

func TestUntilEncodedViewWaitsForCMDetectionOrFinalFailure(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()
	id := insertTestRecordingWithEventID(t, pool, 901)
	assetID := seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d.ts", id), []byte("ts"))
	markOriginalTSScanComplete(t, pool, assetID)
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindEncoded, &profile, fmt.Sprintf("cm/%d-h264.mp4", id), []byte("encoded"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindThumbnail, nil, fmt.Sprintf("cm/%d.jpg", id), []byte("thumbnail"))
	seedSeekTilesAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d-tiles", id))
	if _, err := pool.Exec(ctx, `
		INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
		VALUES ($1, 'until_encoded', ARRAY['h264'], true)`, id); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM until_encoded_deletable_originals WHERE recording_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(); got != 0 {
		t.Fatalf("eligible originals before CM completion = %d, want 0", got)
	}
	attemptCount := startCMDetectionTestAttempt(t, ctx, q, id)
	markCMDetectionTestFailure(t, ctx, q, id, attemptCount, "retrying", nil, nil)
	if got := count(); got != 0 {
		t.Fatalf("eligible originals while retrying = %d, want 0", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE recording_cm_attempts SET state = 'failed' WHERE recording_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Fatalf("eligible originals after final failure = %d, want 1", got)
	}
	if err := q.DeleteCMDetectionAttempt(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: id, CmRanges: "{}"}); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Fatalf("eligible originals after zero-CM successful result = %d, want 1", got)
	}
}

func TestReadStationLogoLatestFormats(t *testing.T) {
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
			if err := os.WriteFile(filepath.Join(dir, "n1-s2-v0001.lgd"), []byte("LGD"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "n1-s2.latest"), []byte(tt.latest), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readStationLogo(dir, "n1-s2")
			if (err == nil) != tt.ok || (tt.ok && string(got) != "LGD") {
				t.Fatalf("got %q, %v; want ok=%v", got, err, tt.ok)
			}
		})
	}
}

func TestUntilEncodedViewKeepsOriginalWhileLogoAdoptionIsPending(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()
	id := insertTestRecordingWithEventID(t, pool, 902)
	assetID := seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d.ts", id), []byte("ts"))
	markOriginalTSScanComplete(t, pool, assetID)
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindEncoded, &profile, fmt.Sprintf("cm/%d-h264.mp4", id), []byte("encoded"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindThumbnail, nil, fmt.Sprintf("cm/%d.jpg", id), []byte("thumbnail"))
	seedSeekTilesAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d-tiles", id))
	if _, err := pool.Exec(ctx, `
		INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
		VALUES ($1, 'until_encoded', ARRAY['h264'], true)`, id); err != nil {
		t.Fatal(err)
	}
	attemptCount := startCMDetectionTestAttempt(t, ctx, q, id)
	stage := "adopt"
	markCMDetectionTestFailure(t, ctx, q, id, attemptCount, "failed", &stage, nil)
	var eligible int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM until_encoded_deletable_originals WHERE recording_id = $1`, id).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	if eligible != 0 {
		t.Fatalf("adoption-waiting original is eligible = %d, want 0", eligible)
	}
	stage = "logo"
	if _, err := pool.Exec(ctx, `UPDATE recording_cm_attempts SET stage = 'logo' WHERE recording_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM until_encoded_deletable_originals WHERE recording_id = $1`, id).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	if eligible != 1 {
		t.Fatalf("ordinary failed original is eligible = %d, want 1", eligible)
	}
}
