package streamer

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// lookPathFFprobe は lookPathFFmpeg の ffprobe 版（ROKUBAN_REQUIRE_FFMPEG で skip を禁じる）。
func lookPathFFprobe(t *testing.T) string {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		if os.Getenv("ROKUBAN_REQUIRE_FFMPEG") != "" {
			t.Fatalf("ffprobe not in PATH but ROKUBAN_REQUIRE_FFMPEG is set: %v", err)
		}
		t.Skip("ffprobe not in PATH")
	}
	return ffprobe
}

const (
	// realOriginalVideoSeconds は合成 TS の映像の長さ。音声は realOriginalAudioSeconds
	// まで続くので format の duration は映像の終端より約 1.5 秒長い（実測した録画と同じ形。
	// 録画では format 660.010 / 映像 660.000）。
	realOriginalVideoSeconds = 40
	realOriginalAudioSeconds = 41.5
	// 映像の輝度は round(T*4)（T は録画先頭からの秒）。先頭フレームの輝度（Y 面。yuv420p のまま読み、レンジ変換を挟まない）から時刻を読む。
	realOriginalLumaPerSecond = 4
)

// writeSyntheticOriginal は輝度が時刻に比例する MPEG-2 + MP2 の TS を書く。
func writeSyntheticOriginal(t *testing.T, ffmpeg, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating original directory: %v", err)
	}
	video := fmt.Sprintf("color=c=black:s=64x64:r=25:d=%d,geq=lum='round(T*%d)':cb=128:cr=128",
		realOriginalVideoSeconds, realOriginalLumaPerSecond)
	audio := fmt.Sprintf("sine=f=440:d=%v", realOriginalAudioSeconds)
	out, err := exec.Command(ffmpeg, "-v", "error", "-y",
		"-f", "lavfi", "-i", video, "-f", "lavfi", "-i", audio,
		"-c:v", "mpeg2video", "-g", "12", "-c:a", "mp2", "-f", "mpegts", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generating synthetic TS: %v\n%s", err, out)
	}
}

// firstFrameSeconds は HLS セグメントの先頭フレームに焼いた時刻（録画先頭からの秒）を返す。
func firstFrameSeconds(t *testing.T, ffmpeg, segment string) float64 {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command(ffmpeg, "-v", "error", "-i", segment, "-frames:v", "1",
		"-vf", "scale=1:1", "-pix_fmt", "yuv420p", "-f", "rawvideo", "-")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		t.Fatalf("decoding first frame of %s: %v (%d bytes)\n%s", segment, err, len(out), stderr.String())
	}
	return float64(out[0]) / realOriginalLumaPerSecond
}

// TestOriginalVODOffsetRealFFmpegSeekAccuracyAndRange は本物の ffmpeg / ffprobe で
// runSession / probeOriginalVODDuration の経路を通し、(1) offset の先頭フレームが
// 要求位置から 0.5 秒以内であること、(2) 映像が残らない offset
// （format の duration の整数部を含む）が 416 で即座に返ることを固定する。
func TestOriginalVODOffsetRealFFmpegSeekAccuracyAndRange(t *testing.T) {
	ffmpeg := lookPathFFmpeg(t)
	ffprobe := lookPathFFprobe(t)
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	relPath := "recordings/synthetic.ts"
	fullPath := filepath.Join(mediaDir, relPath)
	writeSyntheticOriginal(t, ffmpeg, fullPath)
	info, err := os.Stat(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	recordingID := seedRecording(t, pool)
	seedAsset(t, pool, recordingID, relPath, info.Size())

	cfg := originalVODConfig(t, mediaDir, ffmpeg)
	cfg.FFprobe = ffprobe
	cfg.Profiles = cfg.Profiles[:1]
	cfg.MaxSessions = 8
	ls, srv := newOriginalVODTestServer(t, pool, cfg)

	for _, offset := range []int64{0, 7, 20, 38} {
		t.Run(fmt.Sprintf("accuracy/%d", offset), func(t *testing.T) {
			resp, body := get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, offset, "hd"), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("offset %d status/body = %d %q, want 200", offset, resp.StatusCode, body)
			}
			ls.mu.Lock()
			s := ls.chaseSessions[originalVODSessionKeyFor(recordingID, offset)]
			ls.mu.Unlock()
			if s == nil {
				t.Fatalf("offset %d session was not retained", offset)
			}
			select {
			case <-s.done:
			case <-time.After(60 * time.Second):
				t.Fatalf("ffmpeg for offset %d did not finish", offset)
			}
			segment := filepath.Join(originalVODSessionDir(ls.cfg.SegmentDir, testSite, recordingID, offset),
				"segments", "hd.0_seg00000.ts")
			got := firstFrameSeconds(t, ffmpeg, segment)
			pts, _ := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
				"-show_entries", "packet=pts_time", "-read_intervals", "%+#1",
				"-of", "csv=p=0", segment).Output()
			t.Logf("offset %d first segment video pts_time=%s", offset, bytes.TrimSpace(pts))
			t.Logf("offset %d first frame at %.2fs (diff %+.2fs)", offset, got, got-float64(offset))
			if diff := got - float64(offset); diff < -0.5 || diff > 0.5 {
				t.Fatalf("offset %d first frame at %.2fs (diff %+.2fs), want within 0.5s", offset, got, diff)
			}
		})
	}

	// format の duration は 41.5 秒前後、映像は 40 秒で終わる。41 は format の
	// duration の整数部で、映像が残らない（以前は 15 秒待って 504 になった）。
	for _, offset := range []int64{40, 41, 42} {
		t.Run(fmt.Sprintf("no-video-left/%d", offset), func(t *testing.T) {
			started := time.Now()
			resp, body := get(t, originalVODOffsetPlaylistURL(srv.URL, recordingID, offset, "hd"), nil)
			if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("offset %d status/body = %d %q, want 416", offset, resp.StatusCode, body)
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("offset %d took %v to return 416, want immediate", offset, elapsed)
			}
		})
	}
}
