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

// probeVideoDuration は run 経由で ffprobe を呼び、最初の映像ストリームの長さを返す。
// SeekTilesWorker と CMDetectWorker が共有する。
func probeVideoDuration(ctx context.Context, run func(context.Context, string, ...string) ([]byte, error), ffprobe, inputPath string) (time.Duration, error) {
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
