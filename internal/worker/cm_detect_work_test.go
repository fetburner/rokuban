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
	"time"

	pgx5 "github.com/jackc/pgx/v5"
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
	// hold が存在する間、logoframe のダミーは started を作って止まる。実 logoframe が
	// 長く走っている間に枠の保存が割り込む窓を、テストが作るための足場。
	hold, started string
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
	return newFakeCMToolsWithSizeAndReport(t, lgd, chapterExit, cutAVS, videoSeconds, "1440x1080", "managed logo: v0001 match=90.00% threshold=0%")
}

// newFakeCMToolsWithSize は記録上の大きさ（ffprobe の stream=width,height の答え）を
// 指定できる版。CMDetectWorker は logoframe の前に大きさを 1 回引く。
func newFakeCMToolsWithSize(t *testing.T, lgd []byte, size string) cmToolset {
	t.Helper()
	return newFakeCMToolsWithSizeAndReport(t, lgd, 0, "Trim(0,299)", "10.010000", size, "managed logo: v0001 match=90.00% threshold=0%")
}

// newFakeCMToolsWithSizeAndReport は logoframe の成功出力を差し替えられる版。
// report が空なら一致率の行が無い出力になる。
func newFakeCMToolsWithSizeAndReport(t *testing.T, lgd []byte, chapterExit int, cutAVS, videoSeconds, size, report string) cmToolset {
	t.Helper()
	dir := t.TempDir()
	lgdPath := filepath.Join(dir, "fixture.lgd")
	if err := os.WriteFile(lgdPath, lgd, 0o600); err != nil {
		t.Fatal(err)
	}
	// logoframe は渡された引数を 1 行ずつ argsPath に残す（呼び出し側が何を渡したかを
	// テストから読めるようにする）。作業ディレクトリは実行後に消えるので外に置く。
	argsPath := filepath.Join(dir, "logoframe-args.txt")
	holdPath := filepath.Join(dir, "hold")
	startedPath := filepath.Join(dir, "started")
	writeExecutable(t, filepath.Join(dir, "logoframe"), fmt.Sprintf(`
: > %q
while [ $# -gt 0 ]; do
  echo "$1" >> %q
  case "$1" in -channel) ch=$2;; -logo-dir) d=$2;; -oa) o=$2;; esac
  shift
done
if [ -f %q ]; then
  : > %q
  while [ -f %q ]; do sleep 0.05; done
fi
: > "$o"
if [ ! -f "$d/$ch.latest" ]; then
  cp %q "$d/$ch-v0001.lgd"
  printf '1\n%%s\n' "$ch-v0001.lgd" > "$d/$ch.latest"
fi
echo %q
`, argsPath, argsPath, holdPath, startedPath, holdPath, lgdPath, report))
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
  *width,height*) echo %q ;;
  *) echo %s ;;
