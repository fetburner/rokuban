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

func installCompletedOriginalVODFFmpeg(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-original-vod")
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    *.%v.m3u8)
      outdir=$(dirname "$a")
      output=$(basename "$a")
      profile=${output%%.%v.m3u8}
      mkdir -p "$outdir/segments"
      segment="${profile}.0_seg00001.ts"
      printf 'fake-ts-%s' "$profile" > "$outdir/segments/$segment"
      cat > "$outdir/$profile.m3u8" <<EOF
#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=2000000
${profile}.0.m3u8
EOF
      cat > "$outdir/${profile}.0.m3u8" <<EOF
#EXTM3U
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-TARGETDURATION:2
#EXTINF:2.0,
segments/$segment
#EXT-X-ENDLIST
EOF
      ;;
  esac
done
sleep 1
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake ffmpeg: %v", err)
	}
	return path
}

func installFailedOriginalVODFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ffmpeg-original-vod-fail")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("writing fake ffmpeg: %v", err)
	}
	return path
}

func originalVODConfig(t *testing.T, mediaDir, ffmpeg string, locker func(context.Context, string, string) (io.Closer, error)) LiveConfig {
	t.Helper()
	return LiveConfig{
		Enabled:              true,
		FFmpeg:               ffmpeg,
		MediaDir:             mediaDir,
		SegmentDir:           t.TempDir(),
		MaxSessions:          4,
		IdleTimeout:          time.Minute,
		LockMediaRelPathFile: locker,
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
	var lockCalls atomic.Int32
	locker := func(context.Context, string, string) (io.Closer, error) {
		lockCalls.Add(1)
		return io.NopCloser(strings.NewReader("")), nil
	}
	cfg := originalVODConfig(t, mediaDir, installCompletedOriginalVODFFmpeg(t), locker)
	ls, srv := newOriginalVODTestServer(t, pool, cfg)

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
	if got := lockCalls.Load(); got != 1 {
		t.Fatalf("original source lock calls = %d, want 1 shared FFmpeg session", got)
	}
	if got := ls.sessionCount(); got != 1 {
		t.Fatalf("session count after concurrent requests = %d, want 1", got)
	}

	key := originalVODSessionKeyFor(recordingID)
	ls.mu.Lock()
	s := ls.chaseSessions[key]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("original VOD session is missing")
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatal("original VOD ffmpeg did not finish")
	}

	// A different profile after ENDLIST reads the same retained session and does not
	// reopen the original or start another ffmpeg.
	resp, body := get(t, originalVODPlaylistURL(srv.URL, recordingID, "sd"), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "sd.0.m3u8") {
		t.Fatalf("sd playlist status/body = %d %q, want retained sd master", resp.StatusCode, body)
	}
	if got := lockCalls.Load(); got != 1 {
		t.Fatalf("profile switch reopened original source %d times, want 1", got)
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

func TestOriginalVODUnavailableTargetsReturn404WithoutSession(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	var lockCalls atomic.Int32
	locker := func(context.Context, string, string) (io.Closer, error) {
		lockCalls.Add(1)
		return io.NopCloser(strings.NewReader("")), nil
	}
	ls, srv := newOriginalVODTestServer(t, pool, originalVODConfig(t, mediaDir, "unused-ffmpeg", locker))
	fullURL := originalVODPlaylistURL(srv.URL, recordingID, "hd")

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
	if got := lockCalls.Load(); got != 0 {
		t.Fatalf("invalid targets acquired the source lock %d times", got)
	}
}

func TestOriginalVODTrashInvalidatesRetainedSessionAndScratch(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	locker := func(context.Context, string, string) (io.Closer, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	ls, srv := newOriginalVODTestServer(t, pool, originalVODConfig(t, mediaDir, installCompletedOriginalVODFFmpeg(t), locker))

	resp, body := get(t, originalVODPlaylistURL(srv.URL, recordingID, "hd"), nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "hd.0.m3u8") {
		t.Fatalf("initial playlist status/body = %d %q, want an active VOD playlist", resp.StatusCode, body)
	}
	key := originalVODSessionKeyFor(recordingID)
	ls.mu.Lock()
	s := ls.chaseSessions[key]
	ls.mu.Unlock()
	if s == nil {
		t.Fatal("original VOD session is missing")
	}

	if _, err := pool.Exec(context.Background(), "UPDATE recordings SET deleted_at = now() WHERE id = $1", recordingID); err != nil {
		t.Fatalf("moving recording to trash: %v", err)
	}
	segmentURL := fmt.Sprintf("%s/api/sites/default/recordings/%d/original-vod/segments/hd.0_seg00001.ts", srv.URL, recordingID)
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
	resp, _ = get(t, originalVODPlaylistURL(srv.URL, recordingID, "hd"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("trashed original VOD playlist status = %d, want 404", resp.StatusCode)
	}
}

func TestOriginalVODRechecksDBTargetUnderMediaLock(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	var lockCalls atomic.Int32
	locker := func(_ context.Context, _ string, relPath string) (io.Closer, error) {
		lockCalls.Add(1)
		if relPath != "recordings/original-vod.ts" {
			t.Errorf("locked rel_path = %q, want original rel_path", relPath)
		}
		if _, err := pool.Exec(context.Background(), "UPDATE recordings SET deleted_at = now() WHERE id = $1", recordingID); err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader("")), nil
	}
	ls, srv := newOriginalVODTestServer(t, pool, originalVODConfig(t, mediaDir, "unused-ffmpeg", locker))

	resp, _ := get(t, originalVODPlaylistURL(srv.URL, recordingID, "hd"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("target deleted while acquiring lock status = %d, want 404", resp.StatusCode)
	}
	if got := lockCalls.Load(); got != 1 {
		t.Fatalf("media lock calls = %d, want 1", got)
	}
	waitForOriginalVODSessionCleanup(t, ls)
	waitForOriginalVODScratchCleanup(t, originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID))
}

func TestOriginalVODFailedFFmpegReleasesSessionAndScratch(t *testing.T) {
	withPlaylistStartupTimeout(t, 100*time.Millisecond)
	pool := testutil.SetupDB(t)
	recordingID, mediaDir, _ := originalVODTargetFixture(t, pool)
	locker := func(context.Context, string, string) (io.Closer, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	ls, srv := newOriginalVODTestServer(t, pool, originalVODConfig(t, mediaDir, installFailedOriginalVODFFmpeg(t), locker))

	resp, _ := get(t, originalVODPlaylistURL(srv.URL, recordingID, "hd"), nil)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("failed ffmpeg playlist status = %d, want 504", resp.StatusCode)
	}
	waitForOriginalVODSessionCleanup(t, ls)
	waitForOriginalVODScratchCleanup(t, originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID))
}
