package ffargs

import (
	"encoding/json"
	"fmt"
)

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
