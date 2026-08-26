package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/fetburner/rokuban/internal/serverevent"
)

const (
	encodeProgressInterval     = time.Second
	encodeDurationProbeTimeout = 3 * time.Second
)

// encodeProgressReporter は ffmpeg の読み取りと PostgreSQL NOTIFY を切り離し、
// 区間内の最新値だけを最大 interval ごとに 1 回送る。
type encodeProgressReporter struct {
	recordingID int64
	profile     string
	duration    time.Duration
	interval    time.Duration
	notify      func(context.Context, string) error
	log         *slog.Logger
}

func (r *encodeProgressReporter) start(ctx context.Context) (func(time.Duration), context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	samples := make(chan time.Duration, 1)
	go r.run(ctx, samples)

	report := func(outTime time.Duration) {
		select {
		case samples <- outTime:
			return
		default:
		}
		select {
		case <-samples:
		default:
		}
		select {
		case samples <- outTime:
		default:
		}
	}
	return report, cancel
}

func drainLatestProgress(samples <-chan time.Duration, latest time.Duration) (time.Duration, bool) {
	found := false
	for {
		select {
		case latest = <-samples:
			found = true
		default:
			return latest, found
		}
	}
}

func (r *encodeProgressReporter) run(ctx context.Context, samples <-chan time.Duration) {
	timer := time.NewTimer(r.interval)
	defer timer.Stop()

	var latest time.Duration
	pending := false
	for {
		select {
		case latest = <-samples:
			pending = true
		case <-timer.C:
			if buffered, found := drainLatestProgress(samples, latest); found {
				latest = buffered
				pending = true
			}
			if pending {
				progress := float64(latest) / float64(r.duration)
				progress = max(0, min(progress, 1))
				payload, _ := json.Marshal(serverevent.EncodeProgressEvent{
					Type:        serverevent.EncodeProgressEventType,
					RecordingID: r.recordingID,
					Profile:     r.profile,
					Progress:    progress,
				})
				if err := r.notify(ctx, string(payload)); err != nil && ctx.Err() == nil {
					r.log.Warn("encode: progress notify failed",
						"recording_id", r.recordingID, "profile", r.profile, "err", err)
				}
				pending = false
			}
			timer.Reset(r.interval)
		case <-ctx.Done():
			return
		}
	}
}