esac
`, ffprobeSizeJSON(size), videoSeconds))
	return cmToolset{binDir: dir, ffprobe: filepath.Join(dir, "ffprobe"), logoframeArgs: argsPath, hold: holdPath, started: startedPath}
}

// ffprobeSizeJSON は size（"1440x1080"）を、実 ffprobe 9.0.2 が MPEG-TS に対して
// `-of json` で返す形（programs 側と streams 側の 2 回出る）にする。
func ffprobeSizeJSON(size string) string {
	w, h, _ := strings.Cut(size, "x")
	stream := fmt.Sprintf(`{"width": %s, "height": %s}`, w, h)
	return fmt.Sprintf(`{"programs": [{"streams": [%s]}], "stream_groups": [], "streams": [%s]}`, stream, stream)
}

func newCMDetectTestWorker(pool *pgxpool.Pool, mediaDir string, tools cmToolset) *CMDetectWorker {
	return &CMDetectWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: filepath.Join(mediaDir, "scratch"),
		CMDetect: config.CMDetectConfig{Enabled: true, BinaryDir: tools.binDir},
		FFprobe:  tools.ffprobe,
	}
}

func cmArgumentValue(argsText, name string) (string, bool) {
	args := strings.Split(strings.TrimSpace(argsText), "\n")
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func seedCMRecording(t *testing.T, pool *pgxpool.Pool, mediaDir string, eventID int32) int64 {
	t.Helper()
	id := insertTestRecordingWithEventID(t, pool, eventID)
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET program_duration_ms = 10010 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	assetID := seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d.ts", id), []byte("ts"))
	markOriginalTSScanComplete(t, pool, assetID)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
		VALUES ($1, 'until_encoded', ARRAY['h264'], true)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func cmJob(recordingID int64, attempt int) *river.Job[jobs.CMDetectJobArgs] {
	return &river.Job[jobs.CMDetectJobArgs]{
		JobRow: &rivertype.JobRow{ID: 4242, Attempt: attempt, MaxAttempts: 10},
		Args:   jobs.CMDetectJobArgs{RecordingID: recordingID, RecordingDurationMs: 0},
	}
}

func TestCMDetectWorkRejectsEmptyScratchInsteadOfUsingCurrentDirectory(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 929)
	tools := newFakeCMTools(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000")
	workingDir := t.TempDir()
	t.Chdir(workingDir)

	w := newCMDetectTestWorker(pool, mediaDir, tools)
	w.ScratchDir = ""
	err := w.Work(ctx, cmJob(id, 1))
	if err == nil || !strings.Contains(err.Error(), "scratch dir is empty") {
		args, readErr := os.ReadFile(tools.logoframeArgs)
		t.Fatalf("Work error = %v, want empty-scratch error; logoframe args = %q (read error %v)", err, args, readErr)
	}
	if _, err := os.Stat(filepath.Join(workingDir, "cm-detect")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("current-directory cm-detect scratch stat error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(tools.logoframeArgs); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("logoframe ran with empty scratch (stat error %v)", err)
	}
}

func cmLogoCandidateJob(recordingID, jobID int64, areaUpdatedAt time.Time) *river.Job[jobs.CMLogoCandidateJobArgs] {
	return &river.Job[jobs.CMLogoCandidateJobArgs]{
		JobRow: &rivertype.JobRow{ID: jobID, Attempt: 1, MaxAttempts: 1},
		Args: jobs.CMLogoCandidateJobArgs{
			NetworkID: 32736, ServiceID: 1024, RecordingID: recordingID,
			AreaUpdatedAt: areaUpdatedAt, RecordingDurationMs: 0,
		},
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
	args, err := os.ReadFile(tools.logoframeArgs)
	if err != nil {
		t.Fatalf("reading logoframe args: %v", err)
	}
	logoDir, ok := cmArgumentValue(string(args), "-logo-dir")
	wantLogoDir := filepath.Join(mediaDir, "scratch", "cm-detect", "4242", "logos")
	if !ok || logoDir != wantLogoDir {
		t.Errorf("logoframe -logo-dir = %q (found %v), want job-ID scratch path %q", logoDir, ok, wantLogoDir)
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
	var codedWidth, codedHeight int
	if err := pool.QueryRow(ctx, `SELECT lgd, preview_png, coded_width, coded_height FROM cm_logos WHERE network_id = 32736 AND service_id = 1024`).Scan(&lgd, &preview, &codedWidth, &codedHeight); err != nil {
		t.Fatalf("logo row: %v", err)
	}
	if !bytes.Equal(lgd, buildTestLGD(4, 3, 1000, 4080)) {
		t.Errorf("stored lgd differs from the learned one")
	}
	if _, err := png.Decode(bytes.NewReader(preview)); err != nil {
		t.Errorf("preview_png is not a PNG: %v", err)
	}
	if codedWidth != 1440 || codedHeight != 1080 {
		t.Errorf("coded size = %dx%d, want 1440x1080", codedWidth, codedHeight)
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

// 枠がありロゴが無い局の通常検出は logoframe を走らせず、候補の採用待ちにする。
func TestCMDetectWorkPassesTaughtLogoAreaToLogoframe(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 920)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
	if _, err := pool.Exec(ctx, `
		INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
		VALUES (32736, 1024, 1180, 24, 240, 96, 1440, 1080)`); err != nil {
		t.Fatal(err)
	}

	if err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(id, 1)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if _, err := os.Stat(tools.logoframeArgs); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("logoframe ran (stat %v); a station without an adopted logo must wait for adoption", err)
	}
	var detections int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_detections WHERE recording_id = $1`, id).Scan(&detections); err != nil {
		t.Fatal(err)
	}
	if detections != 0 {
		t.Errorf("detections = %d, want 0 while the candidate is awaiting adoption", detections)
	}
	var state, stage string
	if err := pool.QueryRow(ctx, `SELECT state, stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &stage); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || stage != "adopt" {
		t.Errorf("attempt = %q/%q, want failed/adopt", state, stage)
	}
}

// 通常検出側は枠の解像度を検査せず、候補解析側へ責務を渡す。
func TestCMDetectWorkRejectsTaughtAreaWithOtherResolution(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 921)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1920x1080")
	if _, err := pool.Exec(ctx, `
		INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
		VALUES (32736, 1024, 1180, 24, 240, 96, 1440, 1080)`); err != nil {
		t.Fatal(err)
	}

	if err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(id, 3)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if _, statErr := os.Stat(tools.logoframeArgs); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("logoframe ran (stat %v); the mismatched area must not be used", statErr)
	}
	var state string
	var stage *string
	var message *string
	if err := pool.QueryRow(ctx, `SELECT state, stage, error FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &stage, &message); err != nil {
		t.Fatalf("attempt row: %v", err)
	}
	if state != "failed" || message == nil || !strings.Contains(*message, "adopt") {
		t.Errorf("attempt = %q / %v, want failed with the adoption reason", state, message)
	}
	if stage == nil || *stage != "adopt" {
		t.Errorf("attempt stage = %v, want adopt", stage)
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

func TestCMLogoCandidateWorkerCreatesReadyCandidateFromEmptyLogoDir(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 926)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
	if _, err := pool.Exec(ctx, `
		INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
		VALUES (32736, 1024, 1180, 24, 240, 96, 1440, 1080)`); err != nil {
		t.Fatal(err)
	}
	area, err := sqlcgen.New(pool).GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32736, ServiceID: 1024})
	if err != nil {
		t.Fatal(err)
	}
	w := &CMLogoCandidateWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: filepath.Join(mediaDir, "scratch"),
		CMDetect: config.CMDetectConfig{Enabled: true, BinaryDir: tools.binDir}, FFprobe: tools.ffprobe,
	}
	if err := w.Work(ctx, cmLogoCandidateJob(id, 4343, area.UpdatedAt)); err != nil {
		t.Fatalf("Work: %v", err)
	}
	candidate, err := sqlcgen.New(pool).GetCMLogoCandidate(ctx, sqlcgen.GetCMLogoCandidateParams{NetworkID: 32736, ServiceID: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != "ready" || candidate.Stage != nil || candidate.RecordingID == nil || *candidate.RecordingID != id {
		t.Fatalf("candidate = %#v, want ready candidate for recording %d", candidate, id)
	}
	if !bytes.Equal(candidate.Lgd, buildTestLGD(4, 3, 1000, 4080)) {
		t.Error("candidate LGD differs from the generated logo")
	}
	args, err := os.ReadFile(tools.logoframeArgs)
	if err != nil {
		t.Fatal(err)
	}
	argText := string(args)
	if !strings.Contains(argText, "-logo-area") || strings.Contains(argText, "-seek") || strings.Contains(argText, "-frames") {
		t.Errorf("candidate logoframe args = %q, want area and no seek/frames", argText)
	}
	logoDir, ok := cmArgumentValue(argText, "-logo-dir")
	wantLogoDir := filepath.Join(mediaDir, "scratch", "cm-logo-candidate", "4343", "logos")
	if !ok || logoDir != wantLogoDir {
		t.Errorf("candidate logoframe -logo-dir = %q (found %v), want job-ID scratch path %q", logoDir, ok, wantLogoDir)
	}
	if n := countLogos(t, pool); n != 0 {
		t.Errorf("cm_logos rows = %d, want 0 until adoption", n)
	}
}

func TestCMLogoCandidateWorkerMarksResolutionMismatchFailed(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 927)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1920x1080")
	if _, err := pool.Exec(ctx, `
		INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
		VALUES (32736, 1024, 1180, 24, 240, 96, 1440, 1080)`); err != nil {
		t.Fatal(err)
	}
	area, err := sqlcgen.New(pool).GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: 32736, ServiceID: 1024})
	if err != nil {
		t.Fatal(err)
	}
	w := &CMLogoCandidateWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: filepath.Join(mediaDir, "scratch"),
		CMDetect: config.CMDetectConfig{Enabled: true, BinaryDir: tools.binDir}, FFprobe: tools.ffprobe,
	}
	err = w.Work(ctx, cmLogoCandidateJob(id, 4344, area.UpdatedAt))
	if err == nil || !strings.Contains(err.Error(), "1440x1080") {
		t.Fatalf("Work error = %v, want resolution mismatch", err)
	}
	candidate, err := sqlcgen.New(pool).GetCMLogoCandidate(ctx, sqlcgen.GetCMLogoCandidateParams{NetworkID: 32736, ServiceID: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != "failed" || candidate.Stage == nil || *candidate.Stage != "area" {
		t.Fatalf("candidate = %#v, want failed/area", candidate)
	}
}

// 学習済みロゴと原本の解像度が違えば、logoframe を呼ばずに resolution として
// 失敗する。枠と違ってロゴ自身にも coded size が必要なことを検証する。
func TestCMDetectWorkRejectsLearnedLogoWithOtherResolution(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 922)
	tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1920x1080")
	if err := sqlcgen.New(pool).UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: 32736, ServiceID: 1024, Lgd: buildTestLGD(4, 3, 1000, 4080),
		LearnedFrom: &id, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}

	err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(id, 3))
	if err == nil {
		t.Fatal("Work succeeded although the learned logo is for another resolution")
	}
	for _, want := range []string{"1440x1080", "1920x1080"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}
	if _, statErr := os.Stat(tools.logoframeArgs); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("logoframe ran (stat %v); the mismatched logo must not be used", statErr)
	}
	var state, stage string
	if err := pool.QueryRow(ctx, `SELECT state, stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &stage); err != nil {
		t.Fatal(err)
	}
	if state != "retrying" || stage != "resolution" {
		t.Errorf("attempt = state %q stage %q, want retrying/resolution", state, stage)
	}
}

// 既存ロゴにも一致率判定を掛け、低い場合は match で止める。
func TestCMDetectWorkRejectsLowMatchForExistingLogo(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 923)
	tools := newFakeCMToolsWithSizeAndReport(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000", "1440x1080", "managed logo: v0001 match=9.99% threshold=0%")
	if err := sqlcgen.New(pool).UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: 32736, ServiceID: 1024, Lgd: buildTestLGD(4, 3, 1000, 4080),
		LearnedFrom: &id, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}

	err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(id, 3))
	if err == nil || !strings.Contains(err.Error(), "below") {
		t.Fatalf("Work error = %v, want a low-match failure", err)
	}
	var stage string
	if err := pool.QueryRow(ctx, `SELECT stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&stage); err != nil {
		t.Fatal(err)
	}
	if stage != "match" {
		t.Errorf("attempt stage = %q, want match", stage)
	}
	if n := countLogos(t, pool); n != 1 {
		t.Errorf("cm_logos rows = %d, want the existing logo preserved", n)
	}
}

