package riveradapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// QueuedJobSelector は、キューと（任意で）ジョブ種別で絞った取り消し対象。
// Kinds が空ならキューの全ジョブが対象になる。
type QueuedJobSelector struct {
	Queue string
	Kinds []string
}

// cancelableJobStates は CancelQueued が取り消す状態。running は含めない。
// River の JobCancel は実行中のジョブを running のまま残し、worker が返るまで状態を変えないため、
// 含めると同じジョブが一覧に戻り続ける。
var cancelableJobStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateScheduled,
}

// CancelQueued は、selectors に合う未完了のジョブを取り消し、取り消した件数と、
// 実行中のため取り消せずに残ったジョブの件数を返す。
// River の公開 API（JobList と JobCancel）だけを使い、river_* 表には触れない。
// 完了済みのジョブは対象にしない。取り消し済みのジョブは再度数えない。
// 実行中のジョブは worker が返るまで running のまま残るので、running が 0 になるまで再実行する。
func (adapter *Adapter) CancelQueued(ctx context.Context, selectors []QueuedJobSelector) (cancelled, running int, err error) {
	client := adapter.clientSnapshot()
	if client == nil {
		return 0, 0, errors.New("riveradapter: client is not available")
	}
	for _, selector := range selectors {
		params := river.NewJobListParams().Queues(selector.Queue).States(cancelableJobStates...)
		runningParams := river.NewJobListParams().Queues(selector.Queue).States(rivertype.JobStateRunning)
		if len(selector.Kinds) > 0 {
			params = params.Kinds(selector.Kinds...)
			runningParams = runningParams.Kinds(selector.Kinds...)
		}
		err := forEachJob(ctx, client, params, func(job *rivertype.JobRow) error {
			row, err := client.JobCancel(ctx, job.ID)
			if err != nil {
				return fmt.Errorf("cancelling job %d (%s): %w", job.ID, job.Kind, err)
			}
			if row.State == rivertype.JobStateCancelled {
				cancelled++
			}
			return nil
		})
		if err != nil {
			return cancelled, running, fmt.Errorf("cancelling queued jobs in queue %q: %w", selector.Queue, err)
		}
		err = forEachJob(ctx, client, runningParams, func(*rivertype.JobRow) error {
			running++
			return nil
		})
		if err != nil {
			return cancelled, running, fmt.Errorf("counting running jobs in queue %q: %w", selector.Queue, err)
		}
	}
	return cancelled, running, nil
}

// forEachJob は params に合うジョブを、カーソルで次のページへ進みながら 1 件ずつ fn に渡す。
// 次のページは前のページの最後のカーソルの後ろから取るので、fn が状態を変えても同じジョブを二度渡さない。
func forEachJob(ctx context.Context, client *river.Client[pgx.Tx], params *river.JobListParams, fn func(*rivertype.JobRow) error) error {
	var cursor *river.JobListCursor
	for {
		page := params.First(1000)
		if cursor != nil {
			page = page.After(cursor)
		}
		result, err := client.JobList(ctx, page)
		if err != nil {
			return fmt.Errorf("listing jobs: %w", err)
		}
		for _, job := range result.Jobs {
			if err := fn(job); err != nil {
				return err
			}
		}
		if len(result.Jobs) == 0 || result.LastCursor == nil {
			return nil
		}
		cursor = result.LastCursor
	}
}
