package streamer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

type recordingHLSTestStore struct {
	mu       sync.Mutex
	target   store.RecordingHLSInputTarget
	err      error
	lookups  int
	lookupFn func(recordingID int64, call int) (store.RecordingHLSInputTarget, error)
}

func (s *recordingHLSTestStore) GetRecordingHLSTarget(_ context.Context, recordingID int64) (store.RecordingHLSInputTarget, error) {
	s.mu.Lock()
	s.lookups++
	call := s.lookups
	target, lookupErr, lookupFn := s.target, s.err, s.lookupFn
	s.mu.Unlock()
	if lookupFn != nil {
		return lookupFn(recordingID, call)
	}
	if lookupErr != nil {
		return store.RecordingHLSInputTarget{}, lookupErr
	}
	if recordingID != 42 {
		return store.RecordingHLSInputTarget{}, store.ErrNotFound
	}
	return target, nil
}

func (s *recordingHLSTestStore) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *recordingHLSTestStore) setTarget(target store.RecordingHLSInputTarget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target = target
}

func (s *recordingHLSTestStore) setLookup(fn func(int64, int) (store.RecordingHLSInputTarget, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupFn = fn
}

type recordingHLSTestHarness struct {
	h      *RecordingHLS
	router chi.Router
	store  *recordingHLSTestStore
	root   string
	// probeDir は偽 ffprobe の置き場。tail（末尾の応答）と calls（末尾の測定の回数）を持つ。
	probeDir string
}

// recordingHLSTestProbe は偽 ffprobe の返す値。firstPacketPTS は先頭の映像パケットの PTS（90 kHz）、
// tail は末尾の窓（標準入力）に対する compact 形式のパケット行である。
type recordingHLSTestProbe struct {
	firstPacketPTS int64
	tail           string
	// tailDelay は末尾の測定を遅くして、同時の要求が重なるようにする（sleep の引数、秒）。
	tailDelay string
}

// recordingHLSTailPackets は、秒 endSeconds に終わる映像パケット 1 つの compact 行を返す。
func recordingHLSTailPackets(endSeconds float64) string {
	const frame = 3600
	return fmt.Sprintf("pts=%d|duration=%d|\n", int64(endSeconds*float64(mpegTSTimestampClock))-frame, frame)
}

// 先頭の映像パケットは 10 秒、末尾は 14 秒に終わる（映像の長さは 4 秒）。
var defaultRecordingHLSTestProbe = recordingHLSTestProbe{firstPacketPTS: 900000, tail: recordingHLSTailPackets(14)}

func newRecordingHLSTestHarness(t *testing.T, captions, hasSubtitles bool, waitTimeout time.Duration, ffmpeg string) recordingHLSTestHarness {
	t.Helper()
	return newRecordingHLSTestHarnessWithProbe(t, captions, hasSubtitles, waitTimeout, ffmpeg, defaultRecordingHLSTestProbe)
}

func newRecordingHLSTestHarnessWithProbe(t *testing.T, captions, hasSubtitles bool, waitTimeout time.Duration, ffmpeg string, probe recordingHLSTestProbe, hw ...*ffargs.HWAccel) recordingHLSTestHarness {
	t.Helper()
	mediaDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mediaDir, "recordings"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "recordings", "source.ts"), []byte("not parsed by the fake probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mediaDir, "ingest"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "ingest", "temp.ts"), []byte("growing temp input"), 0o600); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	media := &recordingHLSTestStore{
		target: store.RecordingHLSInputTarget{
			Kind:             store.RecordingHLSInputOriginal,
			ID:               9001,
			RelPath:          "recordings/source.ts",
			SizeBytes:        28,
			IngestGeneration: 7,
			StartedAt:        &startedAt,
		},
	}
	ffprobe := installRecordingHLSTestFFprobe(t, hasSubtitles, probe)
	if ffmpeg == "" {
		ffmpeg = "/bin/false"
	}
	segmentDir := t.TempDir()
	var hwAccel *ffargs.HWAccel
	var inputExtraArgs []string
	if len(hw) > 0 {
		hwAccel = hw[0]
		inputExtraArgs = []string{"-extra_hw_frames", "32"}
	}
	h, err := NewRecordingHLS(media, RecordingHLSConfig{
		StoreName:       "main",
		MediaDir:        mediaDir,
		SegmentDir:      segmentDir,
		FFmpeg:          ffmpeg,
		FFprobe:         ffprobe,
		HWAccel:         hwAccel,
		InputExtraArgs:  inputExtraArgs,
		Profiles:        []LiveProfile{{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6}},
		Captions:        captions,
		CacheBytes:      1 << 20,
		MaxSegmentBytes: 1 << 10,
		MaxConcurrent:   1,
		WaitTimeout:     waitTimeout,
	})
	if err != nil {
		t.Fatalf("NewRecordingHLS: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("closing test recording HLS: %v", err)
		}
	})
	router := chi.NewRouter()
	h.Mount(router)
	return recordingHLSTestHarness{h: h, router: router, store: media, root: mediaDir, probeDir: filepath.Dir(ffprobe)}
}

