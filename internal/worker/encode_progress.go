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
// context.CancelFunc を返す。cancel は他の CancelFunc と同じく非同期・
// fire-and-forget（goroutine の終了を待たない）。cancel すると、次の ticker を
// 待たずに（report された最後の値が前回送信済みの値と違えば）goroutine が
// 1 回だけ flush してから終了する（TestEncodeProgressReporter_FlushesFinalValueOnStop）。
//
// **保証はここまで、ベストエフォート。** 唯一の実運用呼び出し元
// （encode.go の `defer stopProgress()`）は Work が return した後に走る
// 切り離された goroutine なので、この flush が encode.finished 通知（同じ
// encode.go の w.notify）より後に届く順序は排除していない（未検証: 実際に
// 遅延する頻度は測っていない）。また flush の notify には start に渡された
// ctx をそのまま使うため、親 ctx が cancel と同時にキャンセルされる経路
// （worker 停止・ジョブタイムアウト）では notify 自体が失敗し、flush は
// 実質何もしない（notifyCtx.Err() != nil のときは警告ログも出さない ---
// 「notify を打ち切らない」が成り立つのは、呼び出し元が親 ctx を生かした
// まま stop() だけを呼ぶ場合に限る）。
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

	// lastSent は「まだ report されていない」（latest の初期値と同じ番兵 -1）と
	// 「前回送信済みの値」を同じ 1 つの比較で表す。report() は非負の Duration
	// しか latest に書かないので、一度でも送った後は v が -1 に戻ることはなく、
	// 番兵チェックを別条件にする必要が無い。
	lastSent := int64(-1)
	flush := func() {
		v := latest.Load()
		if v == lastSent {
			// 区間内に新しいサンプルが来ていない（初回送信前も含む）か、前回
			// 送信した値から変わっていない。どちらも送らない（「区間内の最新値を
			// 1 回だけ送る」という pending 相当の判定）。
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
