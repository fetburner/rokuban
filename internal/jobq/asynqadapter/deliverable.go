package asynqadapter

import (
	"context"
	"fmt"
	"slices"

	"github.com/hibiken/asynq"
)

const deliverablePageSize = 100

// CountDeliverable は物理キューの待機中、期限到来済みの scheduled/retry、および孤児の active を数える。
// 未作成キューは公開 Inspector.Queues の一覧に存在しないため 0 件とし、
// GetQueueInfo の非公開エラー形には依存しない。期限到来タスクを待機列へ戻す処理と孤児の回収は、
// KEDA が起こした Job の Server が行う。この操作は件数を数えるだけで、タスクを戻さない。
func (adapter *Adapter) CountDeliverable(ctx context.Context, physicalQueue string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	queues, err := adapter.inspector.Queues()
	if err != nil {
		return 0, fmt.Errorf("listing asynq queues: %w", err)
	}
	if !slices.Contains(queues, physicalQueue) {
		return 0, nil
	}
	info, err := adapter.inspector.GetQueueInfo(physicalQueue)
	if err != nil {
		return 0, fmt.Errorf("getting asynq queue info for %q: %w", physicalQueue, err)
	}

	count := int64(info.Pending)
	now := adapter.now()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	scheduled, err := listAllTasks(physicalQueue, adapter.inspector.ListScheduledTasks)
	if err != nil {
		return 0, fmt.Errorf("listing asynq scheduled tasks for %q: %w", physicalQueue, err)
	}
	for _, task := range scheduled {
		if !task.NextProcessAt.After(now) {
			count++
		}
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	retries, err := listAllTasks(physicalQueue, adapter.inspector.ListRetryTasks)
	if err != nil {
		return 0, fmt.Errorf("listing asynq retry tasks for %q: %w", physicalQueue, err)
	}
	for _, task := range retries {
		if !task.NextProcessAt.After(now) {
			count++
		}
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	active, err := listAllTasks(physicalQueue, adapter.inspector.ListActiveTasks)
	if err != nil {
		return 0, fmt.Errorf("listing asynq active tasks for %q: %w", physicalQueue, err)
	}
	for _, task := range active {
		if task.IsOrphaned {
			count++
		}
	}
	return count, nil
}

func listAllTasks(queue string, list func(string, ...asynq.ListOption) ([]*asynq.TaskInfo, error)) ([]*asynq.TaskInfo, error) {
	var all []*asynq.TaskInfo
	for page := 1; ; page++ {
		tasks, err := list(queue, asynq.Page(page), asynq.PageSize(deliverablePageSize))
		if err != nil {
			return nil, err
		}
		all = append(all, tasks...)
		if len(tasks) < deliverablePageSize {
			return all, nil
		}
	}
}
