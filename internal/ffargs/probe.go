package ffargs

import (
	"encoding/json"
	"fmt"
)

// SquarePixelsFilter は JPEG 化する前に sample aspect ratio を画素へ焼き込む。
const SquarePixelsFilter = "scale=round(iw*sar/2)*2:ih,setsar=1"

// FFmpegPath は設定された ffmpeg のパスを返す。空なら PATH 上の既定名を返す。
func FFmpegPath(configured string) string {
	if configured == "" {
		return "ffmpeg"
	}
	return configured
}

// FFprobePath は設定された ffprobe のパスを返す。空なら PATH 上の既定名を返す。
func FFprobePath(configured string) string {
	if configured == "" {
		return "ffprobe"
	}
	return configured
}

// SubtitleProbeArgs は字幕 stream の有無を調べる ffprobe の argv を返す。
// inputArgs はファイルパス、または `-i`, `pipe:0` のような入力指定をそのまま渡す。
// probeSize と analyzeDuration が空でなければ、その上限を先頭に付ける。
func SubtitleProbeArgs(inputArgs []string, probeSize, analyzeDuration string) []string {
	args := []string{"-v", "error"}
	if probeSize != "" {
		args = append(args, "-probesize", probeSize)
	}
	if analyzeDuration != "" {
		args = append(args, "-analyzeduration", analyzeDuration)
	}
	args = append(args,
		"-select_streams", "s",
		"-show_entries", "stream=index",
		"-of", "csv=p=0",
	)
	return append(args, inputArgs...)
}

// 尺の定義は用途ごとに異なるため、次の 3 builder は統合しない。

// FormatDurationProbeArgs は format 全体の尺を読む argv を返す。サムネイルの seek 位置と
// エンコード進捗の分母が、worker の probeDuration 経由でこの尺を使う。
func FormatDurationProbeArgs(inputPath string) []string {
	return []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inputPath,
	}
}

// VideoStreamDurationProbeArgs は音声などの尺に引きずられない映像 stream の長さを読む argv を返す。
func VideoStreamDurationProbeArgs(inputPath string) []string {
	return []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inputPath,
	}
}

// OriginalVODDurationProbeArgs は原本 HLS の尺計算に使う format と映像 stream の時刻を読む argv を返す。
func OriginalVODDurationProbeArgs(inputPath string) []string {
	return []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "format=start_time,duration:stream=start_time,duration",
		"-of", "json",
		"-i", inputPath,
	}
}

// VideoGeometryProbeArgs は最初の映像ストリームの記録上の大きさを問う ffprobe の
// argv を返す。worker（CM 検出）と streamer（/frame）が同じ問い合わせをするための
// 共有点で、出力は ParseVideoGeometry で読む。
//
// **csv は使わない。** MPEG-TS では programs 側と streams 側で 2 回出力され、
// MPEG-2 では末尾に空欄が付く（実測: ffprobe 9.0.2 で "1440x1080x\n\n1440x1080x"）。
// json なら top-level の streams[0] だけを読める。
func VideoGeometryProbeArgs(path string) []string {
	return []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "json",
		path,
	}
}

// ParseVideoGeometry は VideoGeometryProbeArgs の出力から幅と高さ（SAR 適用前）を返す。
func ParseVideoGeometry(out []byte) (width, height int, err error) {
	var doc struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return 0, 0, fmt.Errorf("parsing ffprobe video size: %w", err)
	}
	if len(doc.Streams) == 0 {
		return 0, 0, fmt.Errorf("ffprobe returned no video stream")
	}
	s := doc.Streams[0]
	if s.Width <= 0 || s.Height <= 0 {
		return 0, 0, fmt.Errorf("ffprobe returned a non-positive video size %dx%d", s.Width, s.Height)
	}
	return s.Width, s.Height, nil
}
