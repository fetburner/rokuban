package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
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

// start は progress 更新を受け取る report 関数と、レポーターを止める
// context.CancelFunc を返す。キャンセルすると、次の ticker を待たずに
// （report された最後の値が前回送信済みの値と違えば）1 回だけ flush してから
// goroutine が終了する --- ジョブ終了直前の値を取りこぼさない。flush の
// notify には常に start に渡された ctx を使う（キャンセルは goroutine を
// 止めるための内部シグナルにすぎず、notify 自体を打ち切る理由にしない）。
func (r *encodeProgressReporter) start(ctx context.Context) (func(time.Duration), context.CancelFunc) {
	stopCtx, cancel := context.WithCancel(ctx)

	var latest atomic.Int64
	latest.Store(-1) // 「まだ report されていない」を表す番兵（out_time は負にならない）
	report := func(outTime time.Duration) {
		latest.Store(int64(outTime))
	}

	go r.run(ctx, stopCtx, &latest)

	return report, cancel
}

func (r *encodeProgressReporter) run(notifyCtx, stopCtx context.Context, latest *atomic.Int64) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	lastSent := int64(-1)
	flush := func() {
		v := latest.Load()
		if v < 0 || v == lastSent {
			// v < 0: report() を一度も受け取っていない。v == lastSent: 前回送信した
			// 値から変わっていない。どちらも送らない（「区間内の最新値を 1 回だけ
			// 送る」という pending 相当の判定）。
			return
		}
		lastSent = v

		progress := float64(v) / float64(r.duration)
		progress = max(0, min(progress, 1))
		payload, _ := json.Marshal(serverevent.EncodeProgressEvent{
			Type:        serverevent.EncodeProgressEventType,
			RecordingID: r.recordingID,
			Profile:     r.profile,
			Progress:    progress,
		})
		if err := r.notify(notifyCtx, string(payload)); err != nil && notifyCtx.Err() == nil {
			r.log.Warn("encode: progress notify failed",
				"recording_id", r.recordingID, "profile", r.profile, "err", err)
		}
	}

	for {
		select {
		case <-ticker.C:
			flush()
		case <-stopCtx.Done():
			flush()
			return
		}
	}
}
