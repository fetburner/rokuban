package streamer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// installOriginalVODFFmpeg is a fake ffmpeg that reproduces the measured hls muxer
// behavior (ffmpeg 9.0.2): with `-hls_playlist_type vod` no .m3u8 exists until the
// process exits, while `event` writes the playlists up front and appends ENDLIST
// at exit. runSeconds is how long it stays alive between the two writes.
func installOriginalVODFFmpeg(t *testing.T, runSeconds int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-original-vod")
	script := fmt.Sprintf(`#!/bin/sh
ptype=""
prev=""
outputs=""
input=""
for a in "$@"; do
  if [ "$prev" = "-hls_playlist_type" ]; then ptype="$a"; fi
  if [ "$prev" = "-i" ]; then input="$a"; fi
  case "$a" in *.%%v.m3u8) outputs="$outputs $a" ;; esac
  prev="$a"
done
# fd 3 is the descriptor the parent opened. Its read position is shared with the
# parent and ffprobe, so a missing rewind shows up as zero bytes here.
bytes=$(cat <&3 | wc -c)
[ "$bytes" -gt 0 ] || exit 3
write() {
  endlist="$1"
  for a in $outputs; do
    outdir=$(dirname "$a")
    output=$(basename "$a")
    profile=${output%%%%.%%v.m3u8}
    mkdir -p "$outdir/segments"
    segment="${profile}.0_seg00001.ts"
    printf 'fake-ts-%%s' "$profile" > "$outdir/segments/$segment"
    printf '#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\n%%s.0.m3u8\n' "$profile" > "$outdir/$profile.m3u8"
    printf '#EXTM3U\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nsegments/%%s\n%%s' "$segment" "$endlist" > "$outdir/${profile}.0.m3u8"
  done
}
if [ "$ptype" = "event" ]; then write ""; fi
sleep %d
write "#EXT-X-ENDLIST
"
exit 0
`, runSeconds)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake ffmpeg: %v", err)
	}
	return path
}

func installCompletedOriginalVODFFmpeg(t *testing.T) string {
	t.Helper()
	return installOriginalVODFFmpeg(t, 1)
}

func installFailedOriginalVODFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-original-vod-fail")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("writing fake ffmpeg: %v", err)
	}
	return path
}

// installFakeFFprobeDuration is a fake ffprobe whose video stream ends at
// videoEnd seconds (start_time 0) while the format duration is formatEnd. It
// drains fd 3 like the real ffprobe, which moves the descriptor's shared read
// position.
func installFakeFFprobeDuration(t *testing.T, videoEnd, formatEnd string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffprobe script assumes a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "fake-ffprobe-duration")
	script := fmt.Sprintf(`#!/bin/sh
input=""
prev=""
for a in "$@"; do if [ "$prev" = "-i" ]; then input="$a"; fi; prev="$a"; done
[ "$input" = "/dev/fd/3" ] || exit 2
cat <&3 >/dev/null || exit 3
printf '{"streams":[{"start_time":"0.000000","duration":"%s"}],"format":{"start_time":"0.000000","duration":"%s"}}\n'
`, videoEnd, formatEnd)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake ffprobe: %v", err)
	}
	return path
}

func originalVODConfig(t *testing.T, mediaDir, ffmpeg string) LiveConfig {
	t.Helper()
	return LiveConfig{
		Enabled:     true,
		FFmpeg:      ffmpeg,
		MediaDir:    mediaDir,
		SegmentDir:  t.TempDir(),
		MaxSessions: 4,
		IdleTimeout: time.Minute,
		Profiles: []LiveProfile{
			{Name: "hd", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
			{Name: "sd", VideoCodec: "libx264", AudioCodec: "aac", Height: 480, SegmentSeconds: 2, PlaylistSize: 6},
		},
	}
}

func newOriginalVODTestServer(t *testing.T, pool *pgxpool.Pool, cfg LiveConfig) (*LiveStreamer, *httptest.Server) {
	t.Helper()
	ls := newLiveStreamerWithPool(pool, chaseTestLiveClient{}, testSite, cfg)
	t.Cleanup(ls.shutdown)
	router := chi.NewRouter()
	ls.Mount(router)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return ls, srv
}

func originalVODPlaylistURL(serverURL string, recordingID int64, profile string) string {
	return fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod/playlist.m3u8?profile=%s", serverURL, recordingID, profile)
}

func originalVODOffsetPlaylistURL(serverURL string, recordingID, offsetSeconds int64, profile string) string {
	return fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod/offset/%d/playlist.m3u8?profile=%s",
		serverURL, recordingID, offsetSeconds, profile)
}

