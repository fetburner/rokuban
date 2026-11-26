package riveradapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// deliverableCountPageSize は JobList の 1 ページの件数。River の上限（10,000）より小さく取る。
const deliverableCountPageSize = 1000

// deliverableStates は件数に入りうる状態。running は含めない（lease が有効な実行中は数えない）。
// lease が切れた running は River の rescuer が retryable に戻すので、期限が来れば retryable として数えに入る。
var deliverableStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStateRetryable,
	rivertype.JobStateScheduled,
}

// CountDeliverable は物理キューを 1 台の worker が引き受けた場合に配送される件数を数える。
//
// River は件数を返す公開 API を持たない（v0.51.2 の Client は JobList と JobGet を持つ）。
// river_job を SQL で数えると内部の表に依存するので、公開の JobList をページ送りで読み、
// Go 側で期限を比べる。JobListParams.Where は列名に依存するので使わない。
// 数えるのは available 全件と、scheduled_at が now 以前の retryable と scheduled。
// asynq 構成の CountDeliverable と同じ意味にするための絞り込みである。
// 滞留が多いと件数の取得は遅くなる。
//
// 一度も投入されていないキューは JobList が空を返すので 0 件になる。DB の失敗は 0 件に
// 丸めず、エラーとして返す。KEDA は 0 を「起こさなくてよい」と読むので、失敗を 0 にすると
// 滞留があるのに worker が起きなくなる。
func (adapter *Adapter) CountDeliverable(ctx context.Context, physicalQueue string) (int64, error) {
	client := adapter.clientSnapshot()
	if client == nil {
		return 0, errors.New("riveradapter: client is nil")
	}
	now := adapter.now()
	params := river.NewJobListParams().
		Queues(physicalQueue).
		States(deliverableStates...).
		First(deliverableCountPageSize)

	var count int64
	for {
		result, err := client.JobList(ctx, params)
		if err != nil {
			return 0, fmt.Errorf("riveradapter: listing River jobs for %q: %w", physicalQueue, err)
		}
		for _, job := range result.Jobs {
			if job.State == rivertype.JobStateAvailable || !job.ScheduledAt.After(now) {
				count++
			}
		}
		if len(result.Jobs) < deliverableCountPageSize {
			return count, nil
		}
		if result.LastCursor == nil {
			return 0, fmt.Errorf("riveradapter: listing River jobs for %q: full page has no cursor", physicalQueue)
		}
		params = params.After(result.LastCursor)
	}
}
