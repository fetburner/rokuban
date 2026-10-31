package streamer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
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
	assertArgPair(t, args, "-vf", `trim=start=5404.010022222,setpts=if(eq(N\,0)\,0\,PTS-5404.010022222/TB)`)
	assertArgPair(t, args, "-filter:a:0", "atrim=start=5404.010022222,asetpts=PTS-5404.010022222/TB")
	if index := indexOf(args, "-filter:a:1"); index < 0 || index+1 >= len(args) ||
		!strings.Contains(args[index+1], "atrim=start=5404.010022222,asetpts=PTS-5404.010022222/TB") {
		t.Fatalf("main audio filter = %v, want trim after the dual-mono pan", args)
	}
	assertArgPair(t, args, "-start_number", "2")
	assertArgPair(t, args, "-output_ts_offset", "5404.010022222")
	assertArgPair(t, args, "-enc_time_base:v", "1/90000")
	assertArgPair(t, args, "-fps_mode:v", "passthrough")
	assertArgPair(t, args, "-muxpreload", "0")
	assertArgPair(t, args, "-muxdelay", "0")
	assertArgPair(t, args, "-force_key_frames", "expr:gte(t,n_forced*2)")
	assertArgPair(t, args, "-hls_flags", "temp_file")
	// 絶対秒の -ss と 3 つのフラグ。-noaccurate_seek が無いと、揃った TS で出力が空になった（実 ffmpeg）。
	seek, ss := indexOf(args, "-seek_timestamp"), indexOf(args, "-ss")
	if indexOf(args, "-copyts") < 0 || seek < 0 || args[seek+1] != "1" || indexOf(args, "-noaccurate_seek") < 0 || ss < 0 {
		t.Fatalf("args = %v, want -copyts -seek_timestamp 1 -noaccurate_seek before -ss", args)
	}
	for _, flag := range []string{"-copyts", "-seek_timestamp", "-noaccurate_seek"} {
		if indexOf(args, flag) > ss {
			t.Fatalf("%s must come before -ss: %v", flag, args)
		}
	}
	if ss > indexOf(args, "-i") {
		t.Fatalf("-ss must be an input-side option: %v", args)
	}
}