func installRecordingHLSTestFFprobe(t *testing.T, hasSubtitles bool, probe recordingHLSTestProbe) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffprobe-recording-hls")
	streams := `{"streams":[{"codec_type":"video"},{"codec_type":"audio"}]}`
	if hasSubtitles {
		streams = `{"streams":[{"codec_type":"video"},{"codec_type":"audio"},{"codec_type":"subtitle"}]}`
	}
	delay := ""
	if probe.tailDelay != "" {
		delay = "sleep " + probe.tailDelay + "; "
	}
	script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" pipe:0 "*) echo x >> %[1]q; %[2]s[ "$(wc -c)" -gt 0 ] || exit 1; cat %[3]q ;;
  *" -show_packets "*) printf '%%d\n' %[4]d ;;
  *) printf '%%s\n' '%[5]s' ;;
esac
`, filepath.Join(dir, "calls"), delay, filepath.Join(dir, "tail"), probe.firstPacketPTS, streams)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tail"), []byte(probe.tail), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// setTail は偽 ffprobe の末尾の応答を差し替える。
func (h recordingHLSTestHarness) setTail(t *testing.T, packets string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.probeDir, "tail"), []byte(packets), 0o600); err != nil {
		t.Fatal(err)
	}
}

// tailCalls は末尾の測定（ffprobe の起動）の回数を返す。
func (h recordingHLSTestHarness) tailCalls(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.probeDir, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "x\n")
}

// useTemp は、録画中の temp を入力にする。temp の中身は偽 ffprobe が解釈するので何でもよい。
func (h recordingHLSTestHarness) useTemp() {
	target := h.store.target
	target.Kind = store.RecordingHLSInputTemp
	target.ID = 0
	target.RelPath = "ingest/temp.ts"
	target.StoreName = "main"
	h.store.setTarget(target)
}

// useOriginal は、commit 後の原本を同じ世代の入力にする。
func (h recordingHLSTestHarness) useOriginal() {
	target := h.store.target
	target.Kind = store.RecordingHLSInputOriginal
	target.ID = 9001
	target.RelPath = "recordings/source.ts"
	target.StoreName = ""
	h.store.setTarget(target)
}

// growTemp は temp にバイトを足す（サイズの変化が再観測の合図になる）。
func (h recordingHLSTestHarness) growTemp(t *testing.T) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(h.root, "ingest", "temp.ts"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("+"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// playlistRows は playlist の (URI, EXTINF) の組を並べ、ENDLIST の有無を返す。
func playlistRows(body string) (rows []string, endlist bool) {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "#EXTINF:") && i+1 < len(lines) {
			rows = append(rows, line+" "+lines[i+1])
		}
	}
	return rows, strings.Contains(body, "#EXT-X-ENDLIST")
}

func recordingHLSRequestPath(storeName string, recordingID int64, generation int64, suffix string) string {
	return fmt.Sprintf("/api/media/stores/%s/recordings/%d/hls/v%d/hd/%s", storeName, recordingID, generation, suffix)
}

func (h recordingHLSTestHarness) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, request)
	return response
}

func (h recordingHLSTestHarness) seedSegment(t *testing.T, key SegmentKey, content string) {
	t.Helper()
	path := filepath.Join(h.h.manager.root, "work", "seed-segment")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing cached segment seed: %v", err)
	}
	lease, _, err := h.h.manager.cache.publish(path, key)
	if err != nil {
		t.Fatalf("seeding cached segment: %v", err)
	}
	lease.release()
}

func TestRecordingHLSMasterAndEVENTPlaylist(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")

	master := h.get(t, recordingHLSRequestPath("main", 42, 7, "master.m3u8"))
	if master.Code != http.StatusOK {
		t.Fatalf("master status = %d, want 200: %s", master.Code, master.Body.String())
	}
	if got := master.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("master Cache-Control = %q, want no-cache", got)
	}
	for _, rendition := range []string{"video", "audio-standard", "audio-main", "audio-sub"} {
		if !strings.Contains(master.Body.String(), rendition+"/index.m3u8") {
			t.Errorf("master playlist does not reference %s", rendition)
		}
	}
	if !strings.Contains(master.Body.String(), "#EXT-X-STREAM-INF:BANDWIDTH=20000000") {
		t.Errorf("master playlist BANDWIDTH = %q, want the fixed 20 Mbps declaration that the seek-buffered check in web/e2e/recording-segmented-vod.mjs depends on", master.Body.String())
	}

	playlist := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
	if playlist.Code != http.StatusOK {
		t.Fatalf("video playlist status = %d, want 200: %s", playlist.Code, playlist.Body.String())
	}
	body := playlist.Body.String()
	if !strings.Contains(body, "#EXT-X-PLAYLIST-TYPE:EVENT") || !strings.Contains(body, "#EXT-X-ENDLIST") {
		t.Fatalf("playlist is not a complete EVENT: %s", body)
	}
	if !strings.Contains(body, "#EXT-X-PROGRAM-DATE-TIME:2026-10-01T00:00:00Z") {
		t.Errorf("playlist has no recording-start program date time: %s", body)
	}
	if got := strings.Count(body, ".ts\n"); got != 2 {
		t.Errorf("playlist has %d segment URIs, want 2 for a 4 second video with 2 second segments: %s", got, body)
	}
}

// 壊し方: セグメント cache key に入力種別を含め、commit 後の同世代 cache を分ける。
func TestRecordingHLSSegmentCacheSurvivesTempCommitAtSameURL(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	target := h.store.target
	target.Kind = store.RecordingHLSInputTemp
	target.ID = 0
	target.RelPath = "ingest/temp.ts"
	target.StoreName = "main"
	h.store.setTarget(target)

	key := SegmentKey{RecordingID: "42", Generation: "7", Profile: "hd", Rendition: "video", Number: 0}
	h.seedSegment(t, key, "same-generation-ts")
	path := recordingHLSRequestPath("main", 42, 7, "video/0.ts")
	for _, kind := range []store.RecordingHLSInputKind{store.RecordingHLSInputTemp, store.RecordingHLSInputOriginal} {
		if kind == store.RecordingHLSInputOriginal {
			target = h.store.target
			target.Kind = store.RecordingHLSInputOriginal
			target.ID = 9001
			target.RelPath = "recordings/source.ts"
			target.StoreName = ""
			h.store.setTarget(target)
		}
		response := h.get(t, path)
		if response.Code != http.StatusOK || response.Body.String() != "same-generation-ts" {
			t.Fatalf("%q segment at unchanged URL = %d %q, want cached bytes", kind, response.Code, response.Body.String())
		}
	}
}

// Ingest writer の切替中も、公開済みの playlist とセグメントを変えない。
// 新 writer が旧 writer の written_bytes に追いつくまで、DB の表示先は旧 temp_rel_path と
// written_bytes のままである。ここでは本物の pgstore（UpdateIngestProgress と
// GetRecordingHLSTarget）を通し、要求ごとに DB から表示先を引く。
//
// 壊し方: UpdateRecordingIngestProgress の CASE を無条件の上書きに替える（素朴な切替）。
// 新 writer は 0 バイトなので、streamer は空の temp を測って失敗し、playlist が 503 になる。
// 偽 ffprobe は空の入力で失敗する（実際の ffprobe と同じ）。
func TestRecordingHLSTempWriterSwitchKeepsPublishedSegmentsAndPlaylist(t *testing.T) {
	ctx := t.Context()
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	// 録画中の temp の再生には開始時刻が要る（metadataForTarget）。
	if _, err := pool.Exec(ctx, `UPDATE recordings SET started_at = $2 WHERE id = $1`, recordingID, time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	media := pgstore.NewMedia(pool, "main", "derived")
	h := newRecordingHLSTestHarnessWithProbe(t, false, false, time.Second, "", recordingHLSTestProbe{firstPacketPTS: 900000, tail: recordingHLSTailPackets(20.04)})
	// URL の 42 は DB の録画 id と違うので、検索だけ本物の録画へ向ける。
	h.store.setLookup(func(int64, int) (store.RecordingHLSInputTarget, error) {
		return media.GetRecordingHLSTarget(ctx, recordingID)
	})

	const generation = 7
	oldContent := []byte("old writer has the published prefix")
	if err := os.WriteFile(filepath.Join(h.root, "ingest", "old-writer.ts"), oldContent, 0o600); err != nil {
		t.Fatal(err)
	}
	oldOwner := uuid.New()
	if _, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, oldOwner, time.Minute); err != nil || !acquired {
		t.Fatalf("acquiring old writer lease = (acquired %v, %v)", acquired, err)
	}
	if err := media.UpdateIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: int64(len(oldContent)), TempRelPath: "ingest/old-writer.ts", TempGeneration: generation,
	}, oldOwner, 1, time.Minute); err != nil {
		t.Fatalf("publishing old writer progress: %v", err)
	}

	playlistPath := recordingHLSRequestPath("main", 42, generation, "video/index.m3u8")
	before := h.get(t, playlistPath)
	beforeRows, beforeEndlist := playlistRows(before.Body.String())
	if before.Code != http.StatusOK || beforeEndlist || len(beforeRows) != 5 {
		t.Fatalf("playlist before writer switch = %d, %d rows, endlist %v: %s", before.Code, len(beforeRows), beforeEndlist, before.Body.String())
	}

	segmentKey := SegmentKey{RecordingID: "42", Generation: "7", Profile: "hd", Rendition: "video", Number: 0}
	h.seedSegment(t, segmentKey, "previously published transport stream")
	segmentPath := recordingHLSRequestPath("main", 42, generation, "video/0.ts")
	firstSegment := h.get(t, segmentPath)
	if firstSegment.Code != http.StatusOK || firstSegment.Body.String() != "previously published transport stream" {
		t.Fatalf("segment before writer switch = %d %q", firstSegment.Code, firstSegment.Body.String())
	}

	// old writer の lease が切れ、新 writer が 0 バイトで始まる。旧 writer が見せた長さ
	// より短いので、表示先は旧 path のままでなければならない。
	if _, err := pool.Exec(ctx, `UPDATE recording_ingest_checkpoints SET lease_expires_at = now() WHERE recording_id = $1`, recordingID); err != nil {
		t.Fatal(err)
	}
	newOwner := uuid.New()
	if cp, acquired, err := media.AcquireIngestCheckpoint(ctx, recordingID, newOwner, time.Minute); err != nil || !acquired || cp.WriterGeneration != 2 {
		t.Fatalf("acquiring new writer lease = (%+v, acquired %v, %v), want generation 2", cp, acquired, err)
	}
	if err := os.WriteFile(filepath.Join(h.root, "ingest", "new-writer.ts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := media.UpdateIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 0, TempRelPath: "ingest/new-writer.ts", TempGeneration: generation,
	}, newOwner, 2, time.Minute); err != nil {
		t.Fatalf("publishing new writer progress: %v", err)
	}

	// 切替後の要求は metadata の cache を使わず、表示先から測り直す（cache があると、
	// 測れなかった新 temp の代わりに古い値を返し、表示先の違いが隠れる）。
	h.h.meta.removeRecording(42, generation)
	after := h.get(t, playlistPath)
	afterRows, afterEndlist := playlistRows(after.Body.String())
	if after.Code != http.StatusOK || afterEndlist || strings.Join(afterRows, "\n") != strings.Join(beforeRows, "\n") {
		t.Fatalf("playlist after writer switch = %d, %d rows, endlist %v: %s; before rows were %v", after.Code, len(afterRows), afterEndlist, after.Body.String(), beforeRows)
	}
	segmentAfterSwitch := h.get(t, segmentPath)
	if segmentAfterSwitch.Code != http.StatusOK || segmentAfterSwitch.Body.String() != firstSegment.Body.String() {
		t.Fatalf("published segment after writer switch = %d %q, want the same %d bytes", segmentAfterSwitch.Code, segmentAfterSwitch.Body.String(), firstSegment.Body.Len())
	}
}

// 壊し方: 異なる temp generation を stale として検出しない。
func TestRecordingHLSTempGenerationChangeReturnsGone(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	target := h.store.target
	target.Kind = store.RecordingHLSInputTemp
	target.ID = 0
	target.RelPath = "ingest/temp.ts"
	target.StoreName = "main"
	target.IngestGeneration = 8
	h.store.setTarget(target)

	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
	if response.Code != http.StatusGone {
		t.Fatalf("old temp generation status = %d, want 410", response.Code)
	}
}

func TestRecordingHLSTempTransformErrorsAreRetryableAndDoNotEndPlaylist(t *testing.T) {
	for _, exitCode := range []int{0, 7} {
		t.Run(fmt.Sprintf("ffmpeg exit %d", exitCode), func(t *testing.T) {
			ffmpeg := filepath.Join(t.TempDir(), "fake-ffmpeg")
			script := fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
			if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			h := newRecordingHLSTestHarness(t, false, false, time.Second, ffmpeg)
			target := h.store.target
			target.Kind = store.RecordingHLSInputTemp
			target.ID = 0
			target.RelPath = "ingest/temp.ts"
			target.StoreName = "main"
			h.store.setTarget(target)

			playlist := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
			if playlist.Code != http.StatusOK || strings.Contains(playlist.Body.String(), "#EXT-X-ENDLIST") {
				t.Fatalf("in-progress playlist = %d %s, want no ENDLIST", playlist.Code, playlist.Body.String())
			}
			response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
				t.Fatalf("growing input segment response = %d retry-after=%q %s, want 503 + Retry-After", response.Code, response.Header().Get("Retry-After"), response.Body.String())
			}
		})
	}
}

func TestRecordingHLSUsesConfiguredHardwareAcceleration(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args")
	ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nexit 7\n", argsPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newRecordingHLSTestHarnessWithProbe(t, false, false, time.Second, ffmpeg, defaultRecordingHLSTestProbe, &ffargs.HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128", OutputFormat: "vaapi"})
	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response = %d, want 500", response.Code)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-hwaccel\nvaapi\n", "-hwaccel_device\n/dev/dri/renderD128\n", "-hwaccel_output_format\nvaapi\n", "-extra_hw_frames\n32\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("ffmpeg args missing %q: %s", want, args)
		}
	}
}

// セグメント n の PROGRAM-DATE-TIME は録画開始 + n·D（D はセグメント秒）。
//
// 壊し方: buildRecordingHLSRenditionPlaylist の programDateTime の加算を消す。
func TestRecordingHLSProgramDateTimeFollowsTheGrid(t *testing.T) {
	probe := recordingHLSTestProbe{firstPacketPTS: 900000, tail: recordingHLSTailPackets(17)}
	h := newRecordingHLSTestHarnessWithProbe(t, false, false, time.Second, "", probe)

	playlist := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
	if playlist.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", playlist.Code, playlist.Body.String())
	}
	startedAt := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	var got []string
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if value, ok := strings.CutPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"); ok {
			got = append(got, value)
		}
	}
	if len(got) != 4 {
		t.Fatalf("PROGRAM-DATE-TIME tags = %v, want 4 for 7 seconds in 2 second segments", got)
	}
	for n, value := range got {
		if want := startedAt.Add(time.Duration(n) * 2 * time.Second).Format(time.RFC3339Nano); value != want {
			t.Errorf("segment %d PROGRAM-DATE-TIME = %s, want %s", n, value, want)
		}
	}
}

// セグメント数の原点は映像の最初のパケットの PTS で、映像ストリームの start_time ではない。
// 先頭に古い PES があると start_time は最初の映像より前を指す。
//
// 壊し方: recordingHLSVideoDurationFromPackets の引き算を originPTS ではなく 0 にする。
func TestRecordingHLSSegmentCountFollowsFirstVideoPacket(t *testing.T) {
	// 末尾の映像は 17.0 秒に終わる。最初の映像パケットは 12.0 なので長さは 5.0 秒、
	// 2 秒セグメントなら 3 本。ストリームの start_time（10.0）を原点にすると 7.0 秒で 4 本になる。
	probe := recordingHLSTestProbe{firstPacketPTS: 12 * 90000, tail: recordingHLSTailPackets(17)}
	h := newRecordingHLSTestHarnessWithProbe(t, false, false, time.Second, "", probe)

	playlist := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
	if playlist.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", playlist.Code, playlist.Body.String())
	}
	if got := strings.Count(playlist.Body.String(), ".ts\n"); got != 3 {
		t.Errorf("segments = %d, want 3 for 5 seconds after the first video packet: %s", got, playlist.Body.String())
	}
	if !strings.Contains(playlist.Body.String(), "#EXTINF:1.000,\n2.ts") {
		t.Errorf("last segment is not the 1 second remainder: %s", playlist.Body.String())
	}
}

func TestRecordingHLSGenerationAndStoreChecks(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	staleKey := SegmentKey{RecordingID: "42", Generation: "6", Profile: "hd", Rendition: "video", Number: 0}
	h.seedSegment(t, staleKey, "stale")

	response := h.get(t, recordingHLSRequestPath("main", 42, 6, "video/0.ts"))
	if response.Code != http.StatusGone {
		t.Fatalf("stale generation status = %d, want 410", response.Code)
	}
	if h.h.manager.HasRecording("42", "6") {
		t.Fatal("stale generation cache was retained")
	}

	wrongStore := h.get(t, recordingHLSRequestPath("other", 42, 7, "master.m3u8"))
	if wrongStore.Code != http.StatusNotFound {
		t.Errorf("wrong store status = %d, want 404", wrongStore.Code)
	}
	unknownRecording := h.get(t, recordingHLSRequestPath("main", 43, 7, "master.m3u8"))
	if unknownRecording.Code != http.StatusNotFound {
		t.Errorf("unknown recording status = %d, want 404", unknownRecording.Code)
	}
}

func TestRecordingHLSSegmentCacheHitSurvivesDatabaseError(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	h.seedSegment(t, SegmentKey{RecordingID: "42", Generation: "7", Profile: "hd", Rendition: "video", Number: 0}, "cached-ts")

	// The first playlist request establishes the probe metadata. Later segment requests
	// must still be able to serve an already-opened generation when the DB is unavailable.
	playlist := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
	if playlist.Code != http.StatusOK {
		t.Fatalf("playlist status = %d: %s", playlist.Code, playlist.Body.String())
	}
	h.store.setError(errors.New("database offline"))

	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
	if response.Code != http.StatusOK || response.Body.String() != "cached-ts" {
		t.Fatalf("cached segment response = %d %q, want 200 cached bytes", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != recordingHLSCacheControl {
		t.Errorf("segment Cache-Control = %q, want %q", got, recordingHLSCacheControl)
	}
	if got := response.Header().Get("Content-Type"); got != "video/mp2t" {
		t.Errorf("segment Content-Type = %q, want video/mp2t", got)
	}
}

func TestRecordingHLSTrashReturns404AndStopsCache(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	h.seedSegment(t, SegmentKey{RecordingID: "42", Generation: "7", Profile: "hd", Rendition: "video", Number: 0}, "cached-ts")
	h.store.setError(store.ErrNotFound)

	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
	if response.Code != http.StatusNotFound {
		t.Fatalf("trashed recording status = %d, want 404", response.Code)
	}
	if h.h.manager.HasRecording("42", "7") {
		t.Fatal("trash left cached segments or transforms for the recording")
	}
}

// startRunningRecordingHLSTransform は、終わらない偽 ffmpeg の変換を 1 本走らせた状態を作る。
// 待ち時間が短いので最初の要求は 503 で返るが、変換自体は走り続ける。
func startRunningRecordingHLSTransform(t *testing.T) recordingHLSTestHarness {
	t.Helper()
	ffmpeg := filepath.Join(t.TempDir(), "sleeping-ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newRecordingHLSTestHarness(t, false, false, 50*time.Millisecond, ffmpeg)
	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("first request status = %d, want 503 with the transform left running", response.Code)
	}
	if _, active := h.h.manager.Stats(); active != 1 {
		t.Fatalf("active transforms = %d, want 1 before the DB changes", active)
	}
	return h
}

// ごみ箱に入れた録画の要求は、キャッシュだけでなく走っている変換も止める。
//
// 壊し方: SegmentManager.StopRecording の stopTaskLocked の繰り返しを消す。
func TestRecordingHLSTrashStopsRunningTransform(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child process")
	}
	h := startRunningRecordingHLSTransform(t)
	h.store.setError(store.ErrNotFound)

	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
	if response.Code != http.StatusNotFound {
		t.Fatalf("trashed recording status = %d, want 404", response.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, active := h.h.manager.Stats(); active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the transform of the trashed recording is still running 5 seconds after the 404")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.h.manager.HasRecording("42", "7") {
		t.Error("manager still has the trashed recording")
	}
}

// DB が引けないとき、開いている変換は続ける。行が無いこと（404）とは区別する。
//
// 壊し方: RecordingHLS.request の DB エラーの分岐を ErrNotFound と同じ扱いにする。
func TestRecordingHLSDatabaseOutageKeepsRunningTransform(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child process")
	}
	h := startRunningRecordingHLSTransform(t)
	h.store.setError(errors.New("database offline"))

	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
	if response.Code == http.StatusNotFound {
		t.Fatalf("status = 404 during a DB outage, want the open transform to keep serving (503 while it is not ready)")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", response.Code)
	}
	if _, active := h.h.manager.Stats(); active != 1 {
		t.Errorf("active transforms = %d after a DB outage, want the running one kept", active)
	}
	if !h.h.manager.HasRecording("42", "7") {
		t.Error("manager dropped the recording on a DB outage")
	}
}

func TestRecordingHLSOpenThenVerifyRejectsRemovedOriginal(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	h.store.setLookup(func(_ int64, call int) (store.RecordingHLSInputTarget, error) {
		if call == 1 {
			return h.store.target, nil
		}
		return store.RecordingHLSInputTarget{}, store.ErrNotFound
	})

	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
	if response.Code != http.StatusNotFound {
		t.Fatalf("open-then-verify status = %d, want 404", response.Code)
	}
	if h.store.lookups != 2 {
		t.Errorf("target lookup count = %d, want initial lookup and open-then-verify", h.store.lookups)
	}
}

func TestRecordingHLSOpenThenVerifyRejectsReplacedOriginal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace func(*store.RecordingHLSInputTarget)
	}{
		{
			name: "asset ID",
			replace: func(target *store.RecordingHLSInputTarget) {
				target.ID++
			},
		},
		{
			name: "relative path",
			replace: func(target *store.RecordingHLSInputTarget) {
				target.RelPath = "recordings/replaced.ts"
			},
		},
		{
			name: "ingest generation",
			replace: func(target *store.RecordingHLSInputTarget) {
				target.IngestGeneration++
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
			h.store.setLookup(func(_ int64, call int) (store.RecordingHLSInputTarget, error) {
				if call == 1 {
					return h.store.target, nil
				}
				replaced := h.store.target
				tc.replace(&replaced)
				return replaced, nil
			})

			response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
			wantStatus := http.StatusNotFound
			if tc.name == "ingest generation" {
				wantStatus = http.StatusGone
			}
			if response.Code != wantStatus {
				t.Fatalf("replaced original status = %d, want %d", response.Code, wantStatus)
			}
			if h.store.lookups != 2 {
				t.Errorf("target lookup count = %d, want initial lookup and open-then-verify", h.store.lookups)
			}
		})
	}
}

func TestRecordingHLSCacheMissTimesOutAs503(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child process")
	}
	ffmpeg := filepath.Join(t.TempDir(), "sleeping-ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newRecordingHLSTestHarness(t, false, false, 25*time.Millisecond, ffmpeg)

	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("cache miss status = %d, want 503 instead of 404: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
}

func TestRecordingHLSOptionalSubtitleRendition(t *testing.T) {
	h := newRecordingHLSTestHarness(t, true, true, time.Second, "")

	master := h.get(t, recordingHLSRequestPath("main", 42, 7, "master.m3u8"))
	if master.Code != http.StatusOK || !strings.Contains(master.Body.String(), `SUBTITLES="subs"`) || !strings.Contains(master.Body.String(), "subtitles/index.m3u8") {
		t.Fatalf("subtitle master = %d %s", master.Code, master.Body.String())
	}
	playlist := h.get(t, recordingHLSRequestPath("main", 42, 7, "subtitles/index.m3u8"))
	if playlist.Code != http.StatusOK || !strings.Contains(playlist.Body.String(), "0.vtt") {
		t.Fatalf("subtitle playlist = %d %s", playlist.Code, playlist.Body.String())
	}
	h.seedSegment(t, SegmentKey{RecordingID: "42", Generation: "7", Profile: "hd", Rendition: "subtitles", Number: 0}, "WEBVTT\n\n")
	segment := h.get(t, recordingHLSRequestPath("main", 42, 7, "subtitles/0.vtt"))
	if segment.Code != http.StatusOK || segment.Body.String() != "WEBVTT\n\n" {
		t.Fatalf("subtitle segment = %d %q", segment.Code, segment.Body.String())
	}
	if got := segment.Header().Get("Content-Type"); got != "text/vtt; charset=utf-8" {
		t.Errorf("subtitle Content-Type = %q", got)
	}
}
