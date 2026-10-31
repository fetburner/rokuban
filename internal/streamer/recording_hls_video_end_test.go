package streamer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// writeOffsetTestTS は、全ストリームが offsetSeconds 秒から始まる TS を書く。映像と音声の長さは別々に決められる。
func writeOffsetTestTS(t *testing.T, ffmpeg, path string, offsetSeconds int, videoDuration, audioDuration string) {
	t.Helper()
	writeOffsetTestTSWithVideo(t, ffmpeg, path, offsetSeconds, videoDuration, audioDuration, "25", "0")
}

// writeOffsetTestTSWithVideo は writeOffsetTestTS のフレームレートと B フレーム数を選べる版。
func writeOffsetTestTSWithVideo(t *testing.T, ffmpeg, path string, offsetSeconds int, videoDuration, audioDuration, rate, bFrames string) {
	t.Helper()
	video := fmt.Sprintf("testsrc2=size=160x90:rate=%s:duration=%s", rate, videoDuration)
	audio := fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%s", audioDuration)
	offset := fmt.Sprint(offsetSeconds)
	cmd := exec.Command(ffmpeg, "-v", "error", "-y",
		"-itsoffset", offset, "-f", "lavfi", "-i", video,
		"-itsoffset", offset, "-f", "lavfi", "-i", audio,
		"-itsoffset", offset, "-f", "lavfi", "-i", audio,
		"-map", "0:v:0", "-map", "1:a:0", "-map", "2:a:0",
		"-c:v", "mpeg2video", "-q:v", "3", "-g", "50", "-bf", bFrames, "-sc_threshold", "0",
		"-c:a", "mp2", "-b:a", "128k",
		"-mpegts_copyts", "1", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating test TS: %v\n%s", err, out)
	}
}

// withRecordingHLSTailWindows は、テストの間だけ末尾の窓の候補を差し替える。
func withRecordingHLSTailWindows(t *testing.T, windows ...int64) {
	t.Helper()
	original := recordingHLSTailWindows
	recordingHLSTailWindows = windows
	t.Cleanup(func() { recordingHLSTailWindows = original })
}

func measureTestFile(t *testing.T, ffprobe, path string) (int64, error) {
	t.Helper()
	duration, _, err := measureTestFileSpan(t, ffprobe, path)
	return duration, err
}

// measureTestFileSpan は、製品の測り方で長さと最後のフレームの開始時刻を測る。
func measureTestFileSpan(t *testing.T, ffprobe, path string) (int64, int64, error) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return measureRecordingHLSVideoDuration(t.Context(), ffprobe, file, info.Size(), firstPacketPTS(t, ffprobe, path))
}

// 実 ffprobe に末尾だけを標準入力で渡しても、ファイル全体を渡したときと同じ長さになる。
// 2^33 tick の巻き戻りをまたぐ録画（先頭の PTS が巻き戻りの 0.7 秒前）でも、長さは 20 秒になる。
//
// 壊し方: recordingHLSVideoDurationFromPackets の `%recordingHLSPTSModulus` を消す（巻き戻りの場合が負になる）。
// 壊し方: probe の videoDuration を ffprobe の stream duration に戻すのはここでは検出できない
// （末尾の窓を 64 KiB にしても全体と一致することと、20 秒であることだけを見る）。
func TestMeasureRecordingHLSVideoDurationWithRealFFprobe(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	for _, tc := range []struct {
		name   string
		offset int
	}{
		{"通常の PTS", 5400},
		{"2^33 の 0.7 秒手前から始まり巻き戻る PTS", 95443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recording.ts")
			writeOffsetTestTS(t, ffmpeg, path, tc.offset, "20", "20")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() < 4<<16 {
				t.Fatalf("fixture is %d bytes, want it to be larger than the tail window", info.Size())
			}

			withRecordingHLSTailWindows(t, 1<<30)
			whole, err := measureTestFile(t, ffprobe, path)
			if err != nil {
				t.Fatalf("measuring the whole file: %v", err)
			}
			withRecordingHLSTailWindows(t, 64<<10)
			tail, err := measureTestFile(t, ffprobe, path)
			if err != nil {
				t.Fatalf("measuring only the last 64 KiB: %v", err)
			}
			if tail != whole {
				t.Errorf("tail-only duration = %d ticks, whole-file duration = %d ticks, want them equal", tail, whole)
			}
			if want := 20 * mpegTSTimestampClock; tail < want-3600 || tail > want+3600 {
				t.Errorf("duration = %d ticks, want 20 seconds ± one 25 fps frame (%d)", tail, want)
			}
		})
	}
}

// 末尾の窓に映像パケットが無ければ窓を広げて測り直し、最大の窓にも無ければ errRecordingHLSNoVideoPacket を返す。
//
// 壊し方: measureRecordingHLSVideoDuration の窓の繰り返しを 1 回目で return にする。
func TestMeasureRecordingHLSVideoDurationWidensTheWindow(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	path := filepath.Join(t.TempDir(), "recording.ts")
	writeOffsetTestTS(t, ffmpeg, path, 5400, "10", "10")
	withRecordingHLSTailWindows(t, 1<<30)
	want, err := measureTestFile(t, ffprobe, path)
	if err != nil {
		t.Fatal(err)
	}
	// 映像の後ろに、映像を含まない 1 MiB の null パケットを足す。
	nullPacket := append([]byte{0x47, 0x1F, 0xFF, 0x10}, bytes.Repeat([]byte{0xFF}, 184)...)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(bytes.Repeat(nullPacket, (1<<20)/len(nullPacket))); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	withRecordingHLSTailWindows(t, 256<<10, 64<<20)
	got, err := measureTestFile(t, ffprobe, path)
	if err != nil || got != want {
		t.Fatalf("duration after widening = (%d, %v), want (%d, nil)", got, err, want)
	}
	withRecordingHLSTailWindows(t, 256<<10)
	if _, err := measureTestFile(t, ffprobe, path); !errors.Is(err, errRecordingHLSNoVideoPacket) {
		t.Fatalf("duration with a window that never reaches the video = %v, want errRecordingHLSNoVideoPacket", err)
	}
}