func TestSegmentTransformDoesNotJoinTempAndOriginalInputs(t *testing.T) {
	ffmpeg := filepath.Join(t.TempDir(), "sleeping-ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(t.TempDir(), "input.ts")
	if err := os.WriteFile(inputPath, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2}
	manager, err := NewSegmentManager(SegmentTransformOptions{
		Config:           LiveConfig{FFmpeg: ffmpeg, Profiles: []LiveProfile{profile}},
		ScratchRoot:      t.TempDir(),
		CacheBytes:       1 << 20,
		MaxSegmentBytes:  1,
		MaxConcurrent:    3,
		PrefetchSegments: 0,
		IdleTimeout:      time.Minute,
		PollInterval:     time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSegmentManager: %v", err)
	}
	defer closeSegmentManager(t, manager)
	baseInput := SegmentTransformInput{
		OpenFile:         func(context.Context) (*os.File, error) { return os.Open(inputPath) },
		AudioStreamCount: 1,
	}
	key := SegmentKey{RecordingID: "recording", Generation: "generation", Profile: "test", Rendition: "video", Number: 0}
	for _, tc := range []struct {
		name string
		kind store.RecordingHLSInputKind
	}{
		{name: "temp", kind: store.RecordingHLSInputTemp},
		{name: "original", kind: store.RecordingHLSInputOriginal},
	} {
		input := baseInput
		input.Kind = tc.kind
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		_, getErr := manager.Get(ctx, key, input)
		cancel()
		if !errors.Is(getErr, context.DeadlineExceeded) {
			t.Fatalf("Get(input %q) error = %v, want deadline while fake ffmpeg sleeps", tc.name, getErr)
		}
	}
	if _, active := manager.Stats(); active != 2 {
		t.Fatalf("active transforms after same URL requested from temp and original = %d, want 2", active)
	}
}

func TestBuildSegmentTransformArgsKeepsGrowingInputOptionsBeforeFD(t *testing.T) {
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2}
	args, err := buildSegmentTransformArgs(
		LiveConfig{FFmpeg: "ffmpeg", Profiles: []LiveProfile{profile}},
		profile,
		SegmentTransformInput{
			Kind:             store.RecordingHLSInputTemp,
			Path:             "/dev/fd/3",
			InputArgs:        []string{"-follow", "1", "-rw_timeout", "90000000"},
			OriginPTS:        5400 * mpegTSTimestampClock,
			AudioStreamCount: 1,
		},
		0,
		t.TempDir(),
	)
	if err != nil {
		t.Fatalf("buildSegmentTransformArgs: %v", err)
	}
	inputIndex := indexOf(args, "-i")
	for _, pair := range [][2]string{{"-follow", "1"}, {"-rw_timeout", "90000000"}} {
		index := indexOf(args, pair[0])
		if index < 0 || index+1 >= inputIndex || args[index+1] != pair[1] {
			t.Errorf("input option %q is not before -i with value %q: %v", pair[0], pair[1], args)
		}
	}
}

func TestGrowingTempFollowReadsAppendedBytesThroughFD3WithRealFFmpeg(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	fullPath := filepath.Join(t.TempDir(), "full.ts")
	writeAlignedTestTS(t, ffmpeg, fullPath, 4)
	fullBytes, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	const tsPacketBytes = 188
	partialBytes := len(fullBytes) / 2 / tsPacketBytes * tsPacketBytes
	if partialBytes < tsPacketBytes || partialBytes >= len(fullBytes) {
		t.Fatalf("TS prefix length %d is invalid for %d bytes", partialBytes, len(fullBytes))
	}
	partialPath := filepath.Join(t.TempDir(), "growing.ts")
	if err := os.WriteFile(partialPath, fullBytes[:partialBytes], 0o600); err != nil {
		t.Fatal(err)
	}
	expectedFrames := framehashVideoFrameCount(t, ffmpeg, fullPath)
	prefixFrames := framehashVideoFrameCount(t, ffmpeg, partialPath)
	if prefixFrames <= 0 || prefixFrames >= expectedFrames {
		t.Fatalf("static TS prefix has %d frames; want between 1 and %d", prefixFrames, expectedFrames-1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-v", "error",
		"-skip_estimate_duration_from_pts", "1",
		"-follow", "1",
		"-rw_timeout", "5000000",
		"-i", "/dev/fd/3",
		"-map", "0:v:0", "-an", "-fps_mode", "passthrough",
		"-f", "framehash", "-hash", "md5", "pipe:1",
	)
	input, err := os.Open(partialPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = []*os.File{input}
	var output bytes.Buffer
	cmd.Stdout = &output
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = input.Close()
		t.Fatalf("starting real ffmpeg with /dev/fd/3: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	appended := false
	processFinished := false
	var waitErr error
	deadline := time.Now().Add(5 * time.Second)
	for !appended && time.Now().Before(deadline) {
		select {
		case waitErr = <-waitCh:
			processFinished = true
		default:
		}
		if processFinished {
			break
		}
		offset, offsetErr := growingInputOffset(cmd.Process.Pid, input, partialPath)
		if offsetErr != nil {
			_ = cmd.Process.Kill()
			_ = input.Close()
			t.Fatalf("reading growing input offset: %v", offsetErr)
		}
		if offset >= int64(partialBytes) {
			writer, openErr := os.OpenFile(partialPath, os.O_WRONLY|os.O_APPEND, 0)
			if openErr != nil {
				_ = cmd.Process.Kill()
				_ = input.Close()
				t.Fatal(openErr)
			}
			_, writeErr := writer.Write(fullBytes[partialBytes:])
			closeErr := writer.Close()
			if writeErr != nil || closeErr != nil {
				_ = cmd.Process.Kill()
				_ = input.Close()
				t.Fatalf("appending TS bytes: write=%v close=%v", writeErr, closeErr)
			}
			appended = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = input.Close()
	if !appended && !processFinished {
		_ = cmd.Process.Kill()
		waitErr = <-waitCh
	}
	if appended {
		waitErr = <-waitCh
	}
	if !appended {
		t.Fatalf("ffmpeg did not read the %d-byte initial prefix before it exited: %v; stderr=%s", partialBytes, waitErr, stderr.String())
	}
	frames := 0
	for _, line := range strings.Split(output.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			frames++
		}
	}
	if frames != expectedFrames {
		t.Fatalf("real ffmpeg decoded %d frames from the growing /dev/fd/3 input, want %d after append; exit=%v stderr=%s", frames, expectedFrames, waitErr, stderr.String())
	}
}

func growingInputOffset(pid int, input *os.File, path string) (int64, error) {
	if runtime.GOOS != "linux" {
		return input.Seek(0, io.SeekCurrent)
	}
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	descriptors, err := os.ReadDir(fdDir)
	if err != nil {
		return 0, err
	}
	var maxOffset int64
	for _, descriptor := range descriptors {
		fdPath := filepath.Join(fdDir, descriptor.Name())
		target, err := os.Readlink(fdPath)
		if err != nil || filepath.Clean(target) != filepath.Clean(path) {
			continue
		}
		info, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "fdinfo", descriptor.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(info), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(key) != "pos" {
				continue
			}
			offset, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err == nil && offset > maxOffset {
				maxOffset = offset
			}
		}
	}
	return maxOffset, nil
}

func framehashVideoFrameCount(t *testing.T, ffmpeg, path string) int {
	t.Helper()
	cmd := exec.Command(ffmpeg,
		"-v", "error", "-i", path, "-map", "0:v:0", "-an", "-fps_mode", "passthrough",
		"-f", "framehash", "-hash", "md5", "pipe:1",
	)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("counting framehash output for %s: %v", path, err)
	}
	count := 0
	for _, line := range strings.Split(string(output), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			count++
		}
	}
	return count
}

func TestBuildSegmentTransformArgsMapsOnlyPresentSubtitles(t *testing.T) {
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2}
	input := SegmentTransformInput{Path: "/dev/fd/3", OriginPTS: 90000, AudioStreamCount: 1}
	workDir := t.TempDir()
	argsWithoutSubtitles, err := buildSegmentTransformArgs(
		LiveConfig{FFmpeg: "ffmpeg", Captions: true, Profiles: []LiveProfile{profile}},
		profile, input, 0, workDir,
	)
	if err != nil {
		t.Fatal(err)
	}
	if indexOf(argsWithoutSubtitles, "-hls_subtitle_path") >= 0 || indexOf(argsWithoutSubtitles, "-c:s") >= 0 {
		t.Fatalf("args without source subtitles include a subtitle output: %v", argsWithoutSubtitles)
	}

	input.HasSubtitles = true
	argsWithSubtitles, err := buildSegmentTransformArgs(
		LiveConfig{FFmpeg: "ffmpeg", Captions: true, Profiles: []LiveProfile{profile}},
		profile, input, 0, workDir,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertArgPair(t, argsWithSubtitles, "-c:s", "webvtt")
	assertArgPair(t, argsWithSubtitles, "-hls_subtitle_path", filepath.Join(workDir, "subtitles_%v.m3u8"))
	videoFilterIndex := indexOf(argsWithSubtitles, "-filter:v:0")
	if videoFilterIndex < 0 || videoFilterIndex+1 >= len(argsWithSubtitles) || !strings.Contains(argsWithSubtitles[videoFilterIndex+1], "trim=start=1.000000000") {
		t.Fatalf("subtitle transform video filter does not trim to the requested timestamp: %v", argsWithSubtitles)
	}
	mapIndex := indexOf(argsWithSubtitles, "-var_stream_map")
	if mapIndex < 0 || mapIndex+1 >= len(argsWithSubtitles) || !strings.Contains(argsWithSubtitles[mapIndex+1], "s:0,sgroup:subs") {
		t.Fatalf("subtitle variant is missing from var_stream_map: %v", argsWithSubtitles)
	}
}

func TestNewSegmentManagerValidatesCacheWindowAndCleansOwnedScratch(t *testing.T) {
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2}
	base := SegmentTransformOptions{
		Config:           LiveConfig{Profiles: []LiveProfile{profile}},
		ScratchRoot:      t.TempDir(),
		CacheBytes:       60,
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
	if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:2.0,\nsegments/profile.0_seg00007.ts\n#EXTINF:2.0,\nsegments/profile.0_seg00008.ts\n#EXT-X-ENDLIST\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	segments, endList, err := readSegmentPlaylist(playlist, "profile", "0")
	if err != nil || !endList || len(segments) != 2 || segments[0] != 7 || segments[1] != 8 {
		t.Fatalf("read playlist = (%v, %v, %v), want ordered segments 7 and 8 with ENDLIST", segments, endList, err)
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

func TestReadSubtitleSegmentPlaylistCachesVTTByCanonicalNumber(t *testing.T) {
	dir := t.TempDir()
	playlist := filepath.Join(dir, "subtitles_0.m3u8")
	contents := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nsegments/seg0.vtt\n#EXTINF:2.0,\nsegments/seg1.vtt\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(playlist, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	segments, endList, err := readSubtitleSegmentPlaylist(playlist)
	if err != nil || !endList || len(segments) != 2 || segments[0] != 0 || segments[1] != 1 {
		t.Fatalf("read subtitle playlist = (%v, %v, %v), want ordered segments 0 and 1 with ENDLIST", segments, endList, err)
	}
	for name, want := range map[string]int64{"seg0.vtt": 0, "seg12.vtt": 12} {
		got, ok := parseSubtitleSegmentNumber(name)
		if !ok || got != want {
			t.Fatalf("parseSubtitleSegmentNumber(%q) = (%d, %v), want (%d, true)", name, got, ok, want)
		}
	}
	for _, name := range []string{"seg00.vtt", "seg-1.vtt", "seg1.ts", "other0.vtt"} {
		if _, ok := parseSubtitleSegmentNumber(name); ok {
			t.Errorf("parseSubtitleSegmentNumber(%q) succeeded, want rejection", name)
		}
	}
}

func TestSegmentTransformCachesSegmentsInPlaylistOrder(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	segmentDir := filepath.Join(workDir, "segments")
	if err := os.MkdirAll(segmentDir, 0o750); err != nil {
		t.Fatal(err)
	}
	playlist := filepath.Join(workDir, "test.0.m3u8")
	contents := "#EXTM3U\n#EXTINF:2.0,\ntest.0_seg00001.ts\n#EXTINF:2.0,\ntest.0_seg00002.ts\n"
	if err := os.WriteFile(playlist, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, segment := range []struct {
		number  int
		content string
		suffix  string
	}{
		{number: 1, content: "one", suffix: ".ts"},
		{number: 2, content: "two", suffix: ".ts"},
		{number: 3, content: "partial", suffix: ".ts.tmp"},
	} {
		path := filepath.Join(segmentDir, fmt.Sprintf("test.0_seg%05d%s", segment.number, segment.suffix))
		if err := os.WriteFile(path, []byte(segment.content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cache, err := newSegmentCache(filepath.Join(root, "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	manager := &SegmentManager{cache: cache, maxSegmentBytes: 1024, prefetchSegments: 5}
	task := &segmentTransformTask{
		group:         segmentTransformGroup{recordingID: "recording", generation: "generation", profile: "test"},
		profile:       LiveProfile{Name: "test", SegmentSeconds: 2},
		requestStart:  1,
		startNumber:   1,
		currentNumber: 1,
		maxRequested:  2,
		workDir:       workDir,
	}
	if err := manager.scanTransform(task, false); err != nil {
		t.Fatal(err)
	}
	for _, number := range []int64{1, 2} {
		key := SegmentKey{"recording", "generation", "test", "video", number}
		lease, ok, err := cache.acquire(key)
		if err != nil || !ok {
			t.Fatalf("segment %d after ordered scan = (ok %v, err %v), want cached", number, ok, err)
		}
		lease.release()
	}
}

func TestSegmentTransformCachesWebVTTSegments(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// 映像は確定した最後の番号 1 に届いている。字幕はそれより先には公開されない。
	videoPlaylist := "#EXTM3U\n#EXTINF:2.0,\nsegments/test.0_seg00000.ts\n#EXTINF:2.0,\nsegments/test.0_seg00001.ts\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(filepath.Join(workDir, "test.0.m3u8"), []byte(videoPlaylist), 0o600); err != nil {
		t.Fatal(err)
	}
	playlist := filepath.Join(workDir, "subtitles_0.m3u8")
	contents := "#EXTM3U\n#EXTINF:2.0,\nsegments/seg0.vtt\n#EXTINF:2.0,\nsegments/seg1.vtt\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(playlist, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	for number, content := range map[int]string{0: "WEBVTT\n\nfirst\n", 1: "WEBVTT\n\nsecond\n"} {
		path := filepath.Join(workDir, fmt.Sprintf("seg%d.vtt", number))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cache, err := newSegmentCache(filepath.Join(root, "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	manager := &SegmentManager{cache: cache, maxSegmentBytes: 1024, prefetchSegments: 5}
	task := &segmentTransformTask{
		group:         segmentTransformGroup{recordingID: "7", generation: "3", profile: "test"},
		profile:       LiveProfile{Name: "test", SegmentSeconds: 2},
		input:         SegmentTransformInput{HasSubtitles: true, VideoDurationPTS: 4 * mpegTSTimestampClock, VideoLastFramePTS: 4*mpegTSTimestampClock - 1},
		requestStart:  0,
		startNumber:   0,
		currentNumber: 0,
		maxRequested:  1,
		workDir:       workDir,
	}
	if err := manager.scanTransform(task, true); err != nil {
		t.Fatal(err)
	}
	for number, want := range map[int64]string{0: "WEBVTT\n\nfirst\n", 1: "WEBVTT\n\nsecond\n"} {
		key := SegmentKey{"7", "3", "test", "subtitles", number}
		lease, ok, err := cache.acquire(key)
		if err != nil || !ok {
			t.Fatalf("subtitle segment %d after scan = (ok %v, err %v), want cached", number, ok, err)
		}
		got, readErr := io.ReadAll(lease.file)
		lease.release()
		if readErr != nil || string(got) != want {
			t.Fatalf("subtitle segment %d = (%q, %v), want %q", number, got, readErr, want)
		}
	}
}

func TestSegmentTransformRealFFmpegKeepsRequestedPTSGrid(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	for _, tc := range []struct {
		name  string
		write func(*testing.T, string, string, int)
		stale bool
	}{
		{"先頭 PES だけ古い", writeOldAudioPESTestTS, true},
		{"全ストリームが揃っている", writeAlignedTestTS, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputPath := filepath.Join(t.TempDir(), "input.ts")
			tc.write(t, ffmpeg, inputPath, 20)
			originPTS := firstPacketPTS(t, ffprobe, inputPath)
			formatStart := formatStartPTS(t, ffprobe, inputPath)
			// 先頭に古い PES があり、format.start_time が映像の原点より数千秒前になる実録画の形を再現できていること。
			// 再現できていなければ、start_time の扱いが間違っていても格子が合ってしまい、何も固定しない。
			gap := float64(originPTS-formatStart) / float64(mpegTSTimestampClock)
			if tc.stale && gap < 5000 {
				t.Fatalf("format.start_time is only %.3f s before the first video PTS, want a stale first PES more than 5000 s earlier", gap)
			}
			if !tc.stale && (gap < -1 || gap > 1) {
				t.Fatalf("format.start_time is %.3f s from the first video PTS, want an aligned input", gap)
			}
			for number := int64(0); number <= 8; number++ {
				manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
				input := testSegmentInput(inputPath, originPTS)
				key := SegmentKey{"recording", "generation-1", "test", "video", number}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				file, err := manager.Get(ctx, key, input)
				cancel()
				if err != nil {
					closeSegmentManager(t, manager)
					t.Fatalf("segment %d: %v", number, err)
				}
				gotPTS := firstPacketPTS(t, ffprobe, file.Name())
				_ = file.Close()
				wantPTS := float64(originPTS)/float64(mpegTSTimestampClock) + float64(number*2)
				if delta := float64(gotPTS)/float64(mpegTSTimestampClock) - wantPTS; delta < -0.04 || delta > 0.04 {
					t.Errorf("segment %d first video PTS = %.6f, want %.6f ± one 25 fps frame", number, float64(gotPTS)/float64(mpegTSTimestampClock), wantPTS)
				}
				previous := key
				previous.Number--
				if number > 0 {
					if lease, ok, err := manager.cache.acquire(previous); err != nil || ok {
						if lease != nil {
							_ = lease.file.Close()
						}
						t.Errorf("one-before segment %#v cached = (%v, %v), want absent", previous, ok, err)
					}
				}
				closeSegmentManager(t, manager)
			}
		})
	}
}

// TestSegmentTransformRestartKeepsSegmentStartFrame は、起動位置が違う 2 つの変換が、同じ番号の
// セグメントを同じフレームから始めることを確かめる。29.97 fps では 2 秒の格子にフレームが割り切れず、
// 起動位置の T と最初のフレームの差が起動ごとに変わる（startNumber 1 で 180 tick、16 で 2880 tick）。
// キーフレームの格子をその差に引きずられると、キャッシュでつないだ境界でフレームが重複・欠落する。
func TestSegmentTransformRestartKeepsSegmentStartFrame(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "ntsc.ts")
	write2997TestTS(t, ffmpeg, inputPath, 44)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	qp := 0
	profile := LiveProfile{
		Name: "test", VideoCodec: "libx264", AudioCodec: "aac",
		Preset: "ultrafast", SegmentSeconds: 2, QP: &qp,
	}
	firstFrames := func(request int64, numbers []int64) map[int64]string {
		manager, err := NewSegmentManager(SegmentTransformOptions{
			Config:          LiveConfig{FFmpeg: ffmpeg, Profiles: []LiveProfile{profile}},
			ScratchRoot:     t.TempDir(),
			CacheBytes:      1 << 30,
			MaxSegmentBytes: 16 << 20,
			MaxConcurrent:   1,
			IdleTimeout:     2 * time.Minute,
			PollInterval:    2 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer closeSegmentManager(t, manager)
		input := testSegmentInput(inputPath, originPTS)
		hashes := map[int64]string{}
		for number := request; number <= numbers[len(numbers)-1]; number++ {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			file, err := manager.Get(ctx, SegmentKey{"recording", "generation-1", "test", "video", number}, input)
			cancel()
			if err != nil {
				t.Fatalf("request %d, segment %d: %v", request, number, err)
			}
			// 出力の PTS は元の PTS のままで、格子 V + n·D 以上の最初のフレーム（1 フレーム未満の後ろ）にある。
			if delta := firstPacketPTS(t, ffprobe, file.Name()) - (originPTS + number*2*mpegTSTimestampClock); delta < 0 || delta >= 3003 {
				t.Errorf("request %d, segment %d: first video PTS is %d ticks from the grid, want [0, 3003)", request, number, delta)
			}
			out, err := exec.Command(ffmpeg, "-v", "error", "-i", file.Name(), "-map", "0:v:0",
				"-frames:v", "1", "-f", "framehash", "-hash", "md5", "-").Output()
			_ = file.Close()
			if err != nil {
				t.Fatalf("hashing segment %d: %v", number, err)
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			fields := strings.Split(lines[len(lines)-1], ",")
			hashes[number] = strings.TrimSpace(fields[len(fields)-1])
		}
		return hashes
	}
	numbers := []int64{17, 18, 19, 20}
	early := firstFrames(2, numbers)
	late := firstFrames(17, numbers)
	for _, number := range numbers {
		if early[number] != late[number] {
			t.Errorf("segment %d starts with different frames from runs started at 1 and 16 (%s vs %s)", number, early[number], late[number])
		}
	}
}

// write2997TestTS は、映像が 29.97 fps（30000/1001）で、映像の最初の PTS が 5400 秒付近にある TS を書く。
// フレーム番号ごとに画素が変わるよう、testsrc2 を使う。
func write2997TestTS(t *testing.T, ffmpeg, path string, seconds int) {
	t.Helper()
	video := fmt.Sprintf("testsrc2=size=160x90:rate=30000/1001:duration=%d", seconds)
	audio := fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", seconds)
	cmd := exec.Command(ffmpeg, "-v", "error", "-y",
		"-itsoffset", "5400", "-f", "lavfi", "-i", video,
		"-itsoffset", "5400", "-f", "lavfi", "-i", audio,
		"-itsoffset", "5400", "-f", "lavfi", "-i", audio,
		"-map", "0:v:0", "-map", "1:a:0", "-map", "2:a:0",
		"-c:v", "mpeg2video", "-q:v", "3", "-g", "15", "-bf", "0", "-sc_threshold", "0",
		"-c:a", "mp2", "-b:a", "128k",
		"-mpegts_copyts", "1", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating 29.97 fps test TS: %v\n%s", err, out)
	}
}

func TestSegmentTransformSIGSTOPAtPrefetchThenSIGCONTOnDemand(t *testing.T) {
	ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeAlignedTestTS(t, ffmpeg, inputPath, 30)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManagerPrefetch(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20, 3)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(inputPath, originPTS)
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
	current, requested := task.currentNumber, task.maxRequested
	manager.mu.Unlock()
	// 止まる位置は「要求済みの最大 + 先読み数」の 1 つ先を書き始めた時点に等しい。
	bound := requested + 3
	if current != bound+1 {
		t.Fatalf("paused at segment %d for request %d with prefetch 3, want exactly %d", current, requested, bound+1)
	}
	// 止まった後の scan で、先読みの範囲がキャッシュに入る。先頭から連続して入っている最後の番号は
	// bound か bound-1 である。ffmpeg は次のセグメントを書き始めた時点では直前のセグメントを
	// playlist に載せていないことがあり、そのとき止めると最後の 1 本は載らないまま止まる。
	// bound の次は、どの場合でもキャッシュに無い。
	cached := func(number int64) bool {
		key := firstKey
		key.Number = number
		lease, ok, err := manager.cache.acquire(key)
		if err != nil {
			t.Fatal(err)
		}
		if lease != nil {
			_ = lease.file.Close()
		}
		return ok
	}
	var last int64
	cacheDeadline := time.Now().Add(3 * time.Second)
	for {
		last = firstKey.Number - 1
		for cached(last + 1) {
			last++
		}
		if last >= bound-1 || time.Now().After(cacheDeadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("paused at %d with maxRequested %d: cached %d..%d", current, requested, firstKey.Number, last)
	if last != bound && last != bound-1 {
		t.Fatalf("contiguous cached segments end at %d, want %d or %d", last, bound-1, bound)
	}
	if cached(bound + 1) {
		t.Fatalf("segment %d beyond the prefetch window is cached", bound+1)
	}

	// 先読みの最後の 1 本（bound）を要求しても、新しい変換は起動せず、止まっていた同じ変換から届く。
	boundKey := firstKey
	boundKey.Number = bound
	boundContext, boundCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer boundCancel()
	boundFile, err := manager.Get(boundContext, boundKey, input)
	if err != nil {
		t.Fatal(err)
	}
	_ = boundFile.Close()
	if taskForRecording(t, manager, "recording") != task {
		t.Fatal("request for the last prefetched segment replaced the paused transform")
	}
	manager.mu.Lock()
	taskCount := len(manager.tasks[segmentTransformGroup{"recording", "generation-1", "test", 0}])
	manager.mu.Unlock()
	if taskCount != 1 {
		t.Fatalf("transforms after requesting segment %d = %d, want 1", bound, taskCount)
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

func TestSegmentTransformResumeAfterSIGSTOPUpdatesPlaylistPromptly(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 30)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManagerPrefetch(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20, 3)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(inputPath, originPTS)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key := SegmentKey{"recording", "generation-1", "test", "video", 1}
	file, err := manager.Get(ctx, key, input)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	task := waitForTaskPause(t, manager, transformPausedAhead, 15*time.Second)
	playlist := filepath.Join(task.workDir, "test.0.m3u8")
	before, _, err := readSegmentPlaylist(playlist, "test", "0")
	if err != nil {
		t.Fatal(err)
	}
	// 止めたまま 1 秒置き、temp_file の書きかけを握った状態で時間が経つ条件を作る。
	time.Sleep(time.Second)

	manager.mu.Lock()
	next := task.currentNumber
	manager.mu.Unlock()
	nextKey := key
	nextKey.Number = next
	resumedAt := time.Now()
	done := startSegmentGet(manager, ctx, nextKey, input)
	var playlistDelay time.Duration
	for {
		entries, _, err := readSegmentPlaylist(playlist, "test", "0")
		if err == nil && len(entries) > len(before) {
			playlistDelay = time.Since(resumedAt)
			break
		}
		if time.Since(resumedAt) > 10*time.Second {
			t.Fatalf("playlist had %d entries and did not grow within 10 s after SIGCONT", len(before))
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	segmentDelay := time.Since(resumedAt)
	t.Logf("libx264 software encode: playlist grew %v and segment %d was returned %v after the request that sent SIGCONT (playlist entries before: %d)",
		playlistDelay, next, segmentDelay, len(before))
	if playlistDelay > 5*time.Second {
		t.Fatalf("playlist grew %v after SIGCONT, want under 5 s", playlistDelay)
	}
}

func TestSegmentTransformAtCapacityDoesNotKillTransformsWithWaiters(t *testing.T) {
	ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeAlignedTestTS(t, ffmpeg, inputPath, 30)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 2, 128<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(inputPath, originPTS)

	var tasks []*segmentTransformTask
	var cancels []context.CancelFunc
	var dones []<-chan error
	for _, recordingID := range []string{"a", "b"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		dones = append(dones, startSegmentGet(manager, ctx, SegmentKey{recordingID, "generation-1", "test", "video", 5}, input))
		waitForTransformCount(t, manager, len(tasks)+1)
		tasks = append(tasks, taskForRecording(t, manager, recordingID))
	}
	cCtx, cCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cCancel()
	file, err := manager.Get(cCtx, SegmentKey{"c", "generation-1", "test", "video", 5}, input)
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request at capacity with every transform waited on = %v, want its own context deadline", err)
	}
	manager.mu.Lock()
	for _, task := range tasks {
		if task.stopping || task.finished {
			manager.mu.Unlock()
			t.Fatal("a transform with a waiting request was killed to make room")
		}
	}
	manager.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, done := range dones {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting request error = %v, want context canceled", err)
		}
	}
}

func TestSegmentTransformAtCapacityKillsOldestIdleTransform(t *testing.T) {
	ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeAlignedTestTS(t, ffmpeg, inputPath, 30)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 2, 128<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(inputPath, originPTS)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	keyFor := func(recordingID string) SegmentKey {
		return SegmentKey{recordingID, "generation-1", "test", "video", 1}
	}
	aDone := startSegmentGet(manager, ctx, keyFor("a"), input)
	waitForTransformCount(t, manager, 1)
	time.Sleep(100 * time.Millisecond)
	bDone := startSegmentGet(manager, ctx, keyFor("b"), input)
	for _, done := range []<-chan error{aDone, bDone} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	aTask := taskForRecording(t, manager, "a")
	bTask := taskForRecording(t, manager, "b")
	// a は b より先に要求されたが、キャッシュ済みのセグメントを読み直して a の最終要求時刻を新しくする。
	file, err := manager.Get(ctx, keyFor("a"), input)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	cFile, err := manager.Get(ctx, keyFor("c"), input)
	if err != nil {
		t.Fatal(err)
	}
	_ = cFile.Close()
	select {
	case <-bTask.done:
	default:
		t.Fatal("transform with the oldest last demand was not killed")
	}
	select {
	case <-aTask.done:
		t.Fatal("transform refreshed by a cache hit was killed instead of the older one")
	default:
	}
}

func TestSegmentTransformWaitWindowReusesAndRestartsWithRealFFmpeg(t *testing.T) {
	ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeAlignedTestTS(t, ffmpeg, inputPath, 90)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 2, 128<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(inputPath, originPTS)
	group := segmentTransformGroup{recordingID: "recording", generation: "generation-1", profile: "test"}

	firstKey := SegmentKey{"recording", "generation-1", "test", "video", 3}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 20*time.Second)
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

func TestSegmentTransformGracefulStopDoesNotCacheInterruptedTail(t *testing.T) {
	// ffmpeg は SIGINT/SIGTERM で終わるとき、書きかけの末尾を rename して playlist に載せる。
	// 次のセグメントが始まっていないその末尾を、キャッシュに入れてはならない。
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
			ffprobe := lookPathFFprobe(t)
			inputPath := filepath.Join(t.TempDir(), "long.ts")
			writeAlignedTestTS(t, ffmpeg, inputPath, 20)
			originPTS := firstPacketPTS(t, ffprobe, inputPath)
			manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
			defer closeSegmentManager(t, manager)
			input := testSegmentInput(inputPath, originPTS)
			key := SegmentKey{"recording", "generation-1", "test", "video", 3}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			file, err := manager.Get(ctx, key, input)
			if err != nil {
				t.Fatal(err)
			}
			_ = file.Close()

			task := taskForRecording(t, manager, "recording")
			// 止めて位置を確定させてから終わらせる。進行中のままだと、末尾が 1 つ先へ動く。
			if err := task.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
				t.Fatalf("stopping transform: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
			manager.mu.Lock()
			tail := task.currentNumber
			manager.mu.Unlock()
			if tail <= key.Number {
				t.Fatalf("tail segment %d is not past the requested segment %d", tail, key.Number)
			}
			if err := task.cmd.Process.Signal(signal); err != nil {
				t.Fatalf("sending %v: %v", signal, err)
			}
			if err := task.cmd.Process.Signal(syscall.SIGCONT); err != nil {
				t.Fatalf("continuing transform: %v", err)
			}
			select {
			case <-task.done:
			case <-time.After(5 * time.Second):
				t.Fatal("transform did not finish after the signal")
			}
			for number := tail; number <= tail+2; number++ {
				cached := key
				cached.Number = number
				if lease, ok, err := manager.cache.acquire(cached); err != nil || ok {
					if lease != nil {
						_ = lease.file.Close()
					}
					t.Fatalf("interrupted tail %#v cached = (%v, %v), want absent", cached, ok, err)
				}
			}
		})
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
	return newRealSegmentManagerPrefetch(t, ffmpeg, scratch, concurrency, cacheBytes, maxSegmentBytes, 0)
}

func newRealSegmentManagerPrefetch(
	t *testing.T,
	ffmpeg string,
	scratch string,
	concurrency int,
	cacheBytes int64,
	maxSegmentBytes int64,
	prefetch int,
) *SegmentManager {
	t.Helper()
	profile := LiveProfile{
		Name: "test", VideoCodec: "libx264", AudioCodec: "aac",
		Preset: "ultrafast", SegmentSeconds: 2, PlaylistSize: 0,
	}
	manager, err := NewSegmentManager(SegmentTransformOptions{
		Config:           LiveConfig{FFmpeg: ffmpeg, Profiles: []LiveProfile{profile}},
		ScratchRoot:      scratch,
		CacheBytes:       cacheBytes,
		MaxSegmentBytes:  maxSegmentBytes,
		MaxConcurrent:    concurrency,
		PrefetchSegments: prefetch,
		IdleTimeout:      2 * time.Minute,
		PollInterval:     2 * time.Millisecond,
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

func testSegmentInput(path string, originPTS int64) SegmentTransformInput {
	return SegmentTransformInput{
		OpenFile:         func(context.Context) (*os.File, error) { return os.Open(path) },
		OriginPTS:        originPTS,
		AudioStreamCount: 2,
	}
}

func writeOldAudioPESTestTS(t *testing.T, ffmpeg, path string, seconds int) {
	t.Helper()
	// 映像と 1 本目の音声は 5400 秒付近から始まり、2 本目の音声は先頭に 0 秒付近の古い PES を
	// 1 秒分だけ持つ。format.start_time がその古い PES の時刻（0 付近）になり、映像の原点より
	// 5000 秒以上前になる。実録画の形（約 5400 秒古い）に合わせた 0 秒で、-noaccurate_seek 付きの絶対秒
	// シークでは、この時刻を 10 / 100 / 5400 秒にしても 9 セグメントが出ることを手元で確認した。
	video := fmt.Sprintf("testsrc2=size=160x90:rate=25:duration=%d", seconds)
	audio := fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", seconds)
	cmd := exec.Command(ffmpeg, "-v", "error", "-y",
		"-itsoffset", "5400", "-f", "lavfi", "-i", video,
		"-itsoffset", "5400", "-f", "lavfi", "-i", audio,
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000:duration=1",
		"-map", "0:v:0", "-map", "1:a:0", "-map", "2:a:0",
		"-c:v", "mpeg2video", "-q:v", "3", "-g", "50", "-bf", "0", "-sc_threshold", "0",
		"-c:a", "mp2", "-b:a", "128k",
		"-mpegts_copyts", "1", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts",
		path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generating old-first-PES test TS: %v\n%s", err, out)
	}
}

// paceFFmpeg は、変換の映像と音声の filter の前に realtime / arealtime を挟んで ffmpeg を実時間で進める
// ラッパーを作り、そのパスを返す。ffmpeg 9.0.2 では、-ss と併用した -re が実時間で進まなかった
// （20 秒の入力が約 1 秒で終わった）ので、先読み・上限・kill のようにペース配分が要るテストはこれを使う。
func paceFFmpeg(t *testing.T, ffmpeg string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg-paced")
	script := `#!/bin/bash
args=()
for a in "$@"; do
  a="${a//atrim=start=/@@A@@}"
  a="${a//trim=start=/realtime,trim=start=}"
  a="${a//@@A@@/arealtime,atrim=start=}"
  args+=("$a")
done
exec "` + ffmpeg + `" "${args[@]}"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeAlignedTestTS は、全ストリームが 5400 秒付近から揃って始まる TS を書く。
// 全ストリームが揃った形の入力。ペース配分が要るテストが使う（paceFFmpeg と組み合わせる）。
func writeAlignedTestTS(t *testing.T, ffmpeg, path string, seconds int) {
	t.Helper()
	video := fmt.Sprintf("testsrc2=size=160x90:rate=25:duration=%d", seconds)
	audio := fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", seconds)
	cmd := exec.Command(ffmpeg, "-v", "error", "-y",
		"-itsoffset", "5400", "-f", "lavfi", "-i", video,
		"-itsoffset", "5400", "-f", "lavfi", "-i", audio,
		"-itsoffset", "5400", "-f", "lavfi", "-i", audio,
		"-map", "0:v:0", "-map", "1:a:0", "-map", "2:a:0",
		"-c:v", "mpeg2video", "-q:v", "3", "-g", "50", "-bf", "0", "-sc_threshold", "0",
		"-c:a", "mp2", "-b:a", "128k",
		"-mpegts_copyts", "1", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating aligned test TS: %v\n%s", err, out)
	}
}

func firstPacketPTS(t *testing.T, ffprobe, path string) int64 {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_packets", "-show_entries", "packet=pts", "-read_intervals", "%+#1",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("probing first video packet from %s: %v", path, err)
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

func TestSegmentTransformDoesNotWaitForPreRollSegmentBeforeRequestStart(t *testing.T) {
	group := segmentTransformGroup{recordingID: "recording", generation: "generation", profile: "test"}
	// n=5 を要求して起動した変換は 4 から書き始めるが、4 は pre-roll なのでキャッシュに入れない。
	task := &segmentTransformTask{group: group, requestStart: 5, startNumber: 4, currentNumber: 4}
	manager := &SegmentManager{
		tasks: map[segmentTransformGroup]map[*segmentTransformTask]struct{}{group: {task: {}}},
	}
	if got := manager.findTaskLocked(group, 4, 2); got != nil {
		t.Fatalf("request for pre-roll segment 4 selected task %p, want a new transform", got)
	}
	if got := manager.findTaskLocked(group, 5, 2); got != task {
		t.Fatalf("request for segment 5 selected %p, want the transform started for it", got)
	}
}

func TestSegmentTransformFindsPrefetchPausedTransformForItsLastCompletedSegment(t *testing.T) {
	group := segmentTransformGroup{recordingID: "recording", generation: "generation", profile: "test"}
	// current=7 は 7 を書き始めた状態で、6 は完成済み（playlist への掲載が遅れているだけ）。
	task := &segmentTransformTask{group: group, requestStart: 1, startNumber: 0, currentNumber: 7}
	manager := &SegmentManager{
		tasks: map[segmentTransformGroup]map[*segmentTransformTask]struct{}{group: {task: {}}},
	}
	if got := manager.findTaskLocked(group, 6, 2); got != task {
		t.Fatalf("request for the last completed segment selected %p, want the paused transform", got)
	}
	if got := manager.findTaskLocked(group, 5, 2); got != nil {
		t.Fatalf("request for segment 5 selected %p, want a new transform", got)
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
	ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeAlignedTestTS(t, ffmpeg, inputPath, 20)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 31<<20, 1<<20)
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

	input := testSegmentInput(inputPath, originPTS)
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
	ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeAlignedTestTS(t, ffmpeg, inputPath, 10)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
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
	file, err := manager.Get(ctx, key, testSegmentInput(inputPath, originPTS))
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
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
	defer closeSegmentManager(t, manager)

	key := SegmentKey{"recording", "generation-1", "test", "video", 4}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := testSegmentInput(inputPath, originPTS)
	input.VideoDurationPTS, input.VideoLastFramePTS = videoSpanPTS(t, ffprobe, inputPath)
	file, err := manager.Get(ctx, key, input)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	gotPTS := firstPacketPTS(t, ffprobe, file.Name())
	wantPTS := originPTS + key.Number*2*mpegTSTimestampClock
	delta := gotPTS - wantPTS
	if delta < -3600 || delta > 3600 {
		t.Fatalf("final short segment PTS = %d ticks, want %d ± one 25 fps frame", gotPTS, wantPTS)
	}
}

func TestSegmentTransformStopsAtAnAlreadyCachedNextSegment(t *testing.T) {
	ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeAlignedTestTS(t, ffmpeg, inputPath, 20)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	key := SegmentKey{"recording", "generation-1", "test", "video", 3}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	file, err := manager.Get(ctx, key, testSegmentInput(inputPath, originPTS))
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
	nextDone := startSegmentGet(manager, nextContext, nextKey, testSegmentInput(inputPath, originPTS))
	waitForSegmentWaiter(t, manager, task, nextKey, 2*time.Second)
	manager.mu.Lock()
	if task.pauseReason == transformRunning {
		if err := task.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			manager.mu.Unlock()
			t.Fatalf("pausing transform after registering future waiter: %v", err)
		}
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

func TestSegmentTransformRequestForSegmentBeforeRunningStartStartsNewTransformWithRealFFmpeg(t *testing.T) {
	ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeAlignedTestTS(t, ffmpeg, inputPath, 30)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 2, 128<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(inputPath, originPTS)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	firstKey := SegmentKey{"recording", "generation-1", "test", "video", 5}
	firstDone := startSegmentGet(manager, ctx, firstKey, input)
	waitForTransformCount(t, manager, 1)
	// 5 の変換は 4 から書き始めるが 4 はキャッシュに入れない。直後の 4 の要求を待たせ続けてはならない。
	previousKey := firstKey
	previousKey.Number = 4
	previousCtx, previousCancel := context.WithTimeout(ctx, 20*time.Second)
	defer previousCancel()
	file, err := manager.Get(previousCtx, previousKey, input)
	if err != nil {
		t.Fatalf("segment 4 requested right after segment 5: %v", err)
	}
	_ = file.Close()
	cancel()
	<-firstDone
}

func TestSegmentTransformEndlistMakesOnlyTheRecordingsLastSegmentFinal(t *testing.T) {
	// 10 秒の録画なら D=2 秒で最後の番号は 4（ENDLIST の有無ではなく長さで決まる）。
	for _, tc := range []struct {
		name         string
		durationPTS  int64
		wantCachedAt bool
	}{
		{"duration matches this segment", 6 * mpegTSTimestampClock, true},
		{"recording continues past this segment", 12 * mpegTSTimestampClock, false},
		{"duration unknown", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newBareSegmentManager(t, 1<<20)
			workDir := filepath.Join(t.TempDir(), "work")
			if err := os.MkdirAll(filepath.Join(workDir, "segments"), 0o750); err != nil {
				t.Fatal(err)
			}
			// 6 秒なら番号 2 が最後。ffmpeg の入力が途中で切れても ENDLIST と終了コード 0 になる。
			playlist := "#EXTM3U\n#EXTINF:2.0,\nsegments/test.0_seg00002.ts\n#EXT-X-ENDLIST\n"
			if err := os.WriteFile(filepath.Join(workDir, "test.0.m3u8"), []byte(playlist), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workDir, "segments", "test.0_seg00002.ts"), []byte("tail"), 0o600); err != nil {
				t.Fatal(err)
			}
			task := &segmentTransformTask{
				group:   segmentTransformGroup{"recording", "generation", "test", 0},
				profile: LiveProfile{Name: "test", SegmentSeconds: 2},
				input:   SegmentTransformInput{VideoDurationPTS: tc.durationPTS, VideoLastFramePTS: tc.durationPTS - 1},
				workDir: workDir, requestStart: 2, startNumber: 1, currentNumber: 1, maxRequested: 2,
			}
			if err := manager.scanTransform(task, true); err != nil {
				t.Fatal(err)
			}
			lease, ok, err := manager.cache.acquire(SegmentKey{"recording", "generation", "test", "video", 2})
			if lease != nil {
				_ = lease.file.Close()
			}
			if err != nil || ok != tc.wantCachedAt {
				t.Fatalf("segment 2 cached = (%v, %v), want %v", ok, err, tc.wantCachedAt)
			}
		})
	}
}

func TestFinalSegmentNumber(t *testing.T) {
	// 単位は 90 kHz tick。10.010 秒は 900900、9.976 秒は 897840（29.97 fps の最後のフレームが
	// 2 秒の格子の 4 番に始まる例）。
	for _, tc := range []struct {
		name                   string
		durationPTS, lastFrame int64
		seconds                int
		want                   int64
	}{
		{"長さが未確定", 0, 0, 2, -1},
		{"最後のフレームが長さの外", 900900, 900900, 2, -1},
		{"最後のフレームが格子の境目に始まる", 10 * mpegTSTimestampClock, 10 * mpegTSTimestampClock, 2, -1},
		{"境目の直前に始まり、境目を越えて終わる（29.97 fps）", 900900, 897840, 2, 4},
		{"境目の 1 tick 手前に始まり、境目の後ろで終わる", 10*mpegTSTimestampClock + 1, 10*mpegTSTimestampClock - 1, 2, 4},
		{"境目ちょうどに始まるフレームがある", 10*mpegTSTimestampClock + 1, 10 * mpegTSTimestampClock, 2, 5},
		{"1 フレームだけ、原点に始まる", mpegTSTimestampClock, 0, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := finalSegmentNumber(tc.durationPTS, tc.lastFrame, tc.seconds); got != tc.want {
				t.Errorf("finalSegmentNumber(%d, %d, %d) = %d, want %d", tc.durationPTS, tc.lastFrame, tc.seconds, got, tc.want)
			}
		})
	}
}

// 最後のフレームが格子の境目をまたいで終わる録画（29.97 fps）でも、playlist の最後の 2 本が全 rendition で
// 取れる。10 秒の入力は最後のフレームが 9.976 秒に始まり 10.010 秒に終わる。格子の 5 番（10 秒〜）には
// 始まるフレームが無いので、5 番は載せず、4 番を最後にする。9 秒の入力は 4 番の中に収まる。
//
// 壊し方: finalSegmentNumber を ⌈長さ/D⌉−1 にする（10 秒の入力で最後が 5 番になり、変換が作れない）。
func TestSegmentTransformFinalFrameCrossingBoundaryServesLastSegments(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	for _, seconds := range []int{9, 10} {
		t.Run(fmt.Sprintf("%d seconds", seconds), func(t *testing.T) {
			inputPath := filepath.Join(t.TempDir(), "ntsc.ts")
			write2997TestTS(t, ffmpeg, inputPath, seconds)
			originPTS := firstPacketPTS(t, ffprobe, inputPath)
			input := testSegmentInput(inputPath, originPTS)
			input.VideoDurationPTS, input.VideoLastFramePTS = videoSpanPTS(t, ffprobe, inputPath)
			final := finalSegmentNumber(input.VideoDurationPTS, input.VideoLastFramePTS, 2)
			if final != 4 {
				t.Errorf("final segment = %d for %d seconds at 29.97 fps, want 4", final, seconds)
			}
			manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
			defer closeSegmentManager(t, manager)
			for _, rendition := range segmentRenditions {
				for number := final - 1; number <= final; number++ {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					file, err := manager.Get(ctx, SegmentKey{"recording", "generation-1", "test", rendition, number}, input)
					cancel()
					if err != nil {
						t.Errorf("%s segment %d: %v", rendition, number, err)
						continue
					}
					_ = file.Close()
				}
			}
		})
	}
}

func TestSegmentTransformGetBeyondRecordingEndFailsWithoutStartingFFmpeg(t *testing.T) {
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2}
	manager, err := NewSegmentManager(SegmentTransformOptions{
		Config:      LiveConfig{Profiles: []LiveProfile{profile}},
		ScratchRoot: t.TempDir(), CacheBytes: 64 << 20, MaxSegmentBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSegmentManager(t, manager)
	input := SegmentTransformInput{
		OpenFile:         func(context.Context) (*os.File, error) { return nil, errors.New("must not open") },
		VideoDurationPTS: 10 * mpegTSTimestampClock, VideoLastFramePTS: 10*mpegTSTimestampClock - 1,
		AudioStreamCount: 1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = manager.Get(ctx, SegmentKey{"recording", "generation", "test", "video", 5}, input)
	if !errors.Is(err, errSegmentBeyondEnd) {
		t.Fatalf("segment past the last number = %v, want errSegmentBeyondEnd without waiting", err)
	}
}

func TestSegmentTransformEOFNotAtKnownLastSegmentIsNotCachedWithRealFFmpeg(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "short.ts")
	writeOldAudioPESTestTS(t, ffmpeg, inputPath, 10)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	for name, durationPTS := range map[string]int64{
		"録画はもっと長いと分かっている": 20 * mpegTSTimestampClock,
		"長さが未確定":          0,
	} {
		t.Run(name, func(t *testing.T) {
			manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
			defer closeSegmentManager(t, manager)
			input := testSegmentInput(inputPath, originPTS)
			input.VideoDurationPTS = durationPTS
			input.VideoLastFramePTS = durationPTS - 1
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// 実ファイルは 10 秒なので、ffmpeg は番号 4 の短い末尾を ENDLIST 付き・終了コード 0 で書く。
			file, err := manager.Get(ctx, SegmentKey{"recording", "generation-1", "test", "video", 4}, input)
			if file != nil {
				_ = file.Close()
			}
			if !errors.Is(err, errSegmentNotCached) {
				t.Fatalf("EOF tail of a recording not known to end here = %v, want errSegmentNotCached", err)
			}
		})
	}
}

func TestSegmentTransformOversizedSegmentIsDeliveredAndHugeSegmentFailsWithoutWaiting(t *testing.T) {
	newTask := func(t *testing.T, manager *SegmentManager, content string) (*segmentTransformTask, SegmentKey, *segmentTransformWaiter) {
		workDir := filepath.Join(t.TempDir(), "work")
		if err := os.MkdirAll(filepath.Join(workDir, "segments"), 0o750); err != nil {
			t.Fatal(err)
		}
		playlist := "#EXTM3U\n#EXTINF:2.0,\nsegments/test.0_seg00001.ts\n"
		if err := os.WriteFile(filepath.Join(workDir, "test.0.m3u8"), []byte(playlist), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string]string{"test.0_seg00001.ts": content, "test.0_seg00002.ts.tmp": "next"} {
			if err := os.WriteFile(filepath.Join(workDir, "segments", name), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		group := segmentTransformGroup{"recording", "generation", "test", 0}
		key := SegmentKey{"recording", "generation", "test", "video", 1}
		waiter := &segmentTransformWaiter{ctx: context.Background(), ch: make(chan segmentTransformResult, 1)}
		task := &segmentTransformTask{
			group: group, profile: LiveProfile{Name: "test", SegmentSeconds: 2},
			workDir: workDir, requestStart: 1, startNumber: 0, currentNumber: 0, maxRequested: 1,
			waiters: map[SegmentKey][]*segmentTransformWaiter{key: {waiter}},
		}
		manager.tasks[group] = map[*segmentTransformTask]struct{}{task: {}}
		return task, key, waiter
	}

	t.Run("MaxSegmentBytes を超えても配信する", func(t *testing.T) {
		manager := newBareSegmentManager(t, 1<<20)
		manager.maxSegmentBytes = 1
		task, key, waiter := newTask(t, manager, "three")
		if err := manager.scanTransform(task, false); err != nil {
			t.Fatalf("segment above MaxSegmentBytes stopped the transform: %v", err)
		}
		select {
		case result := <-waiter.ch:
			if result.err != nil {
				t.Fatal(result.err)
			}
			_ = result.file.Close()
		case <-time.After(time.Second):
			t.Fatalf("segment %#v above MaxSegmentBytes was not delivered", key)
		}
	})

	t.Run("キャッシュ全体より大きければ待たせずエラー", func(t *testing.T) {
		manager := newBareSegmentManager(t, 4)
		task, _, waiter := newTask(t, manager, "much-too-large")
		done := make(chan error, 1)
		go func() { done <- manager.scanTransform(task, false) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("scan = %v, want the oversized segment dropped without stopping the transform", err)
			}
		case <-time.After(time.Second):
			t.Fatal("scan waited for cache space that can never exist")
		}
		select {
		case result := <-waiter.ch:
			if !errors.Is(result.err, errSegmentTooLarge) {
				t.Fatalf("waiter result = %v, want errSegmentTooLarge", result.err)
			}
		case <-time.After(time.Second):
			t.Fatal("waiter was not told that its segment can never be cached")
		}
		if task.pauseReason != transformRunning {
			t.Fatalf("transform pause reason = %d, want running", task.pauseReason)
		}
	})
}

func TestSegmentTransformDeletesPreRollSegmentOnceNextSegmentStarts(t *testing.T) {
	manager := newBareSegmentManager(t, 1<<20)
	workDir := filepath.Join(t.TempDir(), "work")
	segmentDir := filepath.Join(workDir, "segments")
	if err := os.MkdirAll(segmentDir, 0o750); err != nil {
		t.Fatal(err)
	}
	playlist := "#EXTM3U\n#EXTINF:2.0,\nsegments/test.0_seg00001.ts\n"
	if err := os.WriteFile(filepath.Join(workDir, "test.0.m3u8"), []byte(playlist), 0o600); err != nil {
		t.Fatal(err)
	}
	preRoll := filepath.Join(segmentDir, "test.0_seg00001.ts")
	for path, data := range map[string]string{preRoll: "pre-roll", filepath.Join(segmentDir, "test.0_seg00002.ts.tmp"): "next"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task := &segmentTransformTask{
		group:   segmentTransformGroup{"recording", "generation", "test", 0},
		profile: LiveProfile{Name: "test", SegmentSeconds: 2},
		workDir: workDir, requestStart: 2, startNumber: 1, currentNumber: 1, maxRequested: 2,
	}
	if err := manager.scanTransform(task, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(preRoll); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-roll segment stat = %v, want removed from scratch", err)
	}
	if lease, ok, _ := manager.cache.acquire(SegmentKey{"recording", "generation", "test", "video", 1}); ok {
		_ = lease.file.Close()
		t.Fatal("pre-roll segment was cached")
	}
}

func TestSegmentTransformCachedRequestResumesPrefetchPausedTransformWithRealFFmpeg(t *testing.T) {
	ffmpeg := paceFFmpeg(t, lookPathFFmpeg(t))
	ffprobe := lookPathFFprobe(t)
	inputPath := filepath.Join(t.TempDir(), "long.ts")
	writeAlignedTestTS(t, ffmpeg, inputPath, 30)
	originPTS := firstPacketPTS(t, ffprobe, inputPath)
	manager := newRealSegmentManagerPrefetch(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20, 3)
	defer closeSegmentManager(t, manager)
	input := testSegmentInput(inputPath, originPTS)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key := SegmentKey{"recording", "generation-1", "test", "video", 1}
	file, err := manager.Get(ctx, key, input)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	task := waitForTaskPause(t, manager, transformPausedAhead, 15*time.Second)
	manager.mu.Lock()
	pausedAt := task.currentNumber
	manager.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hit := key
		hit.Number = 3
		if lease, ok, _ := manager.cache.acquire(hit); ok {
			_ = lease.file.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	hit := key
	hit.Number = 3
	hitFile, err := manager.Get(ctx, hit, input)
	if err != nil {
		t.Fatal(err)
	}
	_ = hitFile.Close()
	manager.mu.Lock()
	requested, reason := task.maxRequested, task.pauseReason
	manager.mu.Unlock()
	if requested != 3 || reason != transformRunning {
		t.Fatalf("after a cache hit on 3: maxRequested=%d pause reason=%d, want 3 and running", requested, reason)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		manager.mu.Lock()
		current := task.currentNumber
		manager.mu.Unlock()
		if current > pausedAt {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("transform stayed at segment %d after a cache hit moved its prefetch window", pausedAt)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newBareSegmentManager(t *testing.T, cacheBytes int64) *SegmentManager {
	t.Helper()
	cache, err := newSegmentCache(filepath.Join(t.TempDir(), "cache"), cacheBytes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &SegmentManager{
		cache: cache, maxSegmentBytes: 1 << 20, prefetchSegments: 5, ctx: ctx,
		tasks:      make(map[segmentTransformGroup]map[*segmentTransformTask]struct{}),
		capacityCh: make(chan struct{}),
	}
}

func taskForRecording(t *testing.T, manager *SegmentManager, recordingID string) *segmentTransformTask {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for group, tasks := range manager.tasks {
		if group.recordingID != recordingID {
			continue
		}
		for task := range tasks {
			return task
		}
	}
	t.Fatalf("no transform for recording %q", recordingID)
	return nil
}

// videoSpanPTS は、製品が長さと最後のフレームの開始時刻に使う関数で測る。
// 長さは末尾の映像パケットの最大の pts + duration − 原点、最後のフレームの開始時刻は最大の pts − 原点。
func videoSpanPTS(t *testing.T, ffprobe, path string) (durationPTS, lastFramePTS int64) {
	t.Helper()
	duration, last, err := measureTestFileSpan(t, ffprobe, path)
	if err != nil {
		t.Fatalf("measuring video duration of %s: %v", path, err)
	}
	return duration, last
}

func TestSegmentTransformStopAtCachedPositionKeepsTransformsStillProducingAWaitedSegment(t *testing.T) {
	for _, tc := range []struct {
		name       string
		waitedFor  int64
		wantStopAt bool
	}{
		{"現在位置より前の番号を待っている", 4, false},
		{"現在位置より先の番号を待っている", 7, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newBareSegmentManager(t, 1<<20)
			group := segmentTransformGroup{"recording", "generation", "test", 0}
			for _, rendition := range segmentRenditions {
				path := filepath.Join(manager.cache.root, "tmp-"+rendition)
				if err := os.WriteFile(path, []byte("cached-"+rendition), 0o600); err != nil {
					t.Fatal(err)
				}
				lease, _, err := manager.cache.publish(path, SegmentKey{"recording", "generation", "test", rendition, 5})
				if err != nil {
					t.Fatal(err)
				}
				lease.release()
			}
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			waitedKey := SegmentKey{"recording", "generation", "test", "video", tc.waitedFor}
			waiter := &segmentTransformWaiter{ctx: context.Background(), ch: make(chan segmentTransformResult, 1)}
			task := &segmentTransformTask{
				group: group, requestStart: 4, startNumber: 3, currentNumber: 5, maxRequested: tc.waitedFor,
				cmd: cmd, stop: make(chan struct{}),
				waiters: map[SegmentKey][]*segmentTransformWaiter{waitedKey: {waiter}},
			}
			manager.tasks[group] = map[*segmentTransformTask]struct{}{task: {}}
			manager.maybeStopAtCachedSegment(task)
			manager.mu.Lock()
			stopping := task.stopping
			manager.mu.Unlock()
			if stopping != tc.wantStopAt {
				t.Fatalf("stopping = %v, want %v", stopping, tc.wantStopAt)
			}
			if tc.wantStopAt {
				select {
				case result := <-waiter.ch:
					if !errors.Is(result.err, errSegmentRetry) {
						t.Fatalf("waiter result = %v, want errSegmentRetry", result.err)
					}
				case <-time.After(time.Second):
					t.Fatal("waiter was not sent back to retry")
				}
			}
		})
	}
}
