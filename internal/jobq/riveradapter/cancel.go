package riveradapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/riverqueue/river"
)

// QueuedJobSelector は、キューと（任意で）ジョブ種別で絞った取り消し対象。
// Kinds が空ならキューの全ジョブが対象になる。
type QueuedJobSelector struct {
	Queue string
	Kinds []string
}

// CancelQueued は、selectors に合う未完了のジョブをすべて取り消し、取り消した件数を返す。
// River の公開 API（JobList と JobCancel）だけを使い、river_* 表には触れない。
// 完了済みのジョブは対象にしない。取り消し済みのジョブは再度数えない。
func (adapter *Adapter) CancelQueued(ctx context.Context, selectors []QueuedJobSelector) (int, error) {
	client := adapter.clientSnapshot()
	if client == nil {
		return 0, errors.New("riveradapter: client is not available")
	}
	cancelled := 0
	for _, selector := range selectors {
		for {
			params := river.NewJobListParams().Queues(selector.Queue).States(pendingJobStates...).First(1000)
			if len(selector.Kinds) > 0 {
				params = params.Kinds(selector.Kinds...)
			}
			result, err := client.JobList(ctx, params)
			if err != nil {
				return cancelled, fmt.Errorf("listing queued jobs in queue %q: %w", selector.Queue, err)
			}
			if len(result.Jobs) == 0 {
				break
			}
			for _, job := range result.Jobs {
				if _, err := client.JobCancel(ctx, job.ID); err != nil {
					return cancelled, fmt.Errorf("cancelling job %d (%s) in queue %q: %w", job.ID, job.Kind, selector.Queue, err)
				}
				cancelled++
			}
		}
	}
	return cancelled, nil
}
