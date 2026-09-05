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
// fire-and-forget で、reporter goroutine の終了を待たずに戻る ---
// 唯一の実運用呼び出し元 encode.go の `defer stopProgress()` は runEncode の
// return 時に**同期的に**呼ばれるので、ここを同期（goroutine の終了を待つ）に
// すると runEncode の return そのものが最後の NOTIFY の DB 往復ぶん遅れる。
// cancel すると、次の ticker を待たずに（report された最後の値が前回送信済みの
// 値と違えば）reporter goroutine が 1 回だけ flush してから終了する
// （TestEncodeProgressReporter_FlushesFinalValueOnStop）。
//
// **保証はここまで、ベストエフォート。** flush 自体は runEncode の return から
// 切り離された goroutine で非同期に走るので、encode.finished 通知（同じ
// encode.go の w.notify。defer stopProgress() より前に呼ばれる）より後に届く
// 順序は排除していない（未検証: 実際に遅延する頻度は測っていない）。また
// flush の notify には start に渡された ctx をそのまま使うため、親 ctx が
// cancel と同時にキャンセルされる経路（worker 停止。encode ジョブに
// ジョブタイムアウトの経路は無い --- EncodeWorker.Timeout は -1 を返し、river
// は jobTimeout > 0 のときしか派生 ctx を作らない。
// river@v0.47.0 internal/jobexecutor/job_executor.go の
// `cmp.Or(e.WorkUnit.Timeout(), e.ClientJobTimeout)` 参照）では notify 自体が
// 失敗し、flush は実質何もしない（notifyCtx.Err() != nil のときは警告ログも
// 出さない --- 「notify を打ち切らない」が成り立つのは、呼び出し元が親 ctx を
// 生かしたまま stop() だけを呼ぶ場合に限る）。
func (r *encodeProgressReporter) start(ctx context.Context) (func(time.Duration), context.CancelFunc) {
	stopCtx, cancel := context.WithCancel(ctx)

	var latest atomic.Int64
	// 「まだ report されていない」を表す番兵（-1ns）。実運用での唯一の呼び出し元
	// parseFFmpegProgress は常に time.Duration(ms) * time.Millisecond（1e6ns の
	// 倍数）を渡すので、out_time が負であっても -1 と衝突しない。
	latest.Store(-1)
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
	// 「前回送信済みの値」を同じ 1 つの比較で表す。latest に書かれる実値は
	// 常に 1e6ns（1ms）の倍数（parseFFmpegProgress 参照。符号は問わない ---
	// 負の out_time が来ても -1 とは一致しない）なので、番兵チェックを別条件に
	// する必要が無い。
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