func originalVODTargetFixture(t *testing.T, pool *pgxpool.Pool) (int64, string, []byte) {
	t.Helper()
	mediaDir := t.TempDir()
	relPath := "recordings/original-vod.ts"
	contents := makeTSData(128)
	fullPath := filepath.Join(mediaDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating original directory: %v", err)
	}
	if err := os.WriteFile(fullPath, contents, 0o644); err != nil {
		t.Fatalf("writing original: %v", err)
	}
	recordingID := seedRecording(t, pool)
	seedAsset(t, pool, recordingID, relPath, int64(len(contents)))
	return recordingID, mediaDir, contents
}

func waitForOriginalVODSessionCleanup(t *testing.T, ls *LiveStreamer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ls.sessionCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("original VOD session was not cleaned up, session count = %d", ls.sessionCount())
}

func waitForOriginalVODScratchCleanup(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("original VOD scratch still exists after cleanup: %s", dir)
}

func TestOriginalVODSharesOneFFmpegAndRetainsFilesUntilIdleGC(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, original := originalVODTargetFixture(t, pool)
	// k8s mounts media read-only for the site streamer: nothing under the media
	// root may be created (no lock directory either).
	if err := os.Chmod(mediaDir, 0o555); err != nil {
		t.Fatalf("making media root read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(mediaDir, 0o755) })
	cfg := originalVODConfig(t, mediaDir, installCompletedOriginalVODFFmpeg(t))
	ls, srv := newOriginalVODTestServer(t, pool, cfg)
	var opens atomic.Int32
	ls.afterOriginalVODOpen = func() { opens.Add(1) }

	start := make(chan struct{})
	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Get(originalVODPlaylistURL(srv.URL, recordingID, "hd"))
			if err != nil {
				results <- result{err: err}
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			results <- result{status: resp.StatusCode, body: body, err: readErr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for resp := range results {
		if resp.err != nil {
			t.Fatalf("concurrent original VOD request failed: %v", resp.err)
		}
		if resp.status != http.StatusOK {
			t.Fatalf("concurrent original VOD playlist status = %d, want 200 (%s)", resp.status, resp.body)
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("original source opens = %d, want 1 shared FFmpeg session", got)
	}
	if got := ls.sessionCount(); got != 1 {
		t.Fatalf("session count after concurrent requests = %d, want 1", got)
	}
	zeroOffsetURL := originalVODOffsetPlaylistURL(srv.URL, recordingID, 0, "sd")
	resp, body := get(t, zeroOffsetURL, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "sd.0.m3u8") {
		t.Fatalf("explicit zero-offset playlist status/body = %d %q, want the head session", resp.StatusCode, body)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("explicit zero offset reopened original %d times, want the omitted-offset session", got)
	}

	key := originalVODSessionKeyFor(recordingID, 0)
	ls.mu.Lock()
	s := ls.chaseSessions[key]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("original VOD session is missing")
	}
	if got, want := s.dir, originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, 0); got != want {
		t.Fatalf("zero-offset scratch = %q, want canonical path %q", got, want)
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatal("original VOD ffmpeg did not finish")
	}

	// A different profile after ENDLIST reads the same retained session and does not
	// reopen the original or start another ffmpeg.
	resp, body = get(t, originalVODPlaylistURL(srv.URL, recordingID, "sd"), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "sd.0.m3u8") {
		t.Fatalf("sd playlist status/body = %d %q, want retained sd master", resp.StatusCode, body)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("profile switch reopened original source %d times, want 1", got)
	}
	if entries, err := os.ReadDir(mediaDir); err != nil || len(entries) != 1 || entries[0].Name() != "recordings" {
		t.Fatalf("media root entries = %v (err %v), want only recordings/ (streamer must not write to media)", entries, err)
	}

	base := fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod", srv.URL, recordingID)
	resp, body = get(t, base+"/hd.0.m3u8", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "#EXT-X-ENDLIST") {
		t.Fatalf("finished variant playlist status/body = %d %q, want ENDLIST", resp.StatusCode, body)
	}
	resp, body = get(t, base+"/segments/hd.0_seg00001.ts", nil)
	if resp.StatusCode != http.StatusOK || string(body) != "fake-ts-hd" {
		t.Fatalf("retained segment status/body = %d %q, want retained old segment", resp.StatusCode, body)
	}

	var assetCount int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM media_assets WHERE recording_id = $1", recordingID).Scan(&assetCount); err != nil {
		t.Fatalf("counting media assets: %v", err)
	}
	if assetCount != 1 {
		t.Fatalf("media asset count after HLS conversion = %d, want only the original", assetCount)
	}
	if got, err := os.ReadFile(filepath.Join(mediaDir, "recordings/original-vod.ts")); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("original bytes changed or disappeared: read err=%v", err)
	}

	s.mu.Lock()
	s.lastAccess = time.Now().Add(-2 * time.Minute)
	s.mu.Unlock()
	ls.reapIdleAt(time.Now())
	if got := ls.sessionCount(); got != 0 {
		t.Fatalf("session count after idle GC = %d, want 0", got)
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("HLS scratch still exists after idle GC, stat err = %v", err)
	}
}

func TestOriginalVODOffsetIdleGCRemovesScratch(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	cfg := originalVODConfig(t, mediaDir, installCompletedOriginalVODFFmpeg(t))
	cfg.FFprobe = installFakeFFprobeDuration(t, "600.000000", "600.000000")
	ls, srv := newOriginalVODTestServer(t, pool, cfg)

	const offset = int64(73)
	resp, body := get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, offset, "hd"), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hd.0.m3u8") {
		t.Fatalf("offset playlist status/body = %d %q, want active VOD playlist", resp.StatusCode, body)
	}
	dir := originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, offset)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("offset scratch %s was not created: %v", dir, err)
	}
	key := originalVODSessionKeyFor(recordingID, offset)
	ls.mu.Lock()
	s := ls.chaseSessions[key]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("nonzero original VOD session is missing")
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatal("offset ffmpeg did not finish")
	}

	s.mu.Lock()
	s.lastAccess = time.Now().Add(-2 * time.Minute)
	s.mu.Unlock()
	ls.reapIdleAt(time.Now())
	if got := ls.sessionCount(); got != 0 {
		t.Fatalf("session count after nonzero-offset idle GC = %d, want 0", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("nonzero-offset scratch still exists after idle GC, stat err = %v", err)
	}
}

