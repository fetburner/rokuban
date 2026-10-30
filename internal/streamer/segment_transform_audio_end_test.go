package streamer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// 音声が映像より 0.12 秒短い合成データ（実録画では約 0.36 秒）。映像は 8.08 秒で、2 秒のセグメントの
// 最後（番号 4）には映像が 0.08 秒だけある。音声は 7.96 秒で終わるので、足さないと音声の最後は番号 3 になる。
// 全ての音声 rendition の最後（4）とその 1 つ前（3）が取得でき、最後の音声が映像の終端で終わる。
// 途中（3）から起動した変換でも、直接最後（4）から起動した変換でも同じである。
//
// 壊し方: buildSegmentTransformArgs の `audioFilter += ",apad,atrim=end=" + endSeconds` を消す。
// 壊し方: 同じ式の endSeconds を `input.VideoDurationPTS`（起動位置を引かない）から作る。
func TestSegmentTransformAudioRenditionsEndAtTheVideoNumberWithRealFFmpeg(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	path := filepath.Join(t.TempDir(), "short-audio.ts")
	writeOffsetTestTS(t, ffmpeg, path, 5400, "8.08", "7.96")
	originPTS := firstPacketPTS(t, ffprobe, path)
	duration, err := measureTestFile(t, ffprobe, path)
	if err != nil {
		t.Fatalf("measuring the fixture: %v", err)
	}
	if duration < 8*mpegTSTimestampClock+mpegTSTimestampClock/50 || duration > 8*mpegTSTimestampClock+mpegTSTimestampClock/5 {
		t.Fatalf("fixture video duration = %d ticks, want 8.0x seconds so that segment 4 holds a short remainder", duration)
	}
	input := testSegmentInput(path, originPTS)
	input.Kind = store.RecordingHLSInputOriginal
	input.VideoDurationPTS = duration
	audio := []string{"audio-standard", "audio-main", "audio-sub"}

	for name, numbers := range map[string][]int64{
		"途中の番号から起動して最後まで": {3, 4},
		"最後の番号から直接起動する":   {4, 3},
	} {
		t.Run(name, func(t *testing.T) {
			manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
			defer closeSegmentManager(t, manager)
			for _, number := range numbers {
				for _, rendition := range append([]string{"video"}, audio...) {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					file, err := manager.Get(ctx, SegmentKey{"recording", "generation-1", "test", rendition, number}, input)
					cancel()
					if err != nil {
						t.Fatalf("%s segment %d: %v", rendition, number, err)
					}
					if number == 4 && rendition != "video" {
						if seconds := mediaDurationSeconds(t, ffprobe, file.Name()); seconds < 0.03 || seconds > 0.2 {
							t.Errorf("%s last segment lasts %.3f s, want the 0.08 s the video has left", rendition, seconds)
						}
					}
					_ = file.Close()
				}
			}
		})
	}
}

func mediaDurationSeconds(t *testing.T, ffprobe, path string) float64 {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("probing duration of %s: %v", path, err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parsing duration %q: %v", out, err)
	}
	return seconds
}

