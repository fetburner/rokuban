package streamer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBuildSegmentTransformArgsUsesPTSPreRollTrimAndRestoresRecordingTime(t *testing.T) {
	profile := LiveProfile{
		Name: "test", VideoCodec: "libx264", AudioCodec: "aac",
		SegmentSeconds: 2, PlaylistSize: 0,
	}
	origin := 5400*mpegTSTimestampClock + 902
	args, err := buildSegmentTransformArgs(
		LiveConfig{FFmpeg: "ffmpeg", Profiles: []LiveProfile{profile}},
		profile,
		SegmentTransformInput{Path: "/dev/fd/3", OriginPTS: origin, AudioStreamCount: 1},
		2,
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if indexOf(args, "-copyts") < 0 {
		t.Fatalf("args do not contain -copyts: %v", args)
	}
	assertArgPair(t, args, "-ss", "5402.010022222")
	assertArgPair(t, args, "-vf", "trim=start=5404.010022222,setpts=PTS-STARTPTS")
	assertArgPair(t, args, "-filter:a:0", "atrim=start=5404.010022222,asetpts=PTS-STARTPTS")
	if index := indexOf(args, "-filter:a:1"); index < 0 || index+1 >= len(args) ||
		!strings.Contains(args[index+1], "atrim=start=5404.010022222,asetpts=PTS-STARTPTS") {
		t.Fatalf("main audio filter = %v, want trim after the dual-mono pan", args)
	}
	assertArgPair(t, args, "-start_number", "2")
	assertArgPair(t, args, "-output_ts_offset", "5404.010022222")
	assertArgPair(t, args, "-muxpreload", "0")
	assertArgPair(t, args, "-muxdelay", "0")
	assertArgPair(t, args, "-force_key_frames", "expr:gte(t,n_forced*2)")
	assertArgPair(t, args, "-hls_flags", "temp_file")
	if indexOf(args, "-seek_timestamp") >= 0 {
		t.Fatalf("args contain direct absolute seeking: %v", args)
	}
}

func TestBuildSegmentTransformArgsSeeksRelativeToFormatStart(t *testing.T) {
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2}
	args, err := buildSegmentTransformArgs(
		LiveConfig{FFmpeg: "ffmpeg", Profiles: []LiveProfile{profile}},
		profile,
		SegmentTransformInput{
			Path: "/dev/fd/3", OriginPTS: 5400*mpegTSTimestampClock + 902,
			FormatStartPTS: 5400 * mpegTSTimestampClock, AudioStreamCount: 1,
		},
		2,
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	assertArgPair(t, args, "-ss", "2.010022222")
}

func TestNewSegmentManagerValidatesCacheWindowAndCleansOwnedScratch(t *testing.T) {
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2}
	base := SegmentTransformOptions{
		Config:           LiveConfig{Profiles: []LiveProfile{profile}},
		ScratchRoot:      t.TempDir(),
		CacheBytes:       48,
		MaxSegmentBytes:  1,
		MaxConcurrent:    2,
		PrefetchSegments: 5,
	}
	if _, err := NewSegmentManager(base); err == nil {
		t.Fatal("cache limit equal to the minimum should be rejected")
	}
	base.CacheBytes++
	stale := filepath.Join(base.ScratchRoot, "recording-hls", "work", "stale")
	if err := os.MkdirAll(filepath.Dir(stale), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewSegmentManager(base)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSegmentManager(t, manager)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale scratch stat = %v, want not exist", err)
	}
}

