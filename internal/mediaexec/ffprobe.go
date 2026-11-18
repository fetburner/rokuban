package mediaexec

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/ffargs"
)

// ProbeVideoDuration は run 経由で ffprobe を呼び、最初の映像ストリームの長さを返す。
// CM 検出と seek tiles は、映像の終端を決めるためにこの尺と出力の判定を共有する。
// TestProbeVideoDuration が取得対象と出力の判定を検査する。
func ProbeVideoDuration(ctx context.Context, run func(context.Context, string, ...string) ([]byte, error), ffprobe, inputPath string) (time.Duration, error) {
	out, err := run(ctx, ffargs.FFprobePath(ffprobe), ffargs.VideoStreamDurationProbeArgs(inputPath)...)
	if err != nil {
		return 0, err
	}
	s, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0, fmt.Errorf("ffprobe returned empty video duration")
	}
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing video duration %q: %w", s, err)
	}
	return time.Duration(sec * float64(time.Second)), nil
}
