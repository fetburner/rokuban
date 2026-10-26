package streamer

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSegmentTransformSeekMeasure は、製品の SegmentManager（buildSegmentTransformArgs の実引数）が
// 出したセグメントの先頭フレームを、入力の復号フレーム列に照合して誤差を frame 単位で出す測定である。
// 環境変数が無ければ Skip する（CI では走らない）。読み取り専用で、入力は変更しない。
//
//	ROKUBAN_SEEK_INPUT       入力の TS。または ROKUBAN_SEEK_SYNTH=old-pes|wrap で合成入力を作る
//	ROKUBAN_SEEK_STARTS      起動するセグメント番号 k のカンマ区切り（既定 3,5）
//	ROKUBAN_SEEK_COUNT       k から数えて取るセグメント数（既定 6）
//	ROKUBAN_SEEK_FRAMES      基準にする入力の復号フレーム数（既定 900）
//	ROKUBAN_SEEK_AUDIO       入力の音声ストリーム数（既定 2。合成入力は 2 本）
//	ROKUBAN_SEEK_TOLERANCE   許す誤差の絶対値 frame（既定 1）
//
// 期待フレームは、入力の復号フレームのうち PTS が V + n·D 以上になる最初のもの（V は映像の最初の
// packet の PTS、n はセグメント番号）。先頭フレームの hash が入力内で一意でなければ失敗にする。
// 出力は QP=0 の無損失なので、復号した画素の hash が入力と一致する（縮小はしない）。
func TestSegmentTransformSeekMeasure(t *testing.T) {
	input := os.Getenv("ROKUBAN_SEEK_INPUT")
	synth := os.Getenv("ROKUBAN_SEEK_SYNTH")
	if input == "" && synth == "" {
		t.Skip("ROKUBAN_SEEK_INPUT or ROKUBAN_SEEK_SYNTH is not set")
	}
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	if input == "" {
		input = filepath.Join(t.TempDir(), "synth.ts")
		switch synth {
		case "old-pes":
			writeOldAudioPESTestTS(t, ffmpeg, input, 40)
		case "wrap":
			writeWrapTestTS(t, ffmpeg, input, 40)
		default:
			t.Fatalf("ROKUBAN_SEEK_SYNTH = %q, want old-pes or wrap", synth)
		}
	}
	envInt := func(name string, def int) int {
		v := os.Getenv(name)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("%s = %q: %v", name, v, err)
		}
		return n
	}
	count, frames, tolerance := envInt("ROKUBAN_SEEK_COUNT", 6), envInt("ROKUBAN_SEEK_FRAMES", 900), envInt("ROKUBAN_SEEK_TOLERANCE", 1)
	audio := envInt("ROKUBAN_SEEK_AUDIO", 2)
	var starts []int64
	for _, field := range strings.Split(envOr("ROKUBAN_SEEK_STARTS", "3,5"), ",") {
		k, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil {
			t.Fatalf("ROKUBAN_SEEK_STARTS: %v", err)
		}
		starts = append(starts, k)
	}

	originPTS := firstPacketPTS(t, ffprobe, input)
	reference := framehashes(t, ffmpeg, "-copyts", "-i", input, "-map", "0:v:0", "-an", "-frames:v", strconv.Itoa(frames), "-fps_mode", "passthrough")
	t.Logf("input %s: origin PTS %d, format start_time %d, %d reference frames", filepath.Base(input), originPTS, formatStartPTS(t, ffprobe, input), len(reference))

	qp := 0
	profile := LiveProfile{Name: "test", VideoCodec: "libx264", AudioCodec: "aac", Preset: "ultrafast", SegmentSeconds: 2, QP: &qp}
	for _, k := range starts {
		manager, err := NewSegmentManager(SegmentTransformOptions{
			Config:          LiveConfig{FFmpeg: ffmpeg, Profiles: []LiveProfile{profile}},
			ScratchRoot:     t.TempDir(),
			CacheBytes:      8 << 30,
			MaxSegmentBytes: 256 << 20,
			MaxConcurrent:   1,
			IdleTimeout:     2 * time.Minute,
			PollInterval:    2 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		for number := k; number < k+int64(count); number++ {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			file, err := manager.Get(ctx, SegmentKey{"recording", "generation-1", "test", "video", number}, SegmentTransformInput{
				OpenFile:         func(context.Context) (*os.File, error) { return os.Open(input) },
				OriginPTS:        originPTS,
				AudioStreamCount: audio,
			})
			cancel()
			if err != nil {
				t.Errorf("k=%d segment %d: %v", k, number, err)
				continue
			}
			got := framehashes(t, ffmpeg, "-i", file.Name(), "-map", "0:v:0", "-an", "-frames:v", "1")
			_ = file.Close()
			if len(got) != 1 {
				t.Errorf("k=%d segment %d: %d output frames hashed, want 1", k, number, len(got))
				continue
			}
			delta, err := seekFrameError(reference, got[0].hash, originPTS+number*2*mpegTSTimestampClock)
			if err != nil {
				t.Errorf("k=%d segment %d: %v", k, number, err)
				continue
			}
			t.Logf("k=%d n=%d error_frames=%+d", k, number, delta)
			if delta < -tolerance || delta > tolerance {
				t.Errorf("k=%d segment %d: first frame is %+d frames from the grid, tolerance %d", k, number, delta, tolerance)
			}
		}
		closeSegmentManager(t, manager)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

type hashedFrame struct {
	pts  *big.Rat // 秒ではなく入力の time base の tick を 90 kHz に換算した値
	hash string
}

// seekFrameError は、hash が一致する入力フレームの番号から、target tick（90 kHz）以上になる最初の
// 入力フレームの番号を引いた値を返す。hash が入力内で一意でなければエラーにする。
func seekFrameError(reference []hashedFrame, hash string, target int64) (int, error) {
	matched := -1
	for i, f := range reference {
		if f.hash != hash {
			continue
		}
		if matched >= 0 {
			return 0, fmt.Errorf("first frame hash %s is not unique in the input (frames %d and %d)", hash, matched, i)
		}
		matched = i
	}
	if matched < 0 {
		return 0, fmt.Errorf("first frame hash %s is not in the %d reference frames", hash, len(reference))
	}
	want := new(big.Rat).SetInt64(target)
	for i, f := range reference {
		if f.pts.Cmp(want) >= 0 {
			return matched - i, nil
		}
	}
	return 0, fmt.Errorf("target %d is beyond the reference frames", target)
}

// framehashes は ffmpeg の framehash（md5）を実行し、frame ごとの PTS（90 kHz の tick）と hash を返す。
// args には入力オプションと出力オプションを渡す。
func framehashes(t *testing.T, ffmpeg string, args ...string) []hashedFrame {
	t.Helper()
	full := append([]string{"-hide_banner", "-v", "error", "-threads", "1"}, args...)
	// -enc_time_base を付けないと、PTS が frame rate の time base（25 fps なら 1/25）に丸められ、
	// 90 kHz の端数（最大で 1 frame 分）が落ちて期待 frame が 1 つずれる。
	full = append(full, "-enc_time_base:v", "1/90000", "-f", "framehash", "-hash", "md5", "-")
	out, err := exec.Command(ffmpeg, full...).Output()
	if err != nil {
		t.Fatalf("ffmpeg framehash %v: %v", args, err)
	}
	tb := new(big.Rat)
	var result []hashedFrame
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "#tb 0:"):
			if _, ok := tb.SetString(strings.TrimSpace(strings.TrimPrefix(line, "#tb 0:"))); !ok {
				t.Fatalf("framehash time base %q", line)
			}
		case strings.HasPrefix(line, "0,"):
			fields := strings.Split(line, ",")
			pts, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
			if err != nil || tb.Sign() == 0 {
				t.Fatalf("framehash line %q (time base %v): %v", line, tb, err)
			}
			ticks := new(big.Rat).Mul(new(big.Rat).SetInt64(pts), tb)
			ticks.Mul(ticks, new(big.Rat).SetInt64(mpegTSTimestampClock))
			result = append(result, hashedFrame{pts: ticks, hash: strings.TrimSpace(fields[len(fields)-1])})
		}
	}
	return result
}

