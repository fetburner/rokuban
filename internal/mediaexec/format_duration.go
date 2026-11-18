package mediaexec

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/ffargs"
)

// ProbeFormatDuration は encode の進捗と thumbnail の seek に使うコンテナ全体の尺を返す。
// worker の encode と thumbnail はコンテナの尺と出力の判定を共有するため、
// 両パッケージが import できる共用の実行補助 mediaexec に置く。
// ProbeVideoDuration とは問い合わせる対象と出力の判定が異なる。
// TestProbeDuration_IgnoresStderr が stderr の診断を含む入力で尺の取得を検査する。
func ProbeFormatDuration(ctx context.Context, ffprobe, inputPath string, run func(context.Context, string, ...string) ([]byte, error)) (time.Duration, error) {
	out, err := run(ctx, ffargs.FFprobePath(ffprobe), ffargs.FormatDurationProbeArgs(inputPath)...)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" || s == "N/A" {
		return 0, fmt.Errorf("ffprobe returned empty duration")
	}
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing duration %q: %w", s, err)
	}
	if sec < 0 {
		return 0, fmt.Errorf("negative duration %v", sec)
	}
	return time.Duration(sec * float64(time.Second)), nil
}