func TestOriginalVODOffsetRejectsNonCanonicalAndOutOfRangeValues(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	cfg := originalVODConfig(t, mediaDir, installOriginalVODFFmpeg(t, 0))
	// 映像は 600 秒で終わり、format の duration は音声のぶん 601.5 秒まで続く。
	cfg.FFprobe = installFakeFFprobeDuration(t, "600.000000", "601.500000")
	ls, srv := newOriginalVODTestServer(t, pool, cfg)

	for _, raw := range []string{"007", "+5", "5.0", "-1", "9223372036854775808"} {
		t.Run("malformed/"+raw, func(t *testing.T) {
			url := fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod/offset/%s/playlist.m3u8?profile=hd",
				srv.URL, recordingID, raw)
			resp, _ := get(t, url, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("offset %q status = %d, want 400", raw, resp.StatusCode)
			}
			if got := ls.sessionCount(); got != 0 {
				t.Fatalf("malformed offset %q created %d sessions", raw, got)
			}
		})
	}

	for _, offset := range []int64{600, 601} {
		t.Run(fmt.Sprintf("outside/%d", offset), func(t *testing.T) {
			resp, _ := get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, offset, "hd"), nil)
			if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("offset %d status = %d, want 416", offset, resp.StatusCode)
			}
			waitForOriginalVODSessionCleanup(t, ls)
			waitForOriginalVODScratchCleanup(t, originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, offset))
		})
	}
}