func TestReadSegmentPlaylistParsesStableAndTemporaryNames(t *testing.T) {
	dir := t.TempDir()
	playlist := filepath.Join(dir, "profile.0.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:2.0,\nsegments/profile.0_seg00007.ts\n#EXT-X-ENDLIST\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	segments, endList, err := readSegmentPlaylist(playlist, "profile", "0")
	if err != nil || !endList || !segments[7] {
		t.Fatalf("read playlist = (%v, %v, %v), want segment 7 and ENDLIST", segments, endList, err)
	}
	for name, want := range map[string]int64{
		"profile.0_seg00009.ts":     9,
		"profile.0_seg00010.ts.tmp": 10,
	} {
		got, ok := parseOutputSegmentNumber(name, "profile", "0")
		if !ok || got != want {
			t.Fatalf("parseOutputSegmentNumber(%q) = (%d, %v), want (%d, true)", name, got, ok, want)
		}
	}
}

func TestSegmentTransformRealFFmpegKeepsRequestedPTSGrid(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "old-audio-pes.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 20)
	originPTS := firstPacketPTS(t, ffprobe, inputPath, "v:0")
	mainAudioPTS := firstPacketPTS(t, ffprobe, inputPath, "a:0")
	audioPTS := firstPacketPTS(t, ffprobe, inputPath, "a:1")
	// MPEG-TS wraps at 2^33 ticks. The main audio starts with video; the other
	// audio rendition starts one 5,400-second revolution behind, then follows it.
	wrapSeconds := float64(uint64(1)<<33) / float64(mpegTSTimestampClock)
	mainDifference := float64(mainAudioPTS-originPTS) / float64(mpegTSTimestampClock)
	if mainDifference < -0.1 || mainDifference > 0.1 {
		t.Fatalf("main audio PES offset = %.3f s, want aligned to video", mainDifference)
	}
	difference := float64(audioPTS-originPTS) / float64(mpegTSTimestampClock)
	difference -= mathRound(difference/wrapSeconds) * wrapSeconds
	if difference > -5399 || difference < -5401 {
		t.Fatalf("wrapped first audio PES offset = %.3f s, want about -5400 s", difference)
	}

	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	input := SegmentTransformInput{
		OpenFile:         func(context.Context) (*os.File, error) { return os.Open(inputPath) },
		OriginPTS:        originPTS,
		FormatStartPTS:   formatStartPTS(t, ffprobe, inputPath),
		AudioStreamCount: 2,
	}
	key := SegmentKey{"recording", "generation-1", "test", "video", 3}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	file, err := manager.Get(ctx, key, input)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	gotPTS := firstPacketPTS(t, ffprobe, file.Name(), "v:0")
	wantPTS := float64(originPTS)/float64(mpegTSTimestampClock) + float64(key.Number*2)
	if delta := float64(gotPTS)/float64(mpegTSTimestampClock) - wantPTS; delta < -0.04 || delta > 0.04 {
		t.Fatalf("segment %d first video PTS = %.6f, want %.6f ± one 25 fps frame", key.Number, float64(gotPTS)/float64(mpegTSTimestampClock), wantPTS)
	}
	previous := key
	previous.Number--
	if lease, ok, err := manager.cache.acquire(previous); err != nil || ok {
		if lease != nil {
			_ = lease.file.Close()
		}
		t.Fatalf("one-before segment %#v cached = (%v, %v), want absent", previous, ok, err)
	}
}

func TestSegmentTransformSIGSTOPAtPrefetchThenSIGCONTOnDemand(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 30)
	originPTS := firstPacketPTS(t, ffprobe, inputPath, "v:0")
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(t, ffprobe, inputPath, originPTS, []string{"-re"})
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	firstKey := SegmentKey{"recording", "generation-1", "test", "video", 1}
	file, err := manager.Get(ctx, firstKey, input)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	task := waitForTaskPause(t, manager, transformPausedAhead, 20*time.Second)
	manager.mu.Lock()
	current, requested, prefetch := task.currentNumber, task.maxRequested, manager.prefetchSegments
	manager.mu.Unlock()
	if current <= requested+int64(prefetch) {
		t.Fatalf("paused at segment %d for request %d with prefetch %d", current, requested, prefetch)
	}
	nextKey := firstKey
	nextKey.Number = current
	nextContext, nextCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer nextCancel()
	nextFile, err := manager.Get(nextContext, nextKey, input)
	if err != nil {
		t.Fatal(err)
	}
	_ = nextFile.Close()
	manager.mu.Lock()
	paused := task.pauseReason
	manager.mu.Unlock()
	if paused != transformRunning {
		t.Fatalf("task pause reason after new demand = %d, want running", paused)
	}
}

