package ffargs

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// VideoGeometryProbeArgs は最初の映像ストリームの記録上の大きさを問う ffprobe の
// argv を返す。worker（CM 検出）と streamer（/frame）が同じ問い合わせをするための
// 共有点で、出力は ParseVideoGeometryWithSAR で読む。
//
// **csv は使わない。** MPEG-TS では programs 側と streams 側で 2 回出力され、
// MPEG-2 では末尾に空欄が付く（実測: ffprobe 9.0.2 で "1440x1080x\n\n1440x1080x"）。
// json なら top-level の streams[0] だけを読める。
func VideoGeometryProbeArgs(path string) []string {
	return []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,sample_aspect_ratio",
		"-of", "json",
		path,
	}
}

// VideoGeometry は映像ストリームの記録上の大きさと SAR を表す。
// Width / Height は SAR を適用する前の coded size である。
type VideoGeometry struct {
	Width             int
	Height            int
	SampleAspectRatio string
}

// ParseVideoGeometry は VideoGeometryProbeArgs の出力から幅と高さ（SAR 適用前）を返す。
// SAR が不要な worker 側の既存の呼び出しを保つためのラッパーである。
func ParseVideoGeometry(out []byte) (width, height int, err error) {
	geometry, err := ParseVideoGeometryWithSAR(out)
	if err != nil {
		return 0, 0, err
	}
	return geometry.Width, geometry.Height, nil
}

// ParseVideoGeometryWithSAR は VideoGeometryProbeArgs の出力から coded size と
// sample aspect ratio を返す。SAR が欠落・不正・0 のときは正方画素 (1:1) とする。
func ParseVideoGeometryWithSAR(out []byte) (VideoGeometry, error) {
	var doc struct {
		Streams []struct {
			Width             int    `json:"width"`
			Height            int    `json:"height"`
			SampleAspectRatio string `json:"sample_aspect_ratio"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return VideoGeometry{}, fmt.Errorf("parsing ffprobe video size: %w", err)
	}
	if len(doc.Streams) == 0 {
		return VideoGeometry{}, fmt.Errorf("ffprobe returned no video stream")
	}
	s := doc.Streams[0]
	if s.Width <= 0 || s.Height <= 0 {
		return VideoGeometry{}, fmt.Errorf("ffprobe returned a non-positive video size %dx%d", s.Width, s.Height)
	}
	return VideoGeometry{
		Width:             s.Width,
		Height:            s.Height,
		SampleAspectRatio: normalizeSampleAspectRatio(s.SampleAspectRatio),
	}, nil
}

func normalizeSampleAspectRatio(value string) string {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return "1:1"
	}
	numerator, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	denominator, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || numerator <= 0 || denominator <= 0 {
		return "1:1"
	}
	return strconv.Itoa(numerator) + ":" + strconv.Itoa(denominator)
}
