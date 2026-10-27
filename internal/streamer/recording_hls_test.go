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

	"github.com/fetburner/rokuban/internal/store"
)

type recordingHLSTestStore struct {
	mu       sync.Mutex
	target   store.RecordingHLSOriginalTarget
	err      error
	lookups  int
	lookupFn func(recordingID int64, call int) (store.RecordingHLSOriginalTarget, error)
}

func (s *recordingHLSTestStore) GetRecordingHLSOriginalTarget(_ context.Context, recordingID int64) (store.RecordingHLSOriginalTarget, error) {
	s.mu.Lock()
	s.lookups++
	call := s.lookups
	target, lookupErr, lookupFn := s.target, s.err, s.lookupFn
	s.mu.Unlock()
	if lookupFn != nil {
		return lookupFn(recordingID, call)
	}
	if lookupErr != nil {
		return store.RecordingHLSOriginalTarget{}, lookupErr
	}
	if recordingID != 42 {
		return store.RecordingHLSOriginalTarget{}, store.ErrNotFound
	}
	return target, nil
}

func (s *recordingHLSTestStore) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *recordingHLSTestStore) setLookup(fn func(int64, int) (store.RecordingHLSOriginalTarget, error)) {
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

func newRecordingHLSTestHarness(t *testing.T, captions, hasSubtitles bool, waitTimeout time.Duration, ffmpeg string) recordingHLSTestHarness {
	t.Helper()
	mediaDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mediaDir, "recordings"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "recordings", "source.ts"), []byte("not parsed by the fake probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	media := &recordingHLSTestStore{
		target: store.RecordingHLSOriginalTarget{
			MediaAsset:       store.MediaAsset{ID: 9001, RelPath: "recordings/source.ts", SizeBytes: 28},
			IngestGeneration: 7,
			StartedAt:        &startedAt,
		},
	}
	ffprobe := installRecordingHLSTestFFprobe(t, hasSubtitles)
	if ffmpeg == "" {
		ffmpeg = "/bin/false"
	}
	segmentDir := t.TempDir()
	h, err := NewRecordingHLS(media, RecordingHLSConfig{
		StoreName:       "main",
		MediaDir:        mediaDir,
		SegmentDir:      segmentDir,
		FFmpeg:          ffmpeg,
		FFprobe:         ffprobe,
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

func installRecordingHLSTestFFprobe(t *testing.T, hasSubtitles bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffprobe-recording-hls")
	streams := `{"streams":[{"codec_type":"video","start_time":"10.0","duration":"4.0"},{"codec_type":"audio","start_time":"10.0","duration":"4.0"}]}`
	if hasSubtitles {
		streams = `{"streams":[{"codec_type":"video","start_time":"10.0","duration":"4.0"},{"codec_type":"audio","start_time":"10.0","duration":"4.0"},{"codec_type":"subtitle"}]}`
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n  *\" -show_packets \"*) printf '900000\\n' ;;\n  *) printf '%%s\\n' '%s' ;;\nesac\n", streams)
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

func TestRecordingHLSMasterAndVODPlaylist(t *testing.T) {
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
		t.Errorf("master playlist BANDWIDTH = %q, want 20 Mbps so hls.js leaves the 52 second seek outside its initial buffer", master.Body.String())
	}

	playlist := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
	if playlist.Code != http.StatusOK {
		t.Fatalf("video playlist status = %d, want 200: %s", playlist.Code, playlist.Body.String())
	}
	body := playlist.Body.String()
	if !strings.Contains(body, "#EXT-X-PLAYLIST-TYPE:VOD") || !strings.Contains(body, "#EXT-X-ENDLIST") {
		t.Fatalf("playlist is not a complete VOD: %s", body)
	}
	if !strings.Contains(body, "#EXT-X-PROGRAM-DATE-TIME:2026-10-01T00:00:00Z") {
		t.Errorf("playlist has no recording-start program date time: %s", body)
	}
	if got := strings.Count(body, ".ts\n"); got != 2 {
		t.Errorf("playlist has %d segment URIs, want 2 for a 4 second video with 2 second segments: %s", got, body)
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

func TestRecordingHLSOpenThenVerifyRejectsRemovedOriginal(t *testing.T) {
	h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
	h.store.setLookup(func(_ int64, call int) (store.RecordingHLSOriginalTarget, error) {
		if call == 1 {
			return h.store.target, nil
		}
		return store.RecordingHLSOriginalTarget{}, store.ErrNotFound
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
		replace func(*store.RecordingHLSOriginalTarget)
	}{
		{
			name: "asset ID",
			replace: func(target *store.RecordingHLSOriginalTarget) {
				target.ID++
			},
		},
		{
			name: "relative path",
			replace: func(target *store.RecordingHLSOriginalTarget) {
				target.RelPath = "recordings/replaced.ts"
			},
		},
		{
			name: "ingest generation",
			replace: func(target *store.RecordingHLSOriginalTarget) {
				target.IngestGeneration++
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecordingHLSTestHarness(t, false, false, time.Second, "")
			h.store.setLookup(func(_ int64, call int) (store.RecordingHLSOriginalTarget, error) {
				if call == 1 {
					return h.store.target, nil
				}
				replaced := h.store.target
				tc.replace(&replaced)
				return replaced, nil
			})

			response := h.get(t, recordingHLSRequestPath("main", 42, 7, "video/index.m3u8"))
			if response.Code != http.StatusNotFound {
				t.Fatalf("replaced original status = %d, want 404", response.Code)
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