// 新規学習ロゴの一致率が低い場合、局のロゴとして保存しない。
func TestCMDetectWorkDoesNotPersistLowMatchForNewLogo(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 924)
	tools := newFakeCMToolsWithSizeAndReport(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000", "1440x1080", "managed logo: v0001 match=9.99% threshold=0%")

	err := newCMDetectTestWorker(pool, mediaDir, tools).Work(context.Background(), cmJob(id, 3))
	if err == nil || !strings.Contains(err.Error(), "below") {
		t.Fatalf("Work error = %v, want a low-match failure", err)
	}
	var stage string
	if err := pool.QueryRow(context.Background(), `SELECT stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&stage); err != nil {
		t.Fatal(err)
	}
	if stage != "match" {
		t.Errorf("attempt stage = %q, want match", stage)
	}
	if n := countLogos(t, pool); n != 0 {
		t.Errorf("cm_logos rows = %d, want 0 for a low-match learned logo", n)
	}
}

// logoframe の出力契約が変わったときは、成功扱いにせず logo で失敗する。
func TestCMDetectWorkRequiresLogoMatchOutput(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 925)
	tools := newFakeCMToolsWithSizeAndReport(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000", "1440x1080", "")

	err := newCMDetectTestWorker(pool, mediaDir, tools).Work(context.Background(), cmJob(id, 3))
	if err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("Work error = %v, want missing-match-output failure", err)
	}
	var stage string
	if err := pool.QueryRow(context.Background(), `SELECT stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&stage); err != nil {
		t.Fatal(err)
	}
	if stage != "logo" {
		t.Errorf("attempt stage = %q, want logo", stage)
	}
	if n := countLogos(t, pool); n != 0 {
		t.Errorf("cm_logos rows = %d, want 0 when match output is missing", n)
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
	}{{1, "retrying"}, {2, "retrying"}, {3, "failed"}} {
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
		var stage *string
		var message *string
		var attemptCount int32
		if err := pool.QueryRow(ctx, `SELECT state, stage, error, attempt_count FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &stage, &message, &attemptCount); err != nil {
			t.Fatalf("attempt %d: attempt row: %v", tt.attempt, err)
		}
		if state != tt.wantState || attemptCount != int32(tt.attempt) || message == nil || !strings.Contains(*message, "chapter_exe") {
			t.Errorf("attempt %d: state = %q count = %d error = %v, want state %q and chapter_exe error", tt.attempt, state, attemptCount, message, tt.wantState)
		}
		if stage == nil || *stage != "chapter" {
			t.Errorf("attempt %d: stage = %v, want chapter", tt.attempt, stage)
		}
	}
}

func waitForCMLogoframeStart(t *testing.T, tools cmToolset) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(tools.started); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("logoframe did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCMDetectDeadlineCountsAndCancellationDoesNotCount(t *testing.T) {
	for _, tc := range []struct {
		name             string
		cancelWork       bool
		newerAttemptLive bool
	}{
		{name: "deadline"},
		{name: "shutdown cancellation", cancelWork: true},
		{name: "shutdown cancellation after rescue", cancelWork: true, newerAttemptLive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()
			mediaDir := t.TempDir()
			id := seedCMRecording(t, pool, mediaDir, 926)
			tools := newFakeCMToolsWithSize(t, buildTestLGD(4, 3, 1000, 4080), "1440x1080")
			if err := os.WriteFile(tools.hold, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			var workCtx context.Context
			var cancel context.CancelFunc
			if tc.cancelWork {
				workCtx, cancel = context.WithCancel(ctx)
			} else {
				workCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- newCMDetectTestWorker(pool, mediaDir, tools).Work(workCtx, cmJob(id, 1))
			}()
			waitForCMLogoframeStart(t, tools)
			if tc.cancelWork {
				if tc.newerAttemptLive {
					if count := startCMDetectionTestAttempt(t, ctx, sqlcgen.New(pool), id); count != 2 {
						t.Fatalf("rescued attempt count = %d, want 2", count)
					}
				}
				cancel()
			}
			err := <-done
			if err == nil {
				t.Fatal("Work succeeded while logoframe was held")
			}
			if !tc.cancelWork && !errors.Is(workCtx.Err(), context.DeadlineExceeded) {
				t.Fatalf("context error = %v, want deadline exceeded", workCtx.Err())
			}
			var state string
			var count int32
			queryErr := pool.QueryRow(ctx, `SELECT state, attempt_count FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &count)
			if tc.cancelWork {
				if tc.newerAttemptLive {
					if queryErr != nil || state != "running" || count != 2 {
						t.Errorf("attempt after old shutdown cancellation = %q/%d, %v; want running/2", state, count, queryErr)
					}
				} else if !errors.Is(queryErr, pgx5.ErrNoRows) {
					t.Errorf("attempt after shutdown cancellation = %q/%d, %v; want no attempt row", state, count, queryErr)
				}
			} else {
				if queryErr != nil {
					t.Fatal(queryErr)
				}
				if state != "retrying" || count != 1 {
					t.Errorf("attempt after deadline = %q/%d, want retrying/1", state, count)
				}
			}
		})
	}
}

