package streamer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
)

type fakeChaseRecordClient struct {
	mu        sync.Mutex
	errors    []error
	calls     int
	recordIDs []string
}

func (c *fakeChaseRecordClient) StreamRecordFollow(_ context.Context, recordID string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.recordIDs = append(c.recordIDs, recordID)
	if len(c.errors) > 0 {
		err := c.errors[0]
		c.errors = c.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	return io.NopCloser(strings.NewReader("fake-record")), nil
}

func (c *fakeChaseRecordClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func withPlaylistStartupTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := playlistStartupTimeout
	playlistStartupTimeout = timeout
	t.Cleanup(func() { playlistStartupTimeout = previous })
}

func TestParseCanonicalRecordingID(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
		ok   bool
	}{
		{name: "zero", raw: "0", want: 0, ok: true},
		{name: "positive", raw: "123", want: 123, ok: true},
		{name: "leading zero", raw: "0123"},
		{name: "negative", raw: "-1"},
		{name: "plus sign", raw: "+1"},
		{name: "empty", raw: ""},
		{name: "overflow", raw: "9223372036854775808"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCanonicalRecordingID(tt.raw)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("parseCanonicalRecordingID(%q) = (%d, %v), want (%d, %v)", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestWaitForChaseRecordRetriesNotReady(t *testing.T) {
	withPlaylistStartupTimeout(t, 350*time.Millisecond)
	client := &fakeChaseRecordClient{
		errors: []error{mirakc.ErrRecordNotReady, mirakc.ErrRecordNotReady, nil},
	}

	body, err := waitForChaseRecord(context.Background(), client, "opaque-record-id")
	if err != nil {
		t.Fatalf("waitForChaseRecord() = %v, want success after 204 retries", err)
	}
	defer func() { _ = body.Close() }()
	if got, want := client.callCount(), 3; got != want {
		t.Fatalf("StreamRecordFollow calls = %d, want %d", got, want)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if got := strings.Join(client.recordIDs, ","); got != "opaque-record-id,opaque-record-id,opaque-record-id" {
		t.Errorf("record ids = %q, want the same opaque mirakc id on every retry", got)
	}
}

func TestWaitForChaseRecordNotReadyTimesOutWithoutUpstreamError(t *testing.T) {
	withPlaylistStartupTimeout(t, 220*time.Millisecond)
	client := &fakeChaseRecordClient{errors: []error{
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
		mirakc.ErrRecordNotReady,
	}}

	start := time.Now()
	_, err := waitForChaseRecord(context.Background(), client, "record-still-empty")
	if !errors.Is(err, errChaseRecordNotReadyTimeout) {
		t.Fatalf("waitForChaseRecord() = %v, want errChaseRecordNotReadyTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitForChaseRecord() took %v, want it bounded by startup timeout", elapsed)
	}
	if got := client.callCount(); got < 2 {
		t.Errorf("StreamRecordFollow calls = %d, want repeated 204 polling", got)
	}
}

type blockingChaseRecordClient struct{}

func (blockingChaseRecordClient) StreamRecordFollow(ctx context.Context, _ string) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestWaitForChaseRecordBoundsBlockedResponse(t *testing.T) {
	withPlaylistStartupTimeout(t, 40*time.Millisecond)
	start := time.Now()
	_, err := waitForChaseRecord(context.Background(), blockingChaseRecordClient{}, "blocked")
	if !errors.Is(err, errChaseRecordNotReadyTimeout) {
		t.Fatalf("waitForChaseRecord() = %v, want errChaseRecordNotReadyTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("waitForChaseRecord() took %v, want bounded response wait", elapsed)
	}
}

func TestBuildChaseFFmpegArgsUsesGrowingEventPlaylist(t *testing.T) {
	cfg := LiveConfig{
		Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}},
	}

	args := BuildChaseFFmpegArgs(cfg, "/tmp/segments/default/chase/42", false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-hls_playlist_type event") {
		t.Fatalf("args = %q, want EVENT playlist", joined)
	}
	if !strings.Contains(joined, "-hls_list_size 0") {
		t.Fatalf("args = %q, want an unbounded playlist", joined)
	}
	if !strings.Contains(joined, "-hls_flags temp_file") {
		t.Fatalf("args = %q, want atomic playlist/segment writes", joined)
	}
	if strings.Contains(joined, "delete_segments") {
		t.Fatalf("args = %q, chase output must retain all segments", joined)
	}

	liveArgs := BuildLiveFFmpegArgs(cfg, "/tmp/segments/default/1", false)
	liveJoined := strings.Join(liveArgs, " ")
	if strings.Contains(liveJoined, "-hls_playlist_type event") || !strings.Contains(liveJoined, "delete_segments") {
		t.Fatalf("live args = %q, want the existing sliding live playlist", liveJoined)
	}
}

func TestFinishedChaseServesRetainedPlaylistWithoutRestarting(t *testing.T) {
	dir := t.TempDir()
	playlist := filepath.Join(dir, "h264.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:2.0,\nsegments/00001.ts\n#EXT-X-ENDLIST\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	close(ready)
	done := make(chan struct{})
	close(done)
	ls := &LiveStreamer{
		mirakc: chaseTestLiveClient{},
		site:   "default",
		cfg: LiveConfig{Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}}},
		chaseSessions: map[int64]*liveSession{
			42: {
				key:    sessionKey{kind: chaseSessionKind, id: 42},
				dir:    dir,
				ready:  ready,
				done:   done,
				cancel: func() {},
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sites/default/recordings/42/chase/playlist.m3u8?profile=h264", nil)
	resp := httptest.NewRecorder()
	ls.ChasePlaylistForTarget(resp, req, ChaseTarget{
		RecordingID:     42,
		Site:            "default",
		RecordID:        "record-42",
		Status:          "finished",
		RecordingStatus: "finished",
	})

	if resp.Code != http.StatusOK {
		t.Fatalf("finished chase playlist status = %d, want 200", resp.Code)
	}
	if got := resp.Body.String(); !strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Fatalf("finished chase playlist = %q, want retained ENDLIST", got)
	}

	delete(ls.chaseSessions, 42)
	resp = httptest.NewRecorder()
	ls.ChasePlaylistForTarget(resp, req, ChaseTarget{
		RecordingID:     42,
		Site:            "default",
		RecordID:        "record-42",
		Status:          "finished",
		RecordingStatus: "finished",
	})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("finished chase without retained session status = %d, want 404", resp.Code)
	}
}

func TestLiveAndChaseShareMaxSessions(t *testing.T) {
	readyLive := make(chan struct{})
	readyChase := make(chan struct{})
	close(readyLive)
	close(readyChase)
	ls := &LiveStreamer{
		cfg: LiveConfig{MaxSessions: 2},
		sessions: map[int64]*liveSession{
			1: {key: sessionKey{kind: liveSessionKind, id: 1}, serviceID: 1, ready: readyLive},
		},
		chaseSessions: map[int64]*liveSession{
			42: {key: sessionKey{kind: chaseSessionKind, id: 42}, ready: readyChase},
		},
	}

	_, err := ls.getOrCreateSessionOnceFor(context.Background(), sessionKey{
		kind: chaseSessionKind,
		id:   99,
	}, func(context.Context) (io.ReadCloser, error) {
		t.Fatal("session source called despite the combined session limit")
		return nil, nil
	})
	if !errors.Is(err, errSessionLimit) {
		t.Fatalf("getOrCreateSessionOnceFor() = %v, want errSessionLimit", err)
	}
}

type chaseTestLiveClient struct{}

func (chaseTestLiveClient) StreamService(context.Context, int64, int) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

func installCompletedChaseFFmpeg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffmpeg-chase-complete")
	script := `#!/bin/sh
segfile=""
playlist=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-hls_segment_filename" ]; then segfile="$a"; fi
  case "$a" in *.m3u8) playlist="$a";; esac
  prev="$a"
done
outdir=$(dirname "$playlist")
mkdir -p "$outdir/segments"
seg=$(printf '%s' "$segfile" | sed 's/%05d/00001/')
printf 'fake-ts' > "$seg"
cat > "$playlist" <<EOF
#EXTM3U
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-TARGETDURATION:2
#EXTINF:2.0,
segments/$(basename "$seg")
#EXT-X-ENDLIST
EOF
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func installFailedChaseFFmpeg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffmpeg-chase-fail")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompletedChaseRetainsEventFilesUntilIdleGC(t *testing.T) {
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      installCompletedChaseFFmpeg(t),
		SegmentDir:  t.TempDir(),
		MaxSessions: 2,
		IdleTimeout: time.Second,
		Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}},
	}
	ls := newLiveStreamer(chaseTestLiveClient{}, cfg)
	targetID := int64(42)
	s, err := ls.getOrCreateSessionFor(context.Background(), sessionKey{
		kind: chaseSessionKind,
		id:   targetID,
	}, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("input")), nil
	})
	if err != nil {
		t.Fatalf("starting chase session: %v", err)
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("completed chase ffmpeg did not exit")
	}

	playlist := filepath.Join(cfg.SegmentDir, "default", "chase", strconv.FormatInt(targetID, 10), "h264.m3u8")
	data, err := os.ReadFile(playlist)
	if err != nil {
		t.Fatalf("reading retained EVENT playlist: %v", err)
	}
	if !strings.Contains(string(data), "#EXT-X-ENDLIST") {
		t.Fatalf("retained playlist = %q, want ENDLIST", data)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(playlist), "segments", "h264_seg00001.ts")); err != nil {
		t.Fatalf("retained segment: %v", err)
	}

	ls.mu.Lock()
	_, present := ls.chaseSessions[targetID]
	ls.mu.Unlock()
	if !present {
		t.Fatal("completed chase session was removed before idle GC")
	}

	s.mu.Lock()
	s.lastAccess = time.Now().Add(-2 * time.Second)
	s.mu.Unlock()
	ls.reapIdleAt(time.Now())
	if _, err := os.Stat(filepath.Dir(playlist)); !os.IsNotExist(err) {
		t.Errorf("chase session directory still exists after idle GC, stat err = %v", err)
	}
	if ls.sessionCount() != 0 {
		t.Errorf("sessionCount after idle GC = %d, want 0", ls.sessionCount())
	}
}

func TestFailedChaseIsRemovedAndCanRestart(t *testing.T) {
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      installFailedChaseFFmpeg(t),
		SegmentDir:  t.TempDir(),
		MaxSessions: 1,
		IdleTimeout: time.Minute,
		Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}},
	}
	ls := newLiveStreamer(chaseTestLiveClient{}, cfg)
	t.Cleanup(ls.shutdown)

	start := func() *liveSession {
		s, err := ls.getOrCreateSessionFor(context.Background(), sessionKey{
			kind: chaseSessionKind,
			id:   43,
		}, func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("input")), nil
		})
		if err != nil {
			t.Fatalf("starting chase session: %v", err)
		}
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			t.Fatal("failed chase ffmpeg did not exit")
		}
		return s
	}

	first := start()
	ls.mu.Lock()
	_, present := ls.chaseSessions[first.key.id]
	ls.mu.Unlock()
	if present {
		t.Fatal("failed chase session remained in the session map")
	}
	if _, err := os.Stat(first.dir); !os.IsNotExist(err) {
		t.Fatalf("failed chase session directory still exists, stat err = %v", err)
	}

	second := start()
	if second == first {
		t.Fatal("restart reused the failed chase session")
	}
}

func TestEvictingCompletedChaseCleansRetainedFiles(t *testing.T) {
	setShortLiveMirakcReleaseWait(t, 0)
	cfg := LiveConfig{
		Enabled:     true,
		FFmpeg:      installCompletedChaseFFmpeg(t),
		SegmentDir:  t.TempDir(),
		MaxSessions: 1,
		IdleTimeout: time.Minute,
		Profiles: []LiveProfile{{
			Name:           "h264",
			VideoCodec:     "libx264",
			AudioCodec:     "aac",
			SegmentSeconds: 2,
			PlaylistSize:   6,
		}},
	}
	ls := newLiveStreamer(chaseTestLiveClient{}, cfg)
	t.Cleanup(ls.shutdown)

	chase, err := ls.getOrCreateSessionFor(context.Background(), sessionKey{
		kind: chaseSessionKind,
		id:   44,
	}, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("input")), nil
	})
	if err != nil {
		t.Fatalf("starting completed chase session: %v", err)
	}
	select {
	case <-chase.done:
	case <-time.After(2 * time.Second):
		t.Fatal("completed chase ffmpeg did not exit")
	}
	if _, err := os.Stat(chase.dir); err != nil {
		t.Fatalf("retained chase directory before eviction: %v", err)
	}

	chase.mu.Lock()
	chase.lastAccess = time.Now().Add(-cfg.idleEvictionThreshold() - time.Second)
	chase.mu.Unlock()

	live, err := ls.getOrCreateSessionFor(context.Background(), sessionKey{
		kind: liveSessionKind,
		id:   99,
	}, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("input")), nil
	})
	if err != nil {
		t.Fatalf("starting live session after chase eviction: %v", err)
	}
	select {
	case <-live.done:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement live ffmpeg did not exit")
	}

	if _, err := os.Stat(chase.dir); !os.IsNotExist(err) {
		t.Errorf("evicted completed chase directory still exists, stat err = %v", err)
	}
}