// 長さが確定した入力の完成判定は、rendition ごとの ENDLIST と最後のファイルで行う。
// 映像の番号（ここでは 4）と比べると、1 つ手前（3）で終わる rendition の末尾が公開されない。
// 伸びる入力では ENDLIST を終端とみなさない。変換が異常終了したときも同じである。
//
// 壊し方: scanTransform の isFinal の `number == entries[rendition][len(entries[rendition])-1]` を
// `number == finalNumber`（映像の番号）に戻す。
// 壊し方: 同じ式の `finalNumber >= 0` を消す（伸びる入力で ENDLIST を終端にする）。
func TestSegmentTransformFinalIsDecidedPerRenditionOnlyForKnownLength(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       store.RecordingHLSInputKind
		duration   int64
		normalExit bool
		wantCached bool
	}{
		{name: "長さが確定していて正常終了", kind: store.RecordingHLSInputOriginal, duration: 8*mpegTSTimestampClock + 7200, normalExit: true, wantCached: true},
		{name: "長さが確定していても終了前は公開しない", kind: store.RecordingHLSInputOriginal, duration: 8*mpegTSTimestampClock + 7200, normalExit: false},
		{name: "伸びる入力の ENDLIST は終端ではない", kind: store.RecordingHLSInputTemp, duration: 0, normalExit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newBareSegmentManager(t, 1<<20)
			workDir := filepath.Join(t.TempDir(), "work")
			if err := os.MkdirAll(filepath.Join(workDir, "segments"), 0o750); err != nil {
				t.Fatal(err)
			}
			// 音声の rendition は番号 3 で ENDLIST になる。映像の最後の番号は 4。
			playlist := "#EXTM3U\n#EXTINF:2.0,\nsegments/test.1_seg00002.ts\n#EXTINF:2.0,\nsegments/test.1_seg00003.ts\n#EXT-X-ENDLIST\n"
			if err := os.WriteFile(filepath.Join(workDir, "test.1.m3u8"), []byte(playlist), 0o600); err != nil {
				t.Fatal(err)
			}
			// 映像は番号 4 で ENDLIST になる（確定した最後の番号に届いている）。
			videoPlaylist := "#EXTM3U\n#EXTINF:2.0,\nsegments/test.0_seg00002.ts\n#EXTINF:2.0,\nsegments/test.0_seg00003.ts\n#EXTINF:0.08,\nsegments/test.0_seg00004.ts\n#EXT-X-ENDLIST\n"
			if err := os.WriteFile(filepath.Join(workDir, "test.0.m3u8"), []byte(videoPlaylist), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"test.1_seg00002.ts", "test.1_seg00003.ts"} {
				if err := os.WriteFile(filepath.Join(workDir, "segments", name), []byte(name), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			group := segmentTransformGroup{"recording", "generation", "test", tc.kind}
			task := &segmentTransformTask{
				group: group, profile: LiveProfile{Name: "test", SegmentSeconds: 2},
				input:   SegmentTransformInput{Kind: tc.kind, VideoDurationPTS: tc.duration},
				workDir: workDir, requestStart: 2, startNumber: 2, currentNumber: 2, maxRequested: 4,
			}
			manager.tasks[group] = map[*segmentTransformTask]struct{}{task: {}}
			if err := manager.scanTransform(task, tc.normalExit); err != nil {
				t.Fatalf("scanTransform: %v", err)
			}
			lease, cached, _ := manager.cache.acquire(SegmentKey{"recording", "generation", "test", "audio-standard", 3})
			if cached {
				_ = lease.file.Close()
			}
			if cached != tc.wantCached {
				t.Fatalf("audio segment 3 (the last of its rendition) cached = %v, want %v", cached, tc.wantCached)
			}
			if lease, ok, _ := manager.cache.acquire(SegmentKey{"recording", "generation", "test", "audio-standard", 2}); ok {
				_ = lease.file.Close()
			} else {
				t.Error("audio segment 2 is not cached although segment 3 started")
			}
		})
	}
}

func TestBuildSegmentTransformArgsTrimsAudioAtTheVideoEndOnlyForKnownLength(t *testing.T) {
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2}
	build := func(durationPTS int64, start int64) []string {
		args, err := buildSegmentTransformArgs(
			LiveConfig{FFmpeg: "ffmpeg", Profiles: []LiveProfile{profile}}, profile,
			SegmentTransformInput{Path: "/dev/fd/3", OriginPTS: 900000, VideoDurationPTS: durationPTS, AudioStreamCount: 1},
			start, t.TempDir(),
		)
		if err != nil {
			t.Fatal(err)
		}
		return args
	}
	// 映像は原点から 8.08 秒。番号 3 から起動すると、起動位置（6 秒）から数えた終端は 2.08 秒。
	known := build(727200, 3)
	assertArgPair(t, known, "-filter:a:0", "atrim=start=16.000000000,asetpts=PTS-16.000000000/TB,apad,atrim=end=2.080000000")
	if index := indexOf(known, "-filter:a:1"); index < 0 || !strings.HasSuffix(known[index+1], ",apad,atrim=end=2.080000000") {
		t.Errorf("main audio filter = %v, want the end trim after the dual-mono pan and the start trim", known)
	}
	for _, arg := range known {
		if strings.Contains(arg, "whole_dur") {
			t.Errorf("args use apad whole_dur, which counts samples: %v", known)
		}
	}
	if args := build(0, 3); strings.Contains(strings.Join(args, " "), "apad") {
		t.Errorf("args of an input with unknown length contain apad: %v", args)
	}
}

