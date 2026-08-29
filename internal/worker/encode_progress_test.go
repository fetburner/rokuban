package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
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

// stop（context.CancelFunc）はジョブ終了時（次の ticker を待たず）に最後に
// report された値を必ず 1 回 flush する。interval を長くしてティッカーが
// テスト中は絶対に発火しない条件を作り、flush が stop 由来であることを
// 確認する。stop 自体は（cancel と同じく）非同期にシグナルを送るだけなので、
// flush の完了は timeout 付きで待つ。
func TestEncodeProgressReporter_FlushesFinalValueOnStop(t *testing.T) {
	sent := make(chan serverevent.EncodeProgressEvent, 1)
	reporter := encodeProgressReporter{
		recordingID: 42,
		profile:     "mobile",
		duration:    4 * time.Second,
		interval:    time.Hour,
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
	report(4 * time.Second)
	stop()

	select {
	case got := <-sent:
		if got.Progress != 1 {
			t.Fatalf("event = %+v, want Progress = 1 (final value flushed on stop)", got)
		}
	case <-time.After(time.Second):
		t.Fatal("stop() did not flush the final report()ed value")
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
	var notifyStartedOnce sync.Once
	notifyStarted := make(chan struct{})
	releaseNotify := make(chan struct{})
	reporter := encodeProgressReporter{
		recordingID: 42,
		profile:     "mobile",
		duration:    4 * time.Second,
		interval:    time.Millisecond,
		notify: func(_ context.Context, _ string) error {
			// stop() は最後に report() された値が変わっていれば flush し直す
			// （TestEncodeProgressReporter_FlushesFinalValueOnStop）ので、この
			// テストの終盤（report が 10,000 回呼ばれた後の stop()）でも
			// notify がもう一度呼ばれうる。notifyStarted の close は 1 回だけに
			// 留める（2 回目の close はパニックする）。
			notifyStartedOnce.Do(func() { close(notifyStarted) })
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
