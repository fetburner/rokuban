package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCMRangesFromCutAVSAviSynthTrimForms(t *testing.T) {
	for _, tt := range []struct {
		name string
		avs  string
		want []frameRange
	}{
		{name: "explicit inclusive end", avs: "Trim(30,89)", want: []frameRange{{0, 30}, {90, 300}}},
		{name: "zero end means to the last frame", avs: "Trim(30,0)", want: []frameRange{{0, 30}}},
		{name: "negative end is a frame count", avs: "Trim(30,-60)", want: []frameRange{{0, 30}, {90, 300}}},
		{name: "whole clip", avs: "Trim(0,0)", want: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cmRangesFromCutAVS(tt.avs, 10010)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ranges = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// buildTestLGD は logo.h の形式（32 byte ヘッダ + LOGO_HEADER + LOGO_PIXEL[h*w]）で .lgd を作る。
func buildTestLGD(w, h int, opacity, y int16) []byte {
	var buf bytes.Buffer
	header := make([]byte, 28)
	copy(header, "<logo data file ver0.1>")
	buf.Write(header)
	_ = binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.Write(make([]byte, 32)) // name
	for _, v := range []int16{0, 0, int16(h), int16(w), 0, 0, 0, 0} {
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}
	for i := 0; i < w*h; i++ {
		for _, v := range []int16{opacity, y, opacity, 0, opacity, 0} {
			_ = binary.Write(&buf, binary.LittleEndian, v)
		}
	}
	return buf.Bytes()
}

func TestLGDPreviewPNG(t *testing.T) {
	data, err := lgdPreviewPNG(buildTestLGD(3, 2, 1000, 4080))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 3 || b.Dy() != 2 {
		t.Fatalf("preview size = %v, want 3x2", b)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0xffff {
		t.Errorf("alpha of a fully opaque pixel = %#x, want 0xffff", a)
	}
	transparent, err := lgdPreviewPNG(buildTestLGD(1, 1, 0, 4080))
	if err != nil {
		t.Fatal(err)
	}
	img, _ = png.Decode(bytes.NewReader(transparent))
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Errorf("alpha of a transparent pixel = %#x, want 0", a)
	}

	valid := buildTestLGD(3, 2, 1000, 4080)
	for name, broken := range map[string][]byte{
		"empty":     nil,
		"truncated": valid[:len(valid)-1],
		"zero size": buildTestLGD(0, 0, 0, 0),
	} {
		if got, err := lgdPreviewPNG(broken); err == nil || got != nil {
			t.Errorf("%s: lgdPreviewPNG = %d bytes, %v; want error", name, len(got), err)
		}
	}
}

// cmToolset は BinaryDir に置くダミー実行ファイルと ffprobe の場所を返す。
type cmToolset struct {
	binDir  string
	ffprobe string
	// logoframeArgs は logoframe のダミーが受け取った引数を 1 行ずつ書くファイル。
	logoframeArgs string
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// newFakeCMTools は JLSE の 3 バイナリと ffprobe の代役を作る。logoframe は未学習の局なら
// lgd を学習して logo-dir に書き、chapter_exe は chapterExit で終了する。
// join_logo_scp は cutAVS をそのまま obs_cut.avs に書く。ffprobe は videoSeconds を返す。
func newFakeCMTools(t *testing.T, lgd []byte, chapterExit int, cutAVS, videoSeconds string) cmToolset {
	t.Helper()
	return newFakeCMToolsWithSize(t, lgd, chapterExit, cutAVS, videoSeconds, "1440x1080")
}

// newFakeCMToolsWithSize は記録上の大きさ（ffprobe の stream=width,height の答え）を
// 指定できる版。CMDetectWorker は logoframe の前に大きさを 1 回引く。
func newFakeCMToolsWithSize(t *testing.T, lgd []byte, chapterExit int, cutAVS, videoSeconds, size string) cmToolset {
	t.Helper()
	dir := t.TempDir()
	lgdPath := filepath.Join(dir, "fixture.lgd")
	if err := os.WriteFile(lgdPath, lgd, 0o600); err != nil {
		t.Fatal(err)
	}
	// logoframe は渡された引数を 1 行ずつ argsPath に残す（呼び出し側が何を渡したかを
	// テストから読めるようにする）。作業ディレクトリは実行後に消えるので外に置く。
	argsPath := filepath.Join(dir, "logoframe-args.txt")
	writeExecutable(t, filepath.Join(dir, "logoframe"), fmt.Sprintf(`
: > %q
while [ $# -gt 0 ]; do
  echo "$1" >> %q
  case "$1" in -channel) ch=$2;; -logo-dir) d=$2;; -oa) o=$2;; esac
  shift
done
: > "$o"
if [ ! -f "$d/$ch.latest" ]; then
  cp %q "$d/$ch-v0001.lgd"
  echo "$ch-v0001.lgd" > "$d/$ch.latest"
fi
`, argsPath, argsPath, lgdPath))
	writeExecutable(t, filepath.Join(dir, "chapter_exe"), fmt.Sprintf(`
while [ $# -gt 0 ]; do
  case "$1" in -o) o=$2;; esac
  shift
done
: > "$o"
exit %d
`, chapterExit))
	writeExecutable(t, filepath.Join(dir, "join_logo_scp"), fmt.Sprintf(`
while [ $# -gt 0 ]; do
  case "$1" in -o) o=$2;; esac
  shift
done
echo %q > "$o"
`, cutAVS))
	writeExecutable(t, filepath.Join(dir, "ffprobe"), fmt.Sprintf(`
case "$*" in
  *width,height*) echo %s ;;
  *) echo %s ;;
esac
`, size, videoSeconds))
	return cmToolset{binDir: dir, ffprobe: filepath.Join(dir, "ffprobe"), logoframeArgs: argsPath}
}

// logoframeSawArea は logoframe に渡された -logo-area の値（無ければ空文字）を返す。
func logoframeSawArea(t *testing.T, tools cmToolset) string {
	t.Helper()
	data, err := os.ReadFile(tools.logoframeArgs)
	if err != nil {
		t.Fatalf("reading logoframe arguments: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i, arg := range args {
		if arg == "-logo-area" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func newCMDetectTestWorker(pool *pgxpool.Pool, mediaDir string, tools cmToolset) *CMDetectWorker {
	return &CMDetectWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: filepath.Join(mediaDir, "scratch"),
		CMDetect: config.CMDetectConfig{Enabled: true, BinaryDir: tools.binDir},
		FFprobe:  tools.ffprobe,
	}
}

func seedCMRecording(t *testing.T, pool *pgxpool.Pool, mediaDir string, eventID int32) int64 {
	t.Helper()
	id := insertTestRecordingWithEventID(t, pool, eventID)
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET program_duration_ms = 10010 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d.ts", id), []byte("ts"))
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
		VALUES ($1, 'until_encoded', ARRAY['h264'], true)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func cmJob(recordingID int64, attempt int) *river.Job[jobs.CMDetectJobArgs] {
	return &river.Job[jobs.CMDetectJobArgs]{
		JobRow: &rivertype.JobRow{ID: 4242, Attempt: attempt, MaxAttempts: 3},
		Args:   jobs.CMDetectJobArgs{RecordingID: recordingID},
	}
}

func TestCMDetectWorkKeepsCommercialsBeyondProgramDurationAndStoresLogoPreview(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	// EPG の尺は 10.010 秒（300 フレーム）だが、録画は延長されて原本は 20.020 秒（600 フレーム）ある。
	id := seedCMRecording(t, pool, mediaDir, 910)
	tools := newFakeCMTools(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "20.020000")

	if err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	var ranges string
	if err := pool.QueryRow(ctx, `SELECT cm_ranges::text FROM recording_cm_detections WHERE recording_id = $1`, id).Scan(&ranges); err != nil {
		t.Fatalf("detection row: %v", err)
	}
	if ranges != "{[10010,20020)}" {
		t.Errorf("cm_ranges = %s, want {[10010,20020)} (the extension beyond the EPG duration)", ranges)
	}
	var preview []byte
	var lgd []byte
	if err := pool.QueryRow(ctx, `SELECT lgd, preview_png FROM cm_logos WHERE network_id = 32736 AND service_id = 1024`).Scan(&lgd, &preview); err != nil {
		t.Fatalf("logo row: %v", err)
	}
	if !bytes.Equal(lgd, buildTestLGD(4, 3, 1000, 4080)) {
		t.Errorf("stored lgd differs from the learned one")
	}
	if _, err := png.Decode(bytes.NewReader(preview)); err != nil {
		t.Errorf("preview_png is not a PNG: %v", err)
	}
}

func TestCMDetectWorkSavesLogoWhenPreviewCannotBeRendered(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 911)
	tools := newFakeCMTools(t, []byte("not an lgd"), 0, "Trim(0,299)", "10.010000")

	if err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	var lgd, preview []byte
	if err := pool.QueryRow(ctx, `SELECT lgd, preview_png FROM cm_logos WHERE network_id = 32736 AND service_id = 1024`).Scan(&lgd, &preview); err != nil {
		t.Fatalf("logo row: %v", err)
	}
	if string(lgd) != "not an lgd" || preview != nil {
		t.Errorf("logo = %q preview = %d bytes, want the lgd kept and no preview", lgd, len(preview))
	}
}

// 人が教えた枠は logoframe の -logo-area に渡る（記録上の解像度の座標）。
func TestCMDetectWorkPassesTaughtLogoAreaToLogoframe(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 920)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000", "1440x1080")
	if _, err := pool.Exec(ctx, `
		INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
		VALUES (32736, 1024, 1180, 24, 240, 96, 1440, 1080)`); err != nil {
		t.Fatal(err)
	}

	if err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := logoframeSawArea(t, tools); got != "1180,24,240,96" {
		t.Errorf("-logo-area = %q, want the taught 1180,24,240,96", got)
	}
	var detections int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_detections WHERE recording_id = $1`, id).Scan(&detections); err != nil {
		t.Fatal(err)
	}
	if detections != 1 {
		t.Errorf("detections = %d, want 1 (the taught area must not stop the run)", detections)
	}
}

// 教えた枠と記録の解像度が違えば、枠を使わず（logoframe を回さず）失敗として
// 理由を残す。理由は録画詳細と /api/cm-logos の警告に出る。
func TestCMDetectWorkRejectsTaughtAreaWithOtherResolution(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 921)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000", "1920x1080")
	if _, err := pool.Exec(ctx, `
		INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
		VALUES (32736, 1024, 1180, 24, 240, 96, 1440, 1080)`); err != nil {
		t.Fatal(err)
	}

	err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(id, 3))
	if err == nil {
		t.Fatal("Work succeeded although the taught area is for another resolution")
	}
	for _, want := range []string{"1440x1080", "1920x1080"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}
	if _, statErr := os.Stat(tools.logoframeArgs); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("logoframe ran (stat %v); the mismatched area must not be used", statErr)
	}
	var state string
	var message *string
	if err := pool.QueryRow(ctx, `SELECT state, error FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &message); err != nil {
		t.Fatalf("attempt row: %v", err)
	}
	if state != "failed" || message == nil || !strings.Contains(*message, "1920x1080") {
		t.Errorf("attempt = %q / %v, want failed with the resolution reason", state, message)
	}
	for _, query := range []string{
		`SELECT count(*) FROM recording_cm_detections`,
		`SELECT count(*) FROM cm_logos`,
	} {
		var n int
		if err := pool.QueryRow(ctx, query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s = %d rows, want 0", query, n)
		}
	}
}

func TestCMDetectWorkFailureWritesNoResultAndMarksAttempt(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 912)
	tools := newFakeCMTools(t, buildTestLGD(2, 2, 1000, 4080), 3, "Trim(0,299)", "10.010000")
	w := newCMDetectTestWorker(pool, mediaDir, tools)

	for _, tt := range []struct {
		attempt   int
		wantState string
	}{{1, "retrying"}, {3, "failed"}} {
		if err := w.Work(ctx, cmJob(id, tt.attempt)); err == nil {
			t.Fatalf("attempt %d: Work succeeded although chapter_exe exits 3", tt.attempt)
		}
		var detections int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_detections WHERE recording_id = $1`, id).Scan(&detections); err != nil {
			t.Fatal(err)
		}
		if detections != 0 {
			t.Fatalf("attempt %d: failure left %d result rows, want 0", tt.attempt, detections)
		}
		var state string
		var message *string
		if err := pool.QueryRow(ctx, `SELECT state, error FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &message); err != nil {
			t.Fatalf("attempt %d: attempt row: %v", tt.attempt, err)
		}
		if state != tt.wantState || message == nil || !strings.Contains(*message, "chapter_exe") {
			t.Errorf("attempt %d: state = %q error = %v, want %q naming chapter_exe", tt.attempt, state, message, tt.wantState)
		}
	}
}

func TestUntilEncodedViewDoesNotWaitForCMDetectionWhenDisabledPerRecording(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := insertTestRecordingWithEventID(t, pool, 913)
	seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d.ts", id), []byte("ts"))
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindEncoded, &profile, fmt.Sprintf("cm/%d-h264.mp4", id), []byte("encoded"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, id, db.AssetKindThumbnail, nil, fmt.Sprintf("cm/%d.jpg", id), []byte("thumbnail"))
	seedSeekTilesAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d-tiles", id))
	if _, err := pool.Exec(ctx, `
		INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
		VALUES ($1, 'until_encoded', ARRAY['h264'], false)`, id); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM until_encoded_deletable_originals WHERE recording_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cm_detect=false original without a CM result: eligible rows = %d, want 1", n)
	}
}

func insertCMJobRow(t *testing.T, pool *pgxpool.Pool, recordingID int64, state string, attempt int) int64 {
	t.Helper()
	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Insert(context.Background(), jobs.CMDetectJobArgs{RecordingID: recordingID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE river_job SET state = $2::river_job_state, attempt = $3, max_attempts = 3,
		       attempted_at = now() - interval '1 hour'
		WHERE id = $1`, res.Job.ID, state, attempt); err != nil {
		t.Fatal(err)
	}
	return res.Job.ID
}

func TestRecoverStaleCMDetectJobs(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)

	retrying := seedCMRecording(t, pool, mediaDir, 920)  // River のバックオフ中
	midway := seedCMRecording(t, pool, mediaDir, 921)    // 途中の試行で死んだ
	exhausted := seedCMRecording(t, pool, mediaDir, 922) // 最終試行で死んだ
	for _, id := range []int64{retrying, midway, exhausted} {
		if err := q.MarkCMDetectionRunning(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.MarkCMDetectionFailure(ctx, sqlcgen.MarkCMDetectionFailureParams{RecordingID: retrying, State: "retrying"}); err != nil {
		t.Fatal(err)
	}
	retryingJob := insertCMJobRow(t, pool, retrying, "retryable", 1)
	midwayJob := insertCMJobRow(t, pool, midway, "running", 1)
	exhaustedJob := insertCMJobRow(t, pool, exhausted, "running", 3)

	if err := recoverStaleCMDetectJobs(ctx, pool); err != nil {
		t.Fatal(err)
	}

	jobState := func(id int64) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT state::text FROM river_job WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	attemptState := func(id int64) (string, string) {
		var s string
		var e *string
		if err := pool.QueryRow(ctx, `SELECT state, error FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&s, &e); err != nil {
			t.Fatal(err)
		}
		if e == nil {
			return s, ""
		}
		return s, *e
	}

	var recovered bool
	if err := pool.QueryRow(ctx, `SELECT metadata ? 'cm_detect_recovery' FROM river_job WHERE id = $1`, retryingJob).Scan(&recovered); err != nil {
		t.Fatal(err)
	}
	if got := jobState(retryingJob); got != "retryable" || recovered {
		t.Errorf("job in River backoff = %q (recovered=%v), want untouched retryable", got, recovered)
	}
	if s, _ := attemptState(retrying); s != "retrying" {
		t.Errorf("attempt of a backing-off job = %q, want retrying", s)
	}
	if got := jobState(midwayJob); got != "retryable" {
		t.Errorf("dead job with attempts left = %q, want retryable", got)
	}
	if s, _ := attemptState(midway); s != "retrying" {
		t.Errorf("attempt of a dead job with attempts left = %q, want retrying", s)
	}
	if got := jobState(exhaustedJob); got != "discarded" {
		t.Errorf("dead job on its last attempt = %q, want discarded", got)
	}
	if s, e := attemptState(exhausted); s != "failed" || !strings.Contains(e, "process stopped") {
		t.Errorf("attempt of a dead final job = (%q, %q), want failed / process stopped", s, e)
	}
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'cm_detect'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("cm_detect job rows = %d, want 3 (recovery must not insert new jobs)", total)
	}
}