func TestSegmentTransformConcurrencyPausesLeastRecentlyRequested(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 30)
	originPTS := firstPacketPTS(t, ffprobe, inputPath, "v:0")
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 2, 128<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(t, ffprobe, inputPath, originPTS, []string{"-re"})

	type pending struct {
		cancel context.CancelFunc
		done   <-chan error
	}
	start := func(recordingID string) pending {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := manager.Get(ctx, SegmentKey{recordingID, "generation-1", "test", "video", 3}, input)
			done <- err
		}()
		return pending{cancel: cancel, done: done}
	}
	a := start("a")
	waitForTransformCount(t, manager, 1)
	time.Sleep(40 * time.Millisecond)
	b := start("b")
	waitForTransformCount(t, manager, 2)
	time.Sleep(40 * time.Millisecond)
	manager.mu.Lock()
	var aTask *segmentTransformTask
	for group, tasks := range manager.tasks {
		if group.recordingID == "a" {
			for task := range tasks {
				aTask = task
			}
		}
	}
	manager.mu.Unlock()
	if aTask == nil {
		t.Fatal("first transform disappeared before refreshed demand")
	}
	aRefreshKey := SegmentKey{"a", "generation-1", "test", "video", 3}
	manager.mu.Lock()
	aRefreshKey.Number = aTask.currentNumber + 3
	manager.mu.Unlock()
	aRefreshCtx, aRefreshCancel := context.WithCancel(context.Background())
	aRefreshDone := startSegmentGet(manager, aRefreshCtx, aRefreshKey, input)
	waitForSegmentWaiterCount(t, manager, aTask, aRefreshKey, 1, 2*time.Second)
	c := start("c")
	waitForTransformCount(t, manager, 3)

	manager.mu.Lock()
	var states = make(map[string]transformPauseReason)
	for group, tasks := range manager.tasks {
		for task := range tasks {
			states[group.recordingID] = task.pauseReason
		}
	}
	running := manager.running
	manager.mu.Unlock()
	if states["a"] != transformRunning || states["b"] != transformPausedCapacity || states["c"] != transformRunning {
		t.Fatalf("transform states = %v, want refreshed a and new c running with oldest-demand b paused", states)
	}
	if running != 2 {
		t.Fatalf("running transforms = %d, want 2", running)
	}
	aRefreshCancel()
	for _, item := range []pending{a, b, c} {
		item.cancel()
	}
	for _, item := range []pending{a, b, c} {
		select {
		case <-item.done:
		case <-time.After(2 * time.Second):
			t.Fatal("canceled segment request did not return")
		}
	}
	select {
	case err := <-aRefreshDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("refreshed request error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refreshed segment request did not return after cancellation")
	}
}

func TestSegmentTransformWaitWindowReusesAndRestartsWithRealFFmpeg(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 90)
	originPTS := firstPacketPTS(t, ffprobe, inputPath, "v:0")
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 2, 128<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(t, ffprobe, inputPath, originPTS, []string{"-re"})
	group := segmentTransformGroup{recordingID: "recording", generation: "generation-1", profile: "test"}

	firstKey := SegmentKey{"recording", "generation-1", "test", "video", 3}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 8*time.Second)
	firstFile, err := manager.Get(firstCtx, firstKey, input)
	firstCancel()
	if err != nil {
		t.Fatal(err)
	}
	_ = firstFile.Close()

	manager.mu.Lock()
	var firstTask *segmentTransformTask
	for task := range manager.tasks[group] {
		firstTask = task
	}
	if firstTask == nil {
		manager.mu.Unlock()
		t.Fatal("first transform completed before wait-window assertions")
	}
	if firstTask.startNumber != firstKey.Number-1 {
		manager.mu.Unlock()
		t.Fatalf("transform start number = %d, want requested segment - 1 = %d", firstTask.startNumber, firstKey.Number-1)
	}
	current := firstTask.currentNumber
	manager.mu.Unlock()

	insideKey := firstKey
	insideKey.Number = current + 11
	insideCtx, insideCancel := context.WithCancel(context.Background())
	insideDone := startSegmentGet(manager, insideCtx, insideKey, input)
	waitForSegmentWaiter(t, manager, firstTask, insideKey, 2*time.Second)
	manager.mu.Lock()
	insideTaskCount := len(manager.tasks[group])
	selectedInside := manager.findTaskLocked(group, insideKey.Number, firstTask.profile.SegmentSeconds)
	manager.mu.Unlock()
	if insideTaskCount != 1 || selectedInside != firstTask {
		insideCancel()
		t.Fatalf("request inside 24-second window selected %p with %d transforms, want original %p with 1", selectedInside, insideTaskCount, firstTask)
	}
	insideCancel()
	if err := <-insideDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("inside-window request error = %v, want context canceled", err)
	}

	manager.mu.Lock()
	current = firstTask.currentNumber
	manager.mu.Unlock()
	outsideKey := firstKey
	outsideKey.Number = current + 16
	outsideCtx, outsideCancel := context.WithCancel(context.Background())
	outsideDone := startSegmentGet(manager, outsideCtx, outsideKey, input)
	waitForTransformCount(t, manager, 2)
	manager.mu.Lock()
	selectedOutside := manager.findTaskLocked(group, outsideKey.Number, firstTask.profile.SegmentSeconds)
	outsideTaskCount := len(manager.tasks[group])
	manager.mu.Unlock()
	if outsideTaskCount != 2 || selectedOutside == nil || selectedOutside == firstTask {
		outsideCancel()
		t.Fatalf("request outside 24-second window selected %p with %d transforms, want a new transform beside %p", selectedOutside, outsideTaskCount, firstTask)
	}
	outsideCancel()
	if err := <-outsideDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("outside-window request error = %v, want context canceled", err)
	}
}