// 原本 VOD の退避は mirakc のチューナー解放待ち（既定 5 秒）を挟まない。
// liveMirakcReleaseWait を短縮しないまま、容量が埋まった状態からのシークが速いことを固定する。
func TestOriginalVODRepeatedSeeksReuseMaxSessionCapacityAfterLeaveHints(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	cfg := originalVODConfig(t, mediaDir, installOriginalVODFFmpeg(t, 0))
	cfg.FFprobe = installFakeFFprobeDuration(t, "1000.000000", "1000.000000")
	cfg.MaxSessions = 1
	cfg.IdleTimeout = time.Minute
	ls, srv := newOriginalVODTestServer(t, pool, cfg)

	offsets := []int64{0, 61, 307, 603, 659}
	for i, offset := range offsets {
		seekStarted := time.Now()
		resp, body := get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, offset, "hd"), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("seek to offset %d status/body = %d %q, want 200", offset, resp.StatusCode, body)
		}
		if elapsed := time.Since(seekStarted); elapsed > time.Second {
			t.Fatalf("seek to offset %d took %v from a full pool, want under 1s (no tuner release wait)", offset, elapsed)
		}
		key := originalVODSessionKeyFor(recordingID, offset)
		ls.mu.Lock()
		s := ls.chaseSessions[key]
		sessionCount := len(ls.chaseSessions)
		ls.mu.Unlock()
		if s == nil {
			t.Fatalf("offset %d session was not retained", offset)
		}
		if sessionCount != 1 {
			t.Fatalf("session count at offset %d = %d with MaxSessions=1, want 1", offset, sessionCount)
		}
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			t.Fatalf("ffmpeg for offset %d did not finish", offset)
		}

		if i > 0 {
			previousOffset := offsets[i-1]
			previousDir := originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, previousOffset)
			if _, err := os.Stat(previousDir); !os.IsNotExist(err) {
				t.Fatalf("previous offset scratch was not reclaimed before offset %d, stat err = %v", offset, err)
			}
		}
		if i == len(offsets)-1 {
			continue
		}
		leavePath := fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod", srv.URL, recordingID)
		if offset > 0 {
			leavePath += fmt.Sprintf("/offset/%d", offset)
		}
		leaveResp, err := http.Post(leavePath+"/leave", "", nil)
		if err != nil {
			t.Fatalf("leave hint for offset %d: %v", offset, err)
		}
		_ = leaveResp.Body.Close()
		if leaveResp.StatusCode != http.StatusNoContent {
			t.Fatalf("leave hint for offset %d status = %d, want 204", offset, leaveResp.StatusCode)
		}
		if idle := s.idleSince(time.Now()); idle <= ls.cfg.idleEvictionThreshold() {
			t.Fatalf("leave hint made offset %d idle by %v, want above eviction threshold %v",
				offset, idle, ls.cfg.idleEvictionThreshold())
		}
	}

	if got := ls.sessionCount(); got != 1 {
		t.Fatalf("session count after repeated seeks = %d, want MaxSessions 1", got)
	}
}

func TestOriginalVODOutOfRangeOffsetReturns416WhenAllSessionsAreActive(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	cfg := originalVODConfig(t, mediaDir, installOriginalVODFFmpeg(t, 30))
	cfg.FFprobe = installFakeFFprobeDuration(t, "10.000000", "10.000000")
	cfg.MaxSessions = 1
	ls, srv := newOriginalVODTestServer(t, pool, cfg)

	resp, body := get(t, originalVODPlaylistURL(srv.URL, recordingID, "hd"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("active session playlist status/body = %d %q, want 200", resp.StatusCode, body)
	}
	key := originalVODSessionKeyFor(recordingID, 0)
	ls.mu.Lock()
	active := ls.chaseSessions[key]
	ls.mu.Unlock()
	if active == nil {
		t.Fatal("active original VOD session is missing")
	}
	select {
	case <-active.done:
		t.Fatal("original VOD session finished before the out-of-range request")
	default:
	}

	resp, body = get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, 10, "hd"), nil)
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out-of-range offset status/body = %d %q, want 416", resp.StatusCode, body)
	}
	ls.mu.Lock()
	stillActive := ls.chaseSessions[key]
	ls.mu.Unlock()
	if stillActive != active {
		t.Fatal("out-of-range request displaced the active session")
	}
	select {
	case <-active.done:
		t.Fatal("out-of-range request stopped the active session")
	default:
	}
}