// 入力が途中で切れた原本（確定した長さは 20 秒、実ファイルは 10 秒）では、音声が apad で無音のまま
// 映像の終端まで続く。映像が届いていない番号（6・9）の音声は、キャッシュに入れず 200 にしない。
//
// 壊し方: scanTransform の `number > videoLast` の continue を消し、isFinal の videoReached を外す。
func TestSegmentTransformTruncatedInputDoesNotPublishSilentAudioWithRealFFmpeg(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	path := filepath.Join(t.TempDir(), "short.ts")
	writeOffsetTestTS(t, ffmpeg, path, 5400, "10", "10")
	input := testSegmentInput(path, firstPacketPTS(t, ffprobe, path))
	input.Kind = store.RecordingHLSInputOriginal
	input.VideoDurationPTS = 20 * mpegTSTimestampClock
	manager := newRealSegmentManager(t, ffmpeg, t.TempDir(), 1, 64<<20, 2<<20)
	defer closeSegmentManager(t, manager)
	for _, rendition := range []string{"video", "audio-standard", "audio-main", "audio-sub"} {
		for _, number := range []int64{6, 9} {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			file, err := manager.Get(ctx, SegmentKey{"recording", "generation-1", "test", rendition, number}, input)
			cancel()
			if file != nil {
				_ = file.Close()
			}
			if err == nil {
				t.Errorf("%s segment %d of an input cut at 10 of 20 seconds was served", rendition, number)
			}
		}
	}
}

// 変換の進み（currentNumber）は、入力の長さによらず映像の進みだけで数える。音声が映像より先行すると
// （長さが確定した入力では apad の無音で。伸びる入力でも CPU 負荷下で 1 本の先行を観測した）、全 rendition で
// 数えた場合、映像が要求に届く前に先読み済みと判断して変換を止めてしまう。このテストが見るのは
// currentNumber の値だけである。止めた変換が追い出される形は、負荷下の
// TestSegmentTransformCachedRequestResumesPrefetchPausedTransformWithRealFFmpeg でだけ再現した。
// 作業ディレクトリでは、映像は番号 3 まで出し終えて 4 を書いている。音声は 5 まで出し終えて 6 を書いている。
//
// 壊し方: updateCurrentNumber の `entries["video"]` を全 rendition の走査に戻す（両方のケースが 6 になる）。
// 壊し方: 同じ関数の Glob を全 rendition の走査に戻す（音声の .tmp で 6 になる）。
func TestSegmentTransformCurrentNumberFollowsOnlyTheVideo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     store.RecordingHLSInputKind
		duration int64
		want     int64
	}{
		{name: "長さが確定した入力は映像の最後の次", kind: store.RecordingHLSInputOriginal, duration: 20 * mpegTSTimestampClock, want: 4},
		{name: "伸びる入力も映像の最後の次", kind: store.RecordingHLSInputTemp, duration: 0, want: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newBareSegmentManager(t, 1<<20)
			workDir := filepath.Join(t.TempDir(), "work")
			if err := os.MkdirAll(filepath.Join(workDir, "segments"), 0o750); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{
				"test.0.m3u8":                     "#EXTM3U\n#EXTINF:2.0,\nsegments/test.0_seg00002.ts\n#EXTINF:2.0,\nsegments/test.0_seg00003.ts\n",
				"segments/test.0_seg00004.ts.tmp": "video",
				"test.1.m3u8":                     "#EXTM3U\n#EXTINF:2.0,\nsegments/test.1_seg00002.ts\n#EXTINF:2.0,\nsegments/test.1_seg00003.ts\n#EXTINF:2.0,\nsegments/test.1_seg00004.ts\n#EXTINF:2.0,\nsegments/test.1_seg00005.ts\n",
				"segments/test.1_seg00006.ts.tmp": "audio",
			}
			for name, body := range files {
				if err := os.WriteFile(filepath.Join(workDir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			group := segmentTransformGroup{"recording", "generation", "test", tc.kind}
			task := &segmentTransformTask{
				group: group, profile: LiveProfile{Name: "test", SegmentSeconds: 2},
				input:   SegmentTransformInput{Kind: tc.kind, VideoDurationPTS: tc.duration},
				workDir: workDir, requestStart: 2, startNumber: 2, currentNumber: 2, maxRequested: 2,
			}
			manager.tasks[group] = map[*segmentTransformTask]struct{}{task: {}}
			if err := manager.scanTransform(task, false); err != nil {
				t.Fatalf("scanTransform: %v", err)
			}
			manager.mu.Lock()
			got := task.currentNumber
			manager.mu.Unlock()
			if got != tc.want {
				t.Fatalf("currentNumber = %d, want %d", got, tc.want)
			}
		})
	}
}
