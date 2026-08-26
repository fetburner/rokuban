package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/serverevent"
)

func TestProbeEncodeDuration_TimesOutWithoutDelayingEncode(t *testing.T) {
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return []byte("4.0\n"), nil
		}
	}

	started := time.Now()
	_, err := probeEncodeDuration(context.Background(), "ffprobe", "input.ts", 20*time.Millisecond, run)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("probe took %s; telemetry must time out before delaying encode", elapsed)
	}
}

func TestDrainLatestProgress_PrefersBufferedSampleAtTickBoundary(t *testing.T) {
	samples := make(chan time.Duration, 1)
	samples <- 3 * time.Second

	got, found := drainLatestProgress(samples, time.Second)
	if !found || got != 3*time.Second {
		t.Fatalf("latest = %s, found = %t; want buffered 3s", got, found)
	}
}

func TestEncodeProgressReporter_PublishesLatestValue(t *testing.T) {
	sent := make(chan serverevent.EncodeProgressEvent, 2)
	reporter := encodeProgressReporter{
		recordingID: 42,
		profile:     "mobile",
		duration:    4 * time.Second,
		interval:    20 * time.Millisecond,
		notify: func(_ context.Context, payload string) error {
			var event serverevent.EncodeProgressEvent
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				t.Errorf("unmarshal progress: %v", err)
			}
			sent <- event
			return nil
		},
		log: slog.Default(),
	}

	report, stop := reporter.start(context.Background())
	defer stop()
	report(time.Second)
	report(3 * time.Second)

	select {
	case got := <-sent:
		if got.Type != "encode-progress" || got.RecordingID != 42 || got.Profile != "mobile" || got.Progress != 0.75 {
			t.Fatalf("event = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for progress event")
	}

	select {
	case extra := <-sent:
		t.Fatalf("unchanged progress was sent again: %+v", extra)
	case <-time.After(2 * reporter.interval):
	}
}

func TestEncodeProgressReporter_DoesNotBlockFFmpegReader(t *testing.T) {
	notifyStarted := make(chan struct{})
	releaseNotify := make(chan struct{})
	reporter := encodeProgressReporter{
		recordingID: 42,
		profile:     "mobile",
		duration:    4 * time.Second,
		interval:    time.Millisecond,
		notify: func(_ context.Context, _ string) error {
			close(notifyStarted)
			<-releaseNotify
			return nil
		},
		log: slog.Default(),
	}

	report, stop := reporter.start(context.Background())
	report(time.Second)
	select {
	case <-notifyStarted:
	case <-time.After(time.Second):
		t.Fatal("notify did not start")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10_000 {
			report(time.Duration(i) * time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("report blocked while NOTIFY was waiting")
	}

	stop()
	close(releaseNotify)
}

func TestEncodeWorker_PublishesObservedProgress(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installProgressFakeFFmpeg(t)
	installFakeFFprobe(t, "4.0", true)

	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "show.m2ts", []string{"mobile"}, []byte("fake input"))

	listenConn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer listenConn.Release()
	if _, err := listenConn.Exec(context.Background(), "LISTEN rokuban"); err != nil {
		t.Fatal(err)
	}

	w := &EncodeWorker{
		Pool:       pool,
		MediaDir:   mediaDir,
		ScratchDir: t.TempDir(),
		FFmpeg:     ffmpegPath,
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{{
			Name: "mobile", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		}}},
	}
	workDone := make(chan error, 1)
	go func() {
		workDone <- w.Work(context.Background(), &river.Job[EncodeJobArgs]{
			JobRow: &rivertype.JobRow{},
			Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "mobile"},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		n, err := listenConn.Conn().WaitForNotification(ctx)
		if err != nil {
			t.Fatalf("waiting for encode progress: %v", err)
		}
		var got serverevent.EncodeProgressEvent
		if json.Unmarshal([]byte(n.Payload), &got) != nil || got.Type != serverevent.EncodeProgressEventType {
			continue
		}
		if got.RecordingID != recordingID || got.Profile != "mobile" || got.Progress != 0.25 {
			t.Fatalf("progress event = %+v", got)
		}
		break
	}
	if err := <-workDone; err != nil {
		t.Fatalf("Work: %v", err)
	}
}

func TestEncodeWorker_DoesNotInventProgressWithoutDuration(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	ffmpegPath := installProgressFakeFFmpeg(t)
	installFakeFFprobe(t, "", false)

	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "show.m2ts", []string{"mobile"}, []byte("fake input"))
	listenConn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer listenConn.Release()
	if _, err := listenConn.Exec(context.Background(), "LISTEN rokuban"); err != nil {
		t.Fatal(err)
	}

	w := &EncodeWorker{
		Pool: pool, MediaDir: mediaDir, ScratchDir: t.TempDir(), FFmpeg: ffmpegPath,
		Profiles: config.EncodeConfig{Profiles: []config.EncodeProfile{{
			Name: "mobile", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		}}},
	}
	if err := w.Work(context.Background(), &river.Job[EncodeJobArgs]{
		JobRow: &rivertype.JobRow{},
		Args:   EncodeJobArgs{RecordingID: recordingID, Profile: "mobile"},
	}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	for {
		n, err := listenConn.Conn().WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			t.Fatal(err)
		}
		var event serverevent.EncodeProgressEvent
		if json.Unmarshal([]byte(n.Payload), &event) == nil && event.Type == serverevent.EncodeProgressEventType {
			t.Fatalf("invented progress without duration: %+v", event)
		}
	}
}

func installFakeFFprobe(t *testing.T, duration string, succeed bool) {
	t.Helper()
	dir := t.TempDir()
	name := "ffprobe"
	if runtime.GOOS == "windows" {
		name = "ffprobe.bat"
	}
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n"
	if succeed {
		script += "printf '" + duration + "\\n'\n"
	} else {
		script += "exit 1\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
}

func installProgressFakeFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := `#!/bin/sh
set -e
input=""
output=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-i" ]; then input="$a"; fi
  prev="$a"
  output="$a"
done
printf 'out_time_ms=1000000\nprogress=continue\n'
sleep 2
printf 'out_time_ms=4000000\nprogress=end\n'
cp "$input" "$output"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
