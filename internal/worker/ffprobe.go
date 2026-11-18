package worker

// ffprobe.go は ffprobe の duration 取得ヘルパーを持つ。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/ffargs"
)

// probeDuration は encode の進捗と thumbnail の seek に使うコンテナ全体の尺を返す。
// 両方の呼び出し元が worker にあり、mediaexec.ProbeVideoDuration とは問い合わせる
// 対象と出力の判定が異なるため、コンテナの契約はここで共有する。
func probeDuration(ctx context.Context, ffprobe, inputPath string, run func(context.Context, string, ...string) ([]byte, error)) (time.Duration, error) {
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