// writeWrapTestTS は、映像の PTS が 2^33 tick（約 95,443.7 秒）の境界を 10 秒目にまたぐ音声 2 本付きの TS を書く。
func writeWrapTestTS(t *testing.T, ffmpeg, path string, seconds int) {
	t.Helper()
	video := fmt.Sprintf("testsrc2=size=160x90:rate=25:duration=%d", seconds)
	audio := fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", seconds)
	cmd := exec.Command(ffmpeg, "-v", "error", "-y",
		"-itsoffset", "95433.7", "-f", "lavfi", "-i", video,
		"-itsoffset", "95433.7", "-f", "lavfi", "-i", audio,
		"-itsoffset", "95433.7", "-f", "lavfi", "-i", audio,
		"-map", "0:v:0", "-map", "1:a:0", "-map", "2:a:0",
		"-c:v", "mpeg2video", "-q:v", "3", "-g", "50", "-bf", "0", "-sc_threshold", "0", "-c:a", "mp2", "-b:a", "128k",
		"-mpegts_copyts", "1", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating wrap test TS: %v\n%s", err, out)
	}
}

func TestSeekFrameErrorPicksFirstFrameAtOrAfterTargetAndRejectsAmbiguousHash(t *testing.T) {
	var reference []hashedFrame
	for i := 0; i < 10; i++ {
		reference = append(reference, hashedFrame{pts: big.NewRat(int64(1000+i*3600), 1), hash: strconv.Itoa(i)})
	}
	// target が frame 4 の PTS ちょうどなら、frame 4 が期待で、frame 3 は -1、frame 5 は +1。
	for hash, want := range map[string]int{"3": -1, "4": 0, "5": 1} {
		if got, err := seekFrameError(reference, hash, 1000+4*3600); err != nil || got != want {
			t.Errorf("hash %s = (%d, %v), want %d", hash, got, err, want)
		}
	}
	// target が frame 3 と 4 の間なら、期待は frame 4。
	if got, err := seekFrameError(reference, "3", 1000+3*3600+1); err != nil || got != -1 {
		t.Errorf("target between frames: got (%d, %v), want -1", got, err)
	}
	if _, err := seekFrameError(append(reference, hashedFrame{pts: big.NewRat(99999, 1), hash: "4"}), "4", 1000); err == nil {
		t.Error("duplicate hash: want error")
	}
	if _, err := seekFrameError(reference, "missing", 1000); err == nil {
		t.Error("missing hash: want error")
	}
	if _, err := seekFrameError(reference, "4", 1<<40); err == nil {
		t.Error("target beyond reference: want error")
	}
}