func TestSegmentTransformKillDoesNotCacheInterruptedTail(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 20)
	originPTS := firstPacketPTS(t, ffprobe, inputPath, "v:0")
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(t, ffprobe, inputPath, originPTS, []string{"-re"})
	key := SegmentKey{"recording", "generation-1", "test", "video", 3}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	file, err := manager.Get(ctx, key, input)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	manager.mu.Lock()
	var task *segmentTransformTask
	for _, tasks := range manager.tasks {
		for candidate := range tasks {
			task = candidate
		}
	}
	if task == nil {
		manager.mu.Unlock()
		t.Fatal("transform completed before the tail-kill assertion")
	}
	err = task.cmd.Process.Kill()
	manager.mu.Unlock()
	if err != nil {
		t.Fatalf("killing transform: %v", err)
	}
	select {
	case <-task.done:
	case <-time.After(3 * time.Second):
		t.Fatal("killed transform did not finish")
	}
	tail := key
	tail.Number++
	if lease, ok, err := manager.cache.acquire(tail); err != nil || ok {
		if lease != nil {
			_ = lease.file.Close()
		}
		t.Fatalf("interrupted tail %#v cached = (%v, %v), want absent", tail, ok, err)
	}
}

func newRealSegmentManager(
	t *testing.T,
	ffmpeg string,
	scratch string,
	concurrency int,
	cacheBytes int64,
	maxSegmentBytes int64,
) *SegmentManager {
	t.Helper()
	profile := LiveProfile{
		Name: "test", VideoCodec: "libx264", AudioCodec: "aac",
		Preset: "ultrafast", SegmentSeconds: 2, PlaylistSize: 0,
	}
	manager, err := NewSegmentManager(SegmentTransformOptions{
		Config:          LiveConfig{FFmpeg: ffmpeg, Profiles: []LiveProfile{profile}},
		ScratchRoot:     scratch,
		CacheBytes:      cacheBytes,
		MaxSegmentBytes: maxSegmentBytes,
		MaxConcurrent:   concurrency,
		IdleTimeout:     2 * time.Minute,
		PollInterval:    2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func closeSegmentManager(t *testing.T, manager *SegmentManager) {
	t.Helper()
	if err := manager.Close(); err != nil {
		t.Errorf("closing segment manager: %v", err)
	}
}

func testSegmentInput(t *testing.T, ffprobe, path string, originPTS int64, inputArgs []string) SegmentTransformInput {
	return SegmentTransformInput{
		OpenFile:         func(context.Context) (*os.File, error) { return os.Open(path) },
		InputArgs:        inputArgs,
		OriginPTS:        originPTS,
		FormatStartPTS:   formatStartPTS(t, ffprobe, path),
		AudioStreamCount: 2,
	}
}

func writeOldAudioPESTestTS(t *testing.T, ffmpeg, path string, seconds int) {
	t.Helper()
	video := fmt.Sprintf("testsrc2=size=64x64:rate=25:duration=%d", seconds)
	audio := fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", seconds)
	filter := "[2:a]asetpts=PTS+gte(N\\,1)*5400/TB[a]"
	cmd := exec.Command(ffmpeg, "-v", "error", "-y",
		"-itsoffset", "5400",
		"-f", "lavfi", "-i", video,
		"-itsoffset", "5400",
		"-f", "lavfi", "-i", audio,
		"-f", "lavfi", "-i", audio,
		"-filter_complex", filter,
		"-map", "0:v:0", "-map", "1:a:0", "-map", "[a]",
		"-c:v", "mpeg2video", "-b:v", "500k", "-g", "25",
		"-c:a", "mp2", "-f", "mpegts", "-mpegts_copyts", "1", "-muxdelay", "0",
		path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generating wrapped PTS test TS: %v\n%s", err, out)
	}
}

func firstPacketPTS(t *testing.T, ffprobe, path, stream string) int64 {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", stream,
		"-show_packets", "-show_entries", "packet=pts", "-read_intervals", "%+#1",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("probing first %s packet from %s: %v", stream, path, err)
	}
	field := strings.Split(strings.TrimSpace(string(out)), ",")[0]
	pts, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		t.Fatalf("parsing first packet pts %q: %v", field, err)
	}
	return pts
}

func formatStartPTS(t *testing.T, ffprobe, path string) int64 {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=start_time",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("probing format start_time from %s: %v", path, err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parsing format start_time %q: %v", out, err)
	}
	return int64(math.Round(seconds * float64(mpegTSTimestampClock)))
}