func TestOriginalVODOutOfRangeOffsetDoesNotEvictIdleSession(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	cfg := originalVODConfig(t, mediaDir, installCompletedOriginalVODFFmpeg(t))
	cfg.FFprobe = installFakeFFprobeDuration(t, "10.000000", "10.000000")
	cfg.MaxSessions = 1
	ls, srv := newOriginalVODTestServer(t, pool, cfg)

	resp, body := get(t, originalVODPlaylistURL(srv.URL, recordingID, "hd"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("idle session playlist status/body = %d %q, want 200", resp.StatusCode, body)
	}
	key := originalVODSessionKeyFor(recordingID, 0)
	ls.mu.Lock()
	idle := ls.chaseSessions[key]
	ls.mu.Unlock()
	if idle == nil {
		t.Fatal("original VOD session is missing")
	}
	select {
	case <-idle.done:
	case <-time.After(3 * time.Second):
		t.Fatal("completed original VOD session did not finish")
	}

	leaveURL := fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod/leave", srv.URL, recordingID)
	leaveResp, err := http.Post(leaveURL, "", nil)
	if err != nil {
		t.Fatalf("leave hint: %v", err)
	}
	_ = leaveResp.Body.Close()
	if leaveResp.StatusCode != http.StatusNoContent {
		t.Fatalf("leave hint status = %d, want 204", leaveResp.StatusCode)
	}
	if idle.idleSince(time.Now()) <= ls.cfg.idleEvictionThreshold() {
		t.Fatal("leave hint did not make the session eligible for eviction")
	}
	dir := originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, 0)

	resp, body = get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, 10, "hd"), nil)
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out-of-range offset status/body = %d %q, want 416", resp.StatusCode, body)
	}
	ls.mu.Lock()
	stillIdle := ls.chaseSessions[key]
	ls.mu.Unlock()
	if stillIdle != idle {
		t.Fatal("out-of-range request evicted the idle session")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("idle session scratch was removed by out-of-range request: %v", err)
	}
}

