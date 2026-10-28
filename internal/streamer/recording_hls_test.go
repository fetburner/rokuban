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

	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/store"
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
}

// recordingHLSTestProbe は偽 ffprobe の返す値。firstPacketPTS は 90 kHz、他は秒。
type recordingHLSTestProbe struct {
	firstPacketPTS int64
	videoStart     string
	videoDuration  string
}

var defaultRecordingHLSTestProbe = recordingHLSTestProbe{firstPacketPTS: 900000, videoStart: "10.0", videoDuration: "4.0"}

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
	return recordingHLSTestHarness{h: h, router: router, store: media, root: mediaDir}
}

func installRecordingHLSTestFFprobe(t *testing.T, hasSubtitles bool, probe recordingHLSTestProbe) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffprobe-recording-hls")
	streams := fmt.Sprintf(`{"streams":[{"codec_type":"video","start_time":"%[1]s","duration":"%[2]s"},{"codec_type":"audio","start_time":"%[1]s","duration":"%[2]s"}]}`, probe.videoStart, probe.videoDuration)
	if hasSubtitles {
		streams = fmt.Sprintf(`{"streams":[{"codec_type":"video","start_time":"%[1]s","duration":"%[2]s"},{"codec_type":"audio","start_time":"%[1]s","duration":"%[2]s"},{"codec_type":"subtitle"}]}`, probe.videoStart, probe.videoDuration)
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n  *\" -show_packets \"*) printf '%d\\n' ;;\n  *) printf '%%s\\n' '%s' ;;\nesac\n", probe.firstPacketPTS, streams)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
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

func TestRecordingHLSTempPlaylistUsesMeasuredVisibilityMargin(t *testing.T) {
	startedAt := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want int64
	}{
		{name: "before visibility margin", at: startedAt.Add(56 * time.Second), want: -1},
		{name: "first segment after margin", at: startedAt.Add(57 * time.Second), want: 0},
		{name: "second segment after one duration", at: startedAt.Add(59 * time.Second), want: 1},
		{name: "third segment after two durations", at: startedAt.Add(61 * time.Second), want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recordingHLSTempLastSegmentNumber(startedAt, tc.at, 2); got != tc.want {
				t.Fatalf("last temp segment = %d, want %d", got, tc.want)
			}
		})
	}

	metadata := recordingHLSMetadata{startedAt: startedAt}
	profile := LiveProfile{Name: "hd", SegmentSeconds: 2}
	playlist, err := buildRecordingHLSTempPlaylist(metadata, profile, "video", startedAt.Add(61*time.Second))
	if err != nil {
		t.Fatalf("build temp playlist: %v", err)
	}
	body := string(playlist)
	if !strings.Contains(body, "#EXT-X-PLAYLIST-TYPE:EVENT") || strings.Contains(body, "#EXT-X-ENDLIST") {
		t.Fatalf("growing temp playlist = %s, want EVENT without ENDLIST", body)
	}
	if got := strings.Count(body, ".ts\n"); got != 3 {
		t.Fatalf("growing temp playlist has %d segments, want 3: %s", got, body)
	}
}

// 壊し方: 未知の record status も終端とみなす。
func TestRecordingHLSTempPlaylistEndsOnlyForKnownTerminalStateOrCommit(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	startedAt := time.Now().UTC().Truncate(time.Second).Add(-61 * time.Second)
	target := h.store.target
	target.Kind = store.RecordingHLSInputTemp
	target.ID = 0
	target.RelPath = "ingest/temp.ts"
	target.StoreName = "main"
	target.StartedAt = &startedAt
	tempInfo, err := os.Stat(filepath.Join(h.root, "ingest", "temp.ts"))
	if err != nil {
		t.Fatal(err)
	}
	target.SizeBytes = tempInfo.Size()
	target.RecordStatus = "recording"
	h.store.setTarget(target)
	path := recordingHLSRequestPath("main", 42, 7, "video/index.m3u8")

	for _, status := range []string{"recording", "finished", "unknown", ""} {
		target.RecordStatus = status
		h.store.setTarget(target)
		response := h.get(t, path)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "#EXT-X-ENDLIST") {
			t.Errorf("record status %q playlist = %d %s, want EVENT without ENDLIST", status, response.Code, response.Body.String())
		}
	}
	for _, status := range []string{"canceled", "failed"} {
		target.RecordStatus = status
		target.TempFinalized = false
		h.store.setTarget(target)
		response := h.get(t, path)
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, "#EXT-X-PLAYLIST-TYPE:EVENT") || strings.Contains(body, "#EXT-X-ENDLIST") {
			t.Errorf("record status %q before ingest stops playlist = %d %s, want open EVENT", status, response.Code, body)
		}
		target.TempFinalized = true
		h.store.setTarget(target)
		response = h.get(t, path)
		body = response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, "#EXT-X-PLAYLIST-TYPE:EVENT") || !strings.Contains(body, "#EXT-X-ENDLIST") {
			t.Errorf("record status %q after ingest stops playlist = %d %s, want ended EVENT", status, response.Code, body)
		}
	}

	// Commit copies the same temp generation to the original. The URL stays v7 while the
	// source changes to the committed original and the EVENT playlist becomes final.
	target = h.store.target
	target.Kind = store.RecordingHLSInputOriginal
	target.ID = 9001
	target.RelPath = "recordings/source.ts"
	target.StoreName = ""
	target.RecordStatus = "finished"
	h.store.setTarget(target)
	response := h.get(t, path)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "#EXT-X-ENDLIST") || !strings.Contains(response.Body.String(), "#EXT-X-PLAYLIST-TYPE:EVENT") {
		t.Fatalf("committed playlist at unchanged URL = %d %s, want final EVENT", response.Code, response.Body.String())
	}
}