func waitForTaskPause(t *testing.T, manager *SegmentManager, reason transformPauseReason, timeout time.Duration) *segmentTransformTask {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		for _, tasks := range manager.tasks {
			for task := range tasks {
				if task.pauseReason == reason {
					manager.mu.Unlock()
					return task
				}
			}
		}
		manager.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no transform reached pause reason %d within %v", reason, timeout)
	return nil
}

func startSegmentGet(manager *SegmentManager, ctx context.Context, key SegmentKey, input SegmentTransformInput) <-chan error {
	done := make(chan error, 1)
	go func() {
		file, err := manager.Get(ctx, key, input)
		if file != nil {
			_ = file.Close()
		}
		done <- err
	}()
	return done
}

func waitForSegmentWaiter(t *testing.T, manager *SegmentManager, task *segmentTransformTask, key SegmentKey, timeout time.Duration) {
	waitForSegmentWaiterCount(t, manager, task, key, 1, timeout)
}

func waitForSegmentWaiterCount(t *testing.T, manager *SegmentManager, task *segmentTransformTask, key SegmentKey, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		waiters := len(task.waiters[key])
		manager.mu.Unlock()
		if waiters >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	manager.mu.Lock()
	waiters := len(task.waiters[key])
	manager.mu.Unlock()
	t.Fatalf("transform registered %d waiters for %#v within %v, want at least %d", waiters, key, timeout, count)
}

func waitForTransformCount(t *testing.T, manager *SegmentManager, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		total := 0
		for _, tasks := range manager.tasks {
			total += len(tasks)
		}
		manager.mu.Unlock()
		if total == count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("transform count did not reach %d", count)
}

func assertArgPair(t *testing.T, args []string, flag, want string) {
	t.Helper()
	index := indexOf(args, flag)
	if index < 0 || index+1 >= len(args) || args[index+1] != want {
		t.Fatalf("args[%q] = %v, want %q", flag, args, want)
	}
}

func mathRound(value float64) float64 {
	if value < 0 {
		return float64(int64(value - 0.5))
	}
	return float64(int64(value + 0.5))
}

func TestSegmentTransformWaitWindowIs24Seconds(t *testing.T) {
	group := segmentTransformGroup{recordingID: "recording", generation: "generation", profile: "test"}
	task := &segmentTransformTask{group: group, currentNumber: 10}
	manager := &SegmentManager{
		tasks: map[segmentTransformGroup]map[*segmentTransformTask]struct{}{
			group: {task: {}},
		},
	}
	if got := manager.findTaskLocked(group, 18, 3); got != task {
		t.Fatalf("request at the 24-second edge selected %p, want current task %p", got, task)
	}
	if got := manager.findTaskLocked(group, 19, 3); got != nil {
		t.Fatalf("request 27 seconds ahead selected task %p, want a new transform", got)
	}
}