// ffprobe に渡るのは、標準入力に流した末尾の窓だけである。/dev/fd/N で fd を渡すと、Linux では
// 通常ファイルを先頭から開き直すので、末尾だけを渡したことにならない。
//
// 壊し方: measureRecordingHLSVideoDuration の `io.NewSectionReader(file, offset, size-offset)` の offset を 0 にする。
func TestMeasureRecordingHLSVideoDurationPipesOnlyTheTail(t *testing.T) {
	dir := t.TempDir()
	received := filepath.Join(dir, "received")
	ffprobe := filepath.Join(dir, "recording-ffprobe")
	script := fmt.Sprintf("#!/bin/sh\ncat > %q\nprintf 'pts=90000|duration=3600|\\n'\n", received)
	if err := os.WriteFile(ffprobe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("0123456789abcdef"), 1024) // 16 KiB
	path := filepath.Join(dir, "input.ts")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	content = append(content, []byte("tail-bytes-written-after-the-open")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	withRecordingHLSTailWindows(t, 4096)
	if _, _, err := measureRecordingHLSVideoDuration(t.Context(), ffprobe, file, int64(len(content)), 0); err != nil {
		t.Fatalf("measure: %v", err)
	}
	got, err := os.ReadFile(received)
	if err != nil {
		t.Fatal(err)
	}
	if want := content[len(content)-4096:]; !bytes.Equal(got, want) {
		t.Fatalf("ffprobe received %d bytes, want exactly the last 4096 bytes of the input", len(got))
	}
}

// ffprobe が成功しても出力に映像パケットが無い窓（パケットが全て pts 無しなど）は、次の大きさの窓で測り直す。
//
// 壊し方: measureRecordingHLSVideoDuration の `lastErr = nil` の後の `if offset == 0 { break }` を無条件の break にする。
func TestMeasureRecordingHLSVideoDurationWidensWhenTheWindowHasNoPacket(t *testing.T) {
	dir := t.TempDir()
	ffprobe := filepath.Join(dir, "recording-ffprobe")
	// 標準入力が 4096 バイトを超えたときだけ、映像パケットを返す。
	script := "#!/bin/sh\nsize=$(wc -c)\nif [ \"$size\" -gt 4096 ]; then printf 'pts=90000|duration=3600|\\n'; else printf 'pts=N/A|duration=N/A|\\n'; fi\n"
	if err := os.WriteFile(ffprobe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "input.ts")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 16384), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	withRecordingHLSTailWindows(t, 1024, 8192)
	got, _, err := measureRecordingHLSVideoDuration(t.Context(), ffprobe, file, 16384, 0)
	if err != nil || got != 93600 {
		t.Fatalf("duration = (%d, %v), want (93600, nil) from the wider window", got, err)
	}
}

// ffprobe が居座っても、呼び出し元の ctx で止まる。
func TestMeasureRecordingHLSVideoDurationStopsWithContext(t *testing.T) {
	ffprobe := filepath.Join(t.TempDir(), "sleeping-ffprobe")
	if err := os.WriteFile(ffprobe, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input.ts")
	if err := os.WriteFile(path, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := measureRecordingHLSVideoDuration(ctx, ffprobe, file, 5, 0); err == nil {
		t.Fatal("measure returned no error while ffprobe was still running")
	}
}

// duration が N/A になる小さな窓で、B フレームがあっても、ファイルの切り出しを長くして長さが減らない。
// 最大の 2 つの pts の差を補完に使うと、I/P フレームの直後で切れたときに 3 フレームぶん過大になり、
// 続く切り出しで長さが 3003 / 6006 tick 縮む。
//
// 壊し方: recordingHLSVideoDurationFromPackets の interval を、最大の 2 つの pts の差にする。
func TestMeasureRecordingHLSVideoDurationDoesNotShrinkWithBFrames(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	path := filepath.Join(t.TempDir(), "bframes.ts")
	writeOffsetTestTSWithVideo(t, ffmpeg, path, 5400, "10", "10", "30000/1001", "2")
	origin := firstPacketPTS(t, ffprobe, path)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	withRecordingHLSTailWindows(t, 64<<10)
	var previous, previousLast int64
	steps := 0
	for size := int64(96 << 10); size <= info.Size(); size += 1500 {
		got, last, err := measureRecordingHLSVideoDuration(t.Context(), ffprobe, file, size, origin)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if got < previous {
			t.Fatalf("size %d: duration %d ticks is shorter than %d ticks measured from a shorter prefix", size, got, previous)
		}
		// 録画中の行は最後のフレームの開始時刻で決まるので、こちらも縮んではならない。
		if last < previousLast {
			t.Fatalf("size %d: last frame start %d ticks is earlier than %d ticks measured from a shorter prefix", size, last, previousLast)
		}
		previous, previousLast = got, last
		steps++
	}
	if steps < 20 {
		t.Fatalf("only %d prefixes were measured, want the fixture to be larger", steps)
	}
}