func TestUntilEncodedViewDoesNotWaitForCMDetectionWhenDisabledPerRecording(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := insertTestRecordingWithEventID(t, pool, 913)
	assetID := seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("cm/%d.ts", id), []byte("ts"))
	markOriginalTSScanComplete(t, pool, assetID)
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

func TestCMDetectCountsDeadAttemptsAndFencesLateResults(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	q := sqlcgen.New(pool)

	dead := seedCMRecording(t, pool, mediaDir, 920)
	for want := int32(1); want <= cmDetectMaxTries; want++ {
		attempt, err := q.BeginCMDetectionAttempt(ctx, sqlcgen.BeginCMDetectionAttemptParams{
			RecordingID: dead, MaxAttempts: cmDetectMaxTries,
		})
		if err != nil || !attempt.ShouldRun || attempt.AttemptCount != want {
			t.Fatalf("begin dead run %d = %#v, %v", want, attempt, err)
		}
		// Leave the state running to model a process that died before returning.
	}
	tools := newFakeCMTools(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000")
	if err := newCMDetectTestWorker(pool, mediaDir, tools).Work(ctx, cmJob(dead, 1)); err != nil {
		t.Fatalf("Work after the third dead execution: %v", err)
	}
	var state string
	var stage, message *string
	var attemptCount int32
	if err := pool.QueryRow(ctx, `SELECT state, stage, error, attempt_count FROM recording_cm_attempts WHERE recording_id = $1`, dead).
		Scan(&state, &stage, &message, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || stage == nil || *stage != "stopped" || message == nil || !strings.Contains(*message, "process stopped") || attemptCount != 3 {
		t.Errorf("dead attempt row = (%q, %v, %v, %d), want failed/stopped/process stopped/3", state, stage, message, attemptCount)
	}
	if _, err := os.Stat(tools.logoframeArgs); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("logoframe ran after the domain limit was reached (stat %v)", err)
	}

	late := seedCMRecording(t, pool, mediaDir, 921)
	first, err := q.BeginCMDetectionAttempt(ctx, sqlcgen.BeginCMDetectionAttemptParams{
		RecordingID: late, MaxAttempts: cmDetectMaxTries,
	})
	if err != nil || !first.ShouldRun || first.AttemptCount != 1 {
		t.Fatalf("first attempt = %#v, %v", first, err)
	}
	second, err := q.BeginCMDetectionAttempt(ctx, sqlcgen.BeginCMDetectionAttemptParams{
		RecordingID: late, MaxAttempts: cmDetectMaxTries,
	})
	if err != nil || !second.ShouldRun || second.AttemptCount != 2 {
		t.Fatalf("rescued attempt = %#v, %v", second, err)
	}
	item, err := q.GetCMDetectionWorkItem(ctx, late)
	if err != nil {
		t.Fatal(err)
	}
	w := newCMDetectTestWorker(pool, mediaDir, tools)
	if err := w.saveCMDetectionResult(ctx, item, false, nil, "{}", first.AttemptCount); err != nil {
		t.Fatalf("persisting late first-attempt result: %v", err)
	}
	var detections int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recording_cm_detections WHERE recording_id = $1`, late).Scan(&detections); err != nil {
		t.Fatal(err)
	}
	if detections != 0 {
		t.Errorf("late attempt wrote %d result rows, want 0", detections)
	}
	if err := pool.QueryRow(ctx, `SELECT state, attempt_count FROM recording_cm_attempts WHERE recording_id = $1`, late).Scan(&state, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if state != "running" || attemptCount != 2 {
		t.Errorf("current attempt after stale result = %q/%d, want running/2", state, attemptCount)
	}
}

func TestCMRecordingDurationDoesNotChangeRiverUniqueIdentity(t *testing.T) {
	pool := testutil.SetupDB(t)
	client, err := NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := client.Insert(ctx, jobs.CMDetectJobArgs{RecordingID: 99001, RecordingDurationMs: 1000}, nil)
	if err != nil || first.UniqueSkippedAsDuplicate {
		t.Fatalf("first CM detect insert = %#v, %v", first, err)
	}
	second, err := client.Insert(ctx, jobs.CMDetectJobArgs{RecordingID: 99001, RecordingDurationMs: 900000}, nil)
	if err != nil || !second.UniqueSkippedAsDuplicate || second.Job.ID != first.Job.ID {
		t.Fatalf("same recording with another duration = %#v, %v; want duplicate of job %d", second, err, first.Job.ID)
	}

	areaAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	logoFirst, err := client.Insert(ctx, jobs.CMLogoCandidateJobArgs{
		NetworkID: 32736, ServiceID: 1024, RecordingID: 99002, AreaUpdatedAt: areaAt,
		RecordingDurationMs: 1000,
	}, nil)
	if err != nil || logoFirst.UniqueSkippedAsDuplicate {
		t.Fatalf("first CM logo candidate insert = %#v, %v", logoFirst, err)
	}
	logoSecond, err := client.Insert(ctx, jobs.CMLogoCandidateJobArgs{
		NetworkID: 32736, ServiceID: 1024, RecordingID: 99002, AreaUpdatedAt: areaAt,
		RecordingDurationMs: 900000,
	}, nil)
	if err != nil || !logoSecond.UniqueSkippedAsDuplicate || logoSecond.Job.ID != logoFirst.Job.ID {
		t.Fatalf("same logo candidate with another duration = %#v, %v; want duplicate of job %d", logoSecond, err, logoFirst.Job.ID)
	}
}

// workHeld は logoframe のダミーが走っている間に during を実行してから Work を終わらせる。
// 実 logoframe が長く走る間に API が割り込む窓の再現。
func workHeld(t *testing.T, pool *pgxpool.Pool, mediaDir string, tools cmToolset, id int64, during func()) error {
	t.Helper()
	if err := os.WriteFile(tools.hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- newCMDetectTestWorker(pool, mediaDir, tools).Work(context.Background(), cmJob(id, 1))
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

// putAreaLikeAPI は PutCMLogoArea と同じ書き込み（局の鍵 + 枠の upsert + ロゴの削除）。
func putAreaLikeAPI(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: 32736, ServiceID: 1024}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: 32736, ServiceID: 1024, X: 1180, Y: 24, W: 240, H: 96, CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.DeleteCMLogo(ctx, sqlcgen.DeleteCMLogoParams{NetworkID: 32736, ServiceID: 1024}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func countLogos(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM cm_logos`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 枠なし・ロゴなしで始まったジョブの実行中に枠が保存されたら、そのジョブが学習した
// ロゴ（枠の外で学習したもの）は保存しない。保存すると枠の保存が消したはずの
// ロゴが復活し、以後の検出が枠ではなくそのロゴを使う。
func TestCMDetectWorkDiscardsLogoLearnedWhileAreaWasSaved(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 930)
	tools := newFakeCMTools(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000")

	if err := workHeld(t, pool, mediaDir, tools, id, func() { putAreaLikeAPI(t, pool) }); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if n := countLogos(t, pool); n != 0 {
		t.Errorf("cm_logos rows = %d, want 0: the logo learned before the area was saved must be discarded", n)
	}
}

// ロゴを持って始まったジョブの実行中にロゴが消されたら、logoDir の古いロゴを書き戻さない。
// 枠の保存とは限らない（ロゴだけを消す操作でも同じ）ので、枠の更新時刻の比較では止まらない。
func TestCMDetectWorkDoesNotWriteBackAnOldLogoDeletedDuringTheJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 931)
	tools := newFakeCMTools(t, buildTestLGD(4, 3, 1000, 4080), 0, "Trim(0,299)", "10.010000")
	if err := sqlcgen.New(pool).UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: 32736, ServiceID: 1024, Lgd: buildTestLGD(4, 3, 1000, 4080), LearnedFrom: &id,
		CodedWidth: 1440, CodedHeight: 1080,
	}); err != nil {
		t.Fatal(err)
	}

	err := workHeld(t, pool, mediaDir, tools, id, func() {
		if _, err := pool.Exec(ctx, `DELETE FROM cm_logos`); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if n := countLogos(t, pool); n != 0 {
		t.Errorf("cm_logos rows = %d, want 0: the old logo read at job start must not be written back", n)
	}
}

// ジョブの実行中に採用（learned_at の更新）があって、そのジョブが失敗しても、attempted_at は
// ジョブ開始時刻のまま残る。終了時刻で上書きすると `attempted_at < l.learned_at` が偽になり、
// 新しいロゴでの再検出が二度と投入されない。
func TestCMDetectWorkFailureAfterAdoptionStaysEligibleForRetry(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	mediaDir := t.TempDir()
	id := seedCMRecording(t, pool, mediaDir, 932)
	tools := newFakeCMTools(t, buildTestLGD(4, 3, 1000, 4080), 1, "Trim(0,149)", "10.010000") // chapter_exe が失敗
	q := sqlcgen.New(pool)
	for range 2 {
		attemptCount := startCMDetectionTestAttempt(t, ctx, q, id)
		markCMDetectionTestFailure(t, ctx, q, id, attemptCount, "retrying", nil, nil)
	}

	err := workHeld(t, pool, mediaDir, tools, id, func() {
		if err := sqlcgen.New(pool).UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
			NetworkID: 32736, ServiceID: 1024, Lgd: buildTestLGD(4, 3, 1000, 4080), LearnedFrom: &id,
			CodedWidth: 1440, CodedHeight: 1080,
		}); err != nil {
			t.Error(err)
		}
	})
	if err == nil {
		t.Fatal("Work succeeded although chapter_exe failed")
	}
	var state, stage string
	if err := pool.QueryRow(ctx, `SELECT state, stage FROM recording_cm_attempts WHERE recording_id = $1`, id).Scan(&state, &stage); err != nil || state != "failed" {
		t.Fatalf("attempt state = %q err = %v, want failed", state, err)
	}
	if stage != "chapter" {
		t.Errorf("attempt stage = %q, want chapter", stage)
	}
	rows, err := sqlcgen.New(pool).ListMissingCMDetections(ctx, sqlcgen.ListMissingCMDetectionsParams{RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RecordingID != id {
		t.Errorf("ListMissingCMDetections = %v, want [%d]: the logo adopted during the failed run must make it eligible again", rows, id)
	}
}