func TestSegmentTransformDoesNotCachePlaylistEntryUntilNextSegmentStarts(t *testing.T) {
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2}
	manager, err := NewSegmentManager(SegmentTransformOptions{
		Config:          LiveConfig{Profiles: []LiveProfile{profile}},
		ScratchRoot:     t.TempDir(),
		CacheBytes:      64 << 20,
		MaxSegmentBytes: 2 << 20,
		MaxConcurrent:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSegmentManager(t, manager)

	group := segmentTransformGroup{recordingID: "recording", generation: "generation-1", profile: "test"}
	workDir := filepath.Join(t.TempDir(), "work")
	segmentDir := filepath.Join(workDir, "segments")
	if err := os.MkdirAll(segmentDir, 0o750); err != nil {
		t.Fatal(err)
	}
	playlist := filepath.Join(workDir, "test.0.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:2.0,\nsegments/test.0_seg00001.ts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	segmentPath := filepath.Join(segmentDir, "test.0_seg00001.ts")
	if err := os.WriteFile(segmentPath, []byte("partial-segment"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := &segmentTransformTask{
		group:         group,
		profile:       profile,
		requestStart:  1,
		startNumber:   0,
		currentNumber: 0,
		maxRequested:  1,
		workDir:       workDir,
	}
	if err := manager.scanTransform(task, false); err != nil {
		t.Fatal(err)
	}
	key := SegmentKey{"recording", "generation-1", "test", "video", 1}
	if lease, ok, err := manager.cache.acquire(key); err != nil || ok {
		if lease != nil {
			_ = lease.file.Close()
		}
		t.Fatalf("playlist entry without a started next segment cached = (%v, %v), want absent", ok, err)
	}
}

func TestSegmentTransformWaitsForCacheSpaceThenResumes(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 20)
	originPTS := firstPacketPTS(t, ffprobe, inputPath, "v:0")
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 25<<20, 1<<20)
	defer closeSegmentManager(t, manager)

	fillKey := SegmentKey{"occupied", "generation-1", "test", "video", 0}
	fillPath := filepath.Join(manager.cache.root, "held.tmp")
	fillSize := manager.cache.maxBytes - 1
	if err := os.WriteFile(fillPath, make([]byte, fillSize), 0o600); err != nil {
		t.Fatal(err)
	}
	fillLease, _, err := manager.cache.publish(fillPath, fillKey)
	if err != nil {
		t.Fatal(err)
	}

	input := testSegmentInput(t, ffprobe, inputPath, originPTS, []string{"-re"})
	key := SegmentKey{"recording", "generation-1", "test", "video", 3}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		file, getErr := manager.Get(ctx, key, input)
		if file != nil {
			_ = file.Close()
		}
		result <- getErr
	}()

	task := waitForTaskPause(t, manager, transformPausedCache, 10*time.Second)
	manager.mu.Lock()
	paused := task.pauseReason
	manager.mu.Unlock()
	if paused != transformPausedCache {
		t.Fatalf("cache-full task pause reason = %d, want cache pause", paused)
	}
	select {
	case err := <-result:
		t.Fatalf("request completed while cache space was protected: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	fillLease.release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("request did not resume after cache space became available")
	}
	if lease, ok, err := manager.cache.acquire(fillKey); err != nil || ok {
		if lease != nil {
			_ = lease.file.Close()
		}
		t.Fatalf("oversized LRU filler survived eviction: ok=%v err=%v", ok, err)
	}
}

func TestSegmentTransformStopsAfterIdleTimeout(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 10)
	originPTS := firstPacketPTS(t, ffprobe, inputPath, "v:0")
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 1}
	manager, err := NewSegmentManager(SegmentTransformOptions{
		Config:      LiveConfig{FFmpeg: ffmpeg, Profiles: []LiveProfile{profile}},
		ScratchRoot: t.TempDir(), CacheBytes: 64 << 20, MaxSegmentBytes: 2 << 20,
		MaxConcurrent: 1, IdleTimeout: 3 * time.Second, PollInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSegmentManager(t, manager)

	key := SegmentKey{"recording", "generation-1", "test", "video", 1}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	file, err := manager.Get(ctx, key, testSegmentInput(t, ffprobe, inputPath, originPTS, []string{"-re"}))
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	manager.mu.Lock()
	var task *segmentTransformTask
	for _, tasks := range manager.tasks {
		for candidate := range tasks {
			task = candidate
		}
	}
	manager.mu.Unlock()
	if task == nil {
		t.Fatal("transform completed before its idle timeout")
	}
	select {
	case <-task.done:
		t.Fatal("transform stopped before its three-second idle timeout")
	default:
	}
	select {
	case <-task.done:
	case <-time.After(5 * time.Second):
		t.Fatal("transform did not exit after the three-second idle timeout")
	}
}

func TestSegmentTransformNormalEOFPublishesFinalShortSegment(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "short.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 10)
	originPTS := firstPacketPTS(t, ffprobe, inputPath, "v:0")
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
	defer closeSegmentManager(t, manager)

	key := SegmentKey{"recording", "generation-1", "test", "video", 4}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	file, err := manager.Get(ctx, key, testSegmentInput(t, ffprobe, inputPath, originPTS, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	gotPTS := firstPacketPTS(t, ffprobe, file.Name(), "v:0")
	wantPTS := originPTS + key.Number*2*mpegTSTimestampClock
	delta := gotPTS - wantPTS
	if delta < -3600 || delta > 3600 {
		t.Fatalf("final short segment PTS = %d ticks, want %d ± one 25 fps frame", gotPTS, wantPTS)
	}
}

func TestSegmentTransformStopsAtAnAlreadyCachedNextSegment(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 20)
	originPTS := firstPacketPTS(t, ffprobe, inputPath, "v:0")
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	key := SegmentKey{"recording", "generation-1", "test", "video", 3}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	file, err := manager.Get(ctx, key, testSegmentInput(t, ffprobe, inputPath, originPTS, []string{"-re"}))
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	manager.mu.Lock()
	var task *segmentTransformTask
	for _, tasks := range manager.tasks {
		for candidate := range tasks {
			task = candidate
		}
	}
	if task == nil {
		manager.mu.Unlock()
		t.Fatal("transform completed before cached-position check")
	}
	if task.pauseReason == transformRunning {
		if err := task.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			manager.mu.Unlock()
			t.Fatalf("pausing transform for deterministic cache check: %v", err)
		}
		manager.running--
	}
	task.pauseReason = transformPausedAhead
	manager.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	manager.mu.Lock()
	current := task.currentNumber
	manager.mu.Unlock()
	if current < task.requestStart {
		t.Fatalf("transform position = %d, request start = %d", current, task.requestStart)
	}

	nextKey := key
	nextKey.Number = current + 2
	nextContext, nextCancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer nextCancel()
	nextDone := startSegmentGet(manager, nextContext, nextKey, testSegmentInput(t, ffprobe, inputPath, originPTS, []string{"-re"}))
	waitForSegmentWaiter(t, manager, task, nextKey, 2*time.Second)
	manager.mu.Lock()
	if task.pauseReason == transformRunning {
		if err := task.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			manager.mu.Unlock()
			t.Fatalf("pausing transform after registering future waiter: %v", err)
		}
		manager.running--
		task.pauseReason = transformPausedAhead
	}
	current = task.currentNumber
	manager.mu.Unlock()

	var publishers []*segmentLease
	for _, rendition := range segmentRenditions {
		cachedKey := key
		cachedKey.Rendition = rendition
		cachedKey.Number = current
		path := filepath.Join(manager.cache.root, "winner-"+rendition)
		if err := os.WriteFile(path, []byte("winner-"+rendition), 0o600); err != nil {
			t.Fatal(err)
		}
		lease, _, err := manager.cache.publish(path, cachedKey)
		if err != nil {
			t.Fatal(err)
		}
		publishers = append(publishers, lease)
	}
	select {
	case <-task.done:
	case <-time.After(3 * time.Second):
		t.Fatal("transform did not stop at the cached next segment")
	}
	manager.mu.Lock()
	stopped := task.stopping
	manager.mu.Unlock()
	if !stopped {
		t.Fatal("transform did not mark itself stopped")
	}
	select {
	case err := <-nextDone:
		if err != nil {
			t.Fatalf("future request was not resumed by a replacement transform: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("future request did not complete after catch-up replaced its transform")
	}
	for _, lease := range publishers {
		lease.release()
	}
	for _, rendition := range segmentRenditions {
		cachedKey := key
		cachedKey.Rendition = rendition
		cachedKey.Number = current
		lease, ok, err := manager.cache.acquire(cachedKey)
		if err != nil || !ok {
			t.Fatalf("cached winner %q missing after stop: ok=%v err=%v", rendition, ok, err)
		}
		got, readErr := io.ReadAll(lease.file)
		lease.release()
		if readErr != nil || string(got) != "winner-"+rendition {
			t.Fatalf("cached winner %q = %q (err %v)", rendition, got, readErr)
		}
	}
}