// 壊し方: terminal metadata を temp の最終サイズが変わっても再利用する。
func TestRecordingHLSTerminalTempSnapshotChangeReprobesMetadata(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	startedAt := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	target := h.store.target
	target.Kind = store.RecordingHLSInputTemp
	target.ID = 0
	target.RelPath = "ingest/temp.ts"
	target.StoreName = "main"
	target.RecordStatus = "canceled"
	target.TempFinalized = true
	target.StartedAt = &startedAt
	path := filepath.Join(h.root, "ingest", "temp.ts")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	target.SizeBytes = info.Size()
	h.store.setTarget(target)

	if _, err := h.h.metadataForTarget(context.Background(), 42, 7, target); err != nil {
		t.Fatalf("probing initial finalized temp: %v", err)
	}
	key := recordingHLSKey{recordingID: 42, generation: 7, kind: store.RecordingHLSInputTemp}
	metadata, ok := h.h.meta.get(key)
	if !ok || metadata.tempSize != info.Size() {
		t.Fatalf("cached temp size = (%d, %v), want %d", metadata.tempSize, ok, info.Size())
	}

	appended := []byte(" more captured bytes")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(appended); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	target.SizeBytes += int64(len(appended))
	h.store.setTarget(target)

	if _, err := h.h.metadataForTarget(context.Background(), 42, 7, target); err != nil {
		t.Fatalf("reprobing changed finalized temp: %v", err)
	}
	metadata, ok = h.h.meta.get(key)
	if !ok || metadata.tempSize != target.SizeBytes {
		t.Fatalf("cached temp size after append = (%d, %v), want %d", metadata.tempSize, ok, target.SizeBytes)
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
			target.RecordStatus = "finished"
			h.store.setTarget(target)
		}
		response := h.get(t, path)
		if response.Code != http.StatusOK || response.Body.String() != "same-generation-ts" {
			t.Fatalf("%q segment at unchanged URL = %d %q, want cached bytes", kind, response.Code, response.Body.String())
		}
	}
}

func TestRecordingHLSTerminalTempMissingAfterOrphanCleanupReturns404(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	target := h.store.target
	target.Kind = store.RecordingHLSInputTemp
	target.ID = 0
	target.RelPath = "ingest/temp.ts"
	target.StoreName = "main"
	target.RecordStatus = "canceled"
	h.store.setTarget(target)
	if err := os.Remove(filepath.Join(h.root, "ingest", "temp.ts")); err != nil {
		t.Fatalf("removing aged temp: %v", err)
	}

	response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing terminal temp status = %d, want 404: %s", response.Code, response.Body.String())
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
			target.RecordStatus = "recording"
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

func TestRecordingHLSTerminalTempUsesFiniteTransform(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		finalized  bool
		wantFollow bool
		wantStatus int
	}{
		{name: "growing", status: "recording", wantFollow: true, wantStatus: http.StatusServiceUnavailable},
		{name: "terminal", status: "canceled", finalized: true, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsPath := filepath.Join(t.TempDir(), "ffmpeg-args")
			ffmpeg := filepath.Join(t.TempDir(), "fake-ffmpeg")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nexit 7\n", argsPath)
			if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			h := newRecordingHLSTestHarness(t, false, false, time.Second, ffmpeg)
			target := h.store.target
			target.Kind = store.RecordingHLSInputTemp
			target.ID = 0
			target.RelPath = "ingest/temp.ts"
			target.StoreName = "main"
			target.RecordStatus = tc.status
			target.TempFinalized = tc.finalized
			info, err := os.Stat(filepath.Join(h.root, "ingest", "temp.ts"))
			if err != nil {
				t.Fatal(err)
			}
			target.SizeBytes = info.Size()
			h.store.setTarget(target)

			response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/0.ts"))
			if response.Code != tc.wantStatus {
				t.Fatalf("segment response = %d %s, want %d", response.Code, response.Body.String(), tc.wantStatus)
			}
			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatalf("reading ffmpeg arguments: %v", err)
			}
			gotFollow := strings.Contains(string(args), "-follow\n")
			if gotFollow != tc.wantFollow {
				t.Errorf("ffmpeg args contain -follow = %v, want %v: %s", gotFollow, tc.wantFollow, args)
			}
			gotTimeout := strings.Contains(string(args), "-rw_timeout\n")
			if gotTimeout != tc.wantFollow {
				t.Errorf("ffmpeg args contain -rw_timeout = %v, want %v: %s", gotTimeout, tc.wantFollow, args)
			}
		})
	}
}

// セグメント n の PROGRAM-DATE-TIME は録画開始 + n·D（D はセグメント秒）。
//
// 壊し方: buildRecordingHLSRenditionPlaylist の programDateTime の加算を消す。
func TestRecordingHLSProgramDateTimeFollowsTheGrid(t *testing.T) {
	probe := recordingHLSTestProbe{firstPacketPTS: 900000, videoStart: "10.0", videoDuration: "7.0"}
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
// 壊し方: probeRecordingHLSTiming の durationSeconds の引き算を originPTS ではなく videoStart にする。
func TestRecordingHLSSegmentCountFollowsFirstVideoPacket(t *testing.T) {
	// start_time 10.0 + duration 7.0 = 終端 17.0。最初の映像パケットは 12.0 なので長さは 5.0 秒、
	// 2 秒セグメントなら 3 本。start_time を原点にすると 7.0 秒で 4 本になる。
	probe := recordingHLSTestProbe{firstPacketPTS: 12 * 90000, videoStart: "10.0", videoDuration: "7.0"}
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