func TestOriginalVODUnavailableTargetsReturn404WithoutSession(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	ls, srv := newOriginalVODTestServer(t, pool, originalVODConfig(t, mediaDir, "unused-ffmpeg"))
	var opens atomic.Int32
	ls.afterOriginalVODOpen = func() { opens.Add(1) }
	fullURL := originalVODOffsetPlaylistURL(srv.URL, recordingID, 60, "hd")

	checks := []struct {
		name   string
		update string
	}{
		{name: "missing original", update: "UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE recording_id = $1"},
		{name: "trash", update: "UPDATE recordings SET deleted_at = now() WHERE id = $1"},
		{name: "purged", update: "UPDATE recordings SET purged_at = now() WHERE id = $1"},
		{name: "failed recording", update: "UPDATE recordings SET status = 'failed' WHERE id = $1"},
		{name: "superseded recording", update: "UPDATE recordings SET superseded_at = now() WHERE id = $1"},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(context.Background(), `UPDATE recordings SET status = 'finished', deleted_at = NULL, purged_at = NULL, superseded_at = NULL WHERE id = $1`, recordingID); err != nil {
				t.Fatalf("resetting recording: %v", err)
			}
			if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET state = 'active', deleted_at = NULL WHERE recording_id = $1`, recordingID); err != nil {
				t.Fatalf("resetting original asset: %v", err)
			}
			if _, err := pool.Exec(context.Background(), tc.update, recordingID); err != nil {
				t.Fatalf("making target unavailable: %v", err)
			}

			resp, _ := get(t, fullURL, nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("unavailable original VOD status = %d, want 404", resp.StatusCode)
			}
			if got := ls.sessionCount(); got != 0 {
				t.Fatalf("unavailable target left %d sessions", got)
			}
		})
	}

	resp, _ := get(t, strings.Replace(fullURL, "/api/sites/default/", "/api/sites/unbound/", 1), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unbound site status = %d, want 404", resp.StatusCode)
	}
	if got := ls.sessionCount(); got != 0 {
		t.Fatalf("unbound site left %d sessions", got)
	}
	if got := opens.Load(); got != 0 {
		t.Fatalf("invalid targets opened the original %d times", got)
	}
}

func TestOriginalVODTrashInvalidatesRetainedSessionAndScratch(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	cfg := originalVODConfig(t, mediaDir, installCompletedOriginalVODFFmpeg(t))
	cfg.FFprobe = installFakeFFprobeDuration(t, "600.000000", "600.000000")
	ls, srv := newOriginalVODTestServer(t, pool, cfg)

	resp, body := get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, 60, "hd"), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hd.0.m3u8") {
		t.Fatalf("initial playlist status/body = %d %q, want an active VOD playlist", resp.StatusCode, body)
	}
	key := originalVODSessionKeyFor(recordingID, 60)
	ls.mu.Lock()
	s := ls.chaseSessions[key]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("original VOD session is missing")
	}
	firstDir := originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, 60)
	if got := s.dir; got != firstDir {
		t.Fatalf("offset 60 scratch = %q, want %q", got, firstDir)
	}
	resp, body = get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, 120, "hd"), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hd.0.m3u8") {
		t.Fatalf("second offset playlist status/body = %d %q, want an active VOD playlist", resp.StatusCode, body)
	}
	secondKey := originalVODSessionKeyFor(recordingID, 120)
	ls.mu.Lock()
	second := ls.chaseSessions[secondKey]
	ls.mu.Unlock()
	if second == nil {
		t.Fatal("second offset original VOD session is missing")
	}
	if s == second {
		t.Fatal("offset 60 and offset 120 unexpectedly share a session")
	}
	secondDir := originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, 120)
	if got := second.dir; got != secondDir {
		t.Fatalf("offset 120 scratch = %q, want %q", got, secondDir)
	}
	if s.dir == second.dir {
		t.Fatalf("different offsets share scratch directory %q", s.dir)
	}

	if _, err := pool.Exec(context.Background(), "UPDATE recordings SET deleted_at = now() WHERE id = $1", recordingID); err != nil {
		t.Fatalf("moving recording to trash: %v", err)
	}
	segmentURL := fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod/offset/60/segments/hd.0_seg00001.ts", srv.URL, recordingID)
	resp, _ = get(t, segmentURL, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("trashed original VOD segment status = %d, want 404", resp.StatusCode)
	}
	if got := ls.sessionCount(); got != 0 {
		t.Fatalf("trashed original VOD left %d sessions", got)
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("trashed original VOD scratch still exists, stat err = %v", err)
	}
	if _, err := os.Stat(secondDir); !os.IsNotExist(err) {
		t.Fatalf("second offset scratch still exists after trash, stat err = %v", err)
	}
	resp, _ = get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, 120, "hd"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("trashed original VOD playlist status = %d, want 404", resp.StatusCode)
	}
}

// open-then-verify: a state change that lands after the original was opened but
// before the DB recheck must still end in 404 (delete_reconcile marks deleting
// before it unlinks; trash hides the recording).
func TestOriginalVODVerifiesDBTargetAfterOpen(t *testing.T) {
	for _, tc := range []struct{ name, update string }{
		{name: "asset deleting", update: "UPDATE media_assets SET state = 'deleting' WHERE recording_id = $1"},
		{name: "trash", update: "UPDATE recordings SET deleted_at = now() WHERE id = $1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
			ls, srv := newOriginalVODTestServer(t, pool, originalVODConfig(t, mediaDir, "unused-ffmpeg"))
			var opens atomic.Int32
			ls.afterOriginalVODOpen = func() {
				opens.Add(1)
				if _, err := pool.Exec(context.Background(), tc.update, recordingID); err != nil {
					t.Errorf("changing target after open: %v", err)
				}
			}

			resp, _ := get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, 1, "hd"), nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("target changed after open status = %d, want 404", resp.StatusCode)
			}
			if got := opens.Load(); got != 1 {
				t.Fatalf("opens = %d, want 1 (the change must land after open)", got)
			}
			waitForOriginalVODSessionCleanup(t, ls)
			waitForOriginalVODScratchCleanup(t, originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, 1))
		})
	}
}

// until_encoded deletes the original (DB state + unlink) often right after encode
// finishes; a playback in progress must keep getting segments.
func TestOriginalVODRetainedSessionSurvivesOriginalDeletion(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	cfg := originalVODConfig(t, mediaDir, installCompletedOriginalVODFFmpeg(t))
	cfg.FFprobe = installFakeFFprobeDuration(t, "600.000000", "600.000000")
	ls, srv := newOriginalVODTestServer(t, pool, cfg)
	originalPath := filepath.Join(mediaDir, "recordings/original-vod.ts")
	unlinked := make(chan error, 1)
	// 1 回目の open は範囲判定の確認で、すぐ閉じられる。セッションが開く 2 回目の直後に消す。
	var opens atomic.Int32
	ls.afterOriginalVODOpen = func() {
		if opens.Add(1) == 2 {
			unlinked <- os.Remove(originalPath)
		}
	}

	resp, body := get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, 90, "hd"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initial playlist status/body = %d %q", resp.StatusCode, body)
	}
	if err := <-unlinked; err != nil {
		t.Fatalf("unlinking original after it was opened: %v", err)
	}
	if _, err := os.Stat(originalPath); !os.IsNotExist(err) {
		t.Fatalf("original canonical path still exists after the open hook, stat err = %v", err)
	}
	if _, err := pool.Exec(context.Background(), "UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE recording_id = $1", recordingID); err != nil {
		t.Fatalf("deleting original asset: %v", err)
	}

	base := fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod/offset/90", srv.URL, recordingID)
	resp, body = get(t, base+"/segments/hd.0_seg00001.ts", nil)
	if resp.StatusCode != http.StatusOK || string(body) != "fake-ts-hd" {
		t.Fatalf("segment after original deletion = %d %q, want 200 retained segment", resp.StatusCode, body)
	}
	if got := ls.sessionCount(); got != 1 {
		t.Fatalf("original deletion dropped the session, count = %d, want 1", got)
	}
	key := originalVODSessionKeyFor(recordingID, 90)
	ls.mu.Lock()
	s := ls.chaseSessions[key]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("offset VOD session is missing after the original was unlinked")
	}

	// A DB outage must not stop serving files that are already on scratch.
	broken, err := pgxpool.New(context.Background(), "postgres://127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("creating unreachable pool: %v", err)
	}
	t.Cleanup(broken.Close)
	ls.pool = broken
	resp, body = get(t, base+"/segments/hd.0_seg00001.ts", nil)
	if resp.StatusCode != http.StatusOK || string(body) != "fake-ts-hd" {
		t.Fatalf("segment during DB outage = %d %q, want 200", resp.StatusCode, body)
	}
}

func TestOriginalVODFailedFFmpegReleasesSessionAndScratch(t *testing.T) {
	withPlaylistStartupTimeout(t, 100*time.Millisecond)
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	ls, srv := newOriginalVODTestServer(t, pool, originalVODConfig(t, mediaDir, installFailedOriginalVODFFmpeg(t)))

	resp, _ := get(t, originalVODPlaylistURL(srv.URL, recordingID, "hd"), nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("failed ffmpeg playlist status = %d, want 504", resp.StatusCode)
	}
	waitForOriginalVODSessionCleanup(t, ls)
	waitForOriginalVODScratchCleanup(t, originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, 0))
}

func TestOriginalVODServesPlaylistWhileFFmpegIsStillConverting(t *testing.T) {
	withPlaylistStartupTimeout(t, 3*time.Second)
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	ls, srv := newOriginalVODTestServer(t, pool, originalVODConfig(t, mediaDir, installOriginalVODFFmpeg(t, 30)))

	resp, body := get(t, originalVODPlaylistURL(srv.URL, recordingID, "hd"), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hd.0.m3u8") {
		t.Fatalf("master status/body = %d %q, want 200 while ffmpeg is still running", resp.StatusCode, body)
	}
	ls.mu.Lock()
	s := ls.chaseSessions[originalVODSessionKeyFor(recordingID, 0)]
	ls.mu.Unlock()
	select {
	case <-s.done:
		t.Fatal("fake ffmpeg exited; the test no longer measures an in-progress conversion")
	default:
	}
	base := fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod", srv.URL, recordingID)
	resp, body = get(t, base+"/hd.0.m3u8", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "#EXTINF") || strings.Contains(string(body), "#EXT-X-ENDLIST") {
		t.Fatalf("in-progress variant status/body = %d %q, want EXTINF without ENDLIST", resp.StatusCode, body)
	}
}
