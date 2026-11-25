package asynqadapter

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/contracttest"
)

func TestCountDeliverablePendingTasks(t *testing.T) {
	fixture := newRedisFixture(t, redisOptions(t), jobq.NewRegistry(), "pending-count")
	for i := range 4 {
		enqueueTestJob(t, fixture.Adapter, contracttest.Args{Value: fmt.Sprintf("pending-%d", i), TimeoutMS: 2000})
	}

	got, err := fixture.CountDeliverable(t.Context(), fixture.queue)
	if err != nil {
		t.Fatalf("CountDeliverable() error = %v", err)
	}
	if got != 4 {
		t.Errorf("deliverable count = %d, want 4", got)
	}
}

func TestCountDeliverableDoesNotCountLeasedActiveTask(t *testing.T) {
	started := make(chan struct{})
	registry := jobq.NewRegistry()
	jobq.Register(registry, testWorker(func(ctx context.Context, _ contracttest.Args) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}))
	fixture := newRedisFixture(t, redisOptions(t), registry, "active-count")
	startTestAdapter(t, fixture.Adapter)
	enqueueTestJob(t, fixture.Adapter, contracttest.Args{TimeoutMS: 10_000})
	await(t, started, 4*time.Second)

	got, err := fixture.CountDeliverable(t.Context(), fixture.queue)
	if err != nil {
		t.Fatalf("CountDeliverable() error = %v", err)
	}
	if got != 0 {
		t.Errorf("deliverable count with a leased active task = %d, want 0", got)
	}
}

func TestCountDeliverableSnoozedTaskOnlyAfterDue(t *testing.T) {
	registry := jobq.NewRegistry()
	jobq.Register(registry, testWorker(func(context.Context, contracttest.Args) error {
		return jobq.Snooze(time.Hour)
	}))
	fixture := newRedisFixture(t, redisOptions(t), registry, "snooze-count")
	startTestAdapter(t, fixture.Adapter)
	enqueueTestJob(t, fixture.Adapter, contracttest.Args{TimeoutMS: 10_000})

	var retry *asynq.TaskInfo
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tasks, err := fixture.inspector.ListRetryTasks(fixture.queue)
		if err == nil && len(tasks) == 1 {
			retry = tasks[0]
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if retry == nil {
		t.Fatal("Snooze did not create a retry task")
	}

	fixture.now = func() time.Time { return retry.NextProcessAt.Add(-time.Second) }
	got, err := fixture.CountDeliverable(t.Context(), fixture.queue)
	if err != nil {
		t.Fatalf("CountDeliverable() before due time error = %v", err)
	}
	if got != 0 {
		t.Errorf("deliverable count before retry time = %d, want 0", got)
	}

	fixture.now = func() time.Time { return retry.NextProcessAt.Add(time.Second) }
	got, err = fixture.CountDeliverable(t.Context(), fixture.queue)
	if err != nil {
		t.Fatalf("CountDeliverable() after due time error = %v", err)
	}
	if got != 1 {
		t.Errorf("deliverable count after retry time = %d, want 1", got)
	}
}

func TestCountDeliverableScheduledTaskOnlyAfterDue(t *testing.T) {
	fixture := newRedisFixture(t, redisOptions(t), jobq.NewRegistry(), "scheduled-count")
	connection := redis.NewClient(fixture.options)
	client := asynq.NewClientFromRedisClient(connection)
	t.Cleanup(func() { closeRedis(t, connection) })
	dueAt := time.Now().Add(48 * time.Hour)
	task := asynq.NewTask("test:scheduled", nil)
	if _, err := client.Enqueue(task, asynq.Queue(fixture.queue), asynq.ProcessAt(dueAt)); err != nil {
		t.Fatalf("Enqueue() scheduled task: %v", err)
	}

	fixture.now = func() time.Time { return dueAt.Add(-time.Second) }
	got, err := fixture.CountDeliverable(t.Context(), fixture.queue)
	if err != nil {
		t.Fatalf("CountDeliverable() before due time error = %v", err)
	}
	if got != 0 {
		t.Errorf("deliverable count before scheduled time = %d, want 0", got)
	}

	fixture.now = func() time.Time { return dueAt.Add(time.Second) }
	got, err = fixture.CountDeliverable(t.Context(), fixture.queue)
	if err != nil {
		t.Fatalf("CountDeliverable() after due time error = %v", err)
	}
	if got != 1 {
		t.Errorf("deliverable count after scheduled time = %d, want 1", got)
	}
}

// TestCountDeliverableCountsOrphanedActiveTask は孤児の active を CountDeliverable が 1 件と数えることを確かめる。
// 孤児になるまで待つので t.Parallel で他の回収待ちと重ねる。根拠は TestCrashExhaustsOnlyAttempt の doc comment と同じ。
// 回収後の片付け（キュー削除は active があると失敗する）のため、回収の完了まで待つ。
func TestCountDeliverableCountsOrphanedActiveTask(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	var calls atomic.Int32
	registry := jobq.NewRegistry()
	jobq.Register(registry, testWorker(func(ctx context.Context, _ contracttest.Args) error {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}))
	fixture := newRedisFixture(t, redisOptions(t), registry, "orphan-count")
	startTestAdapter(t, fixture.Adapter)
	enqueueTestJob(t, fixture.Adapter, contracttest.Args{TimeoutMS: 10_000})
	await(t, started, 4*time.Second)

	crashCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := fixture.Crash(crashCtx); err != nil {
		t.Fatalf("Crash() error = %v", err)
	}

	deadline := time.Now().Add(RecoveryGrace)
	for time.Now().Before(deadline) {
		tasks, err := fixture.inspector.ListActiveTasks(fixture.queue)
		if err == nil && len(tasks) == 1 && tasks[0].IsOrphaned {
			got, countErr := fixture.CountDeliverable(t.Context(), fixture.queue)
			startTestAdapter(t, fixture.Adapter)
			recoveryCtx, cancelRecovery := context.WithTimeout(t.Context(), RecoveryGrace+10*time.Second)
			defer cancelRecovery()
			if err := fixture.WaitIdle(recoveryCtx); err != nil {
				t.Fatalf("WaitIdle() after orphan count: %v", err)
			}
			if countErr != nil {
				t.Fatalf("CountDeliverable() error = %v", countErr)
			}
			if got != 1 {
				t.Errorf("deliverable count with an orphaned active task = %d, want 1", got)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("active task did not become orphaned before the deadline")
}

func TestCountDeliverablePaginatesPastDefaultPage(t *testing.T) {
	fixture := newRedisFixture(t, redisOptions(t), jobq.NewRegistry(), "page-count")
	connection := redis.NewClient(fixture.options)
	client := asynq.NewClientFromRedisClient(connection)
	t.Cleanup(func() { closeRedis(t, connection) })
	dueAt := time.Now().Add(48 * time.Hour)
	for i := range 131 {
		task := asynq.NewTask("test:scheduled", nil)
		if _, err := client.Enqueue(task, asynq.Queue(fixture.queue), asynq.ProcessAt(dueAt)); err != nil {
			t.Fatalf("Enqueue() scheduled task %d: %v", i, err)
		}
	}
	fixture.now = func() time.Time { return dueAt.Add(time.Second) }

	got, err := fixture.CountDeliverable(t.Context(), fixture.queue)
	if err != nil {
		t.Fatalf("CountDeliverable() error = %v", err)
	}
	if got != 131 {
		t.Errorf("deliverable count = %d, want 131", got)
	}
}

func TestCountDeliverableNeverCreatedQueueIsZero(t *testing.T) {
	fixture := newRedisFixture(t, redisOptions(t), jobq.NewRegistry(), "known")
	missingQueue := testQueue("never-created")
	got, err := fixture.CountDeliverable(t.Context(), missingQueue)
	if err != nil {
		t.Fatalf("CountDeliverable() error = %v", err)
	}
	if got != 0 {
		t.Errorf("deliverable count for a never-created queue = %d, want 0", got)
	}
}

func TestCountDeliverableReturnsErrorWhenRedisIsUnavailable(t *testing.T) {
	connection := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 25 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { closeRedis(t, connection) })
	registry := jobq.NewRegistry()
	adapter := testAdapter(t, connection, registry, func(jobq.Args) (string, error) { return "unavailable", nil }, Config{
		Spec: jobq.WorkerSpec{Queues: []jobq.QueueSpec{{Logical: "unavailable", Physical: []string{"unavailable"}, MaxWorkers: 1}}},
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := adapter.CountDeliverable(ctx, "unavailable"); err == nil {
		t.Fatal("CountDeliverable() error = nil, want Redis connection error")
	}
}

func TestCountDeliverableHonorsCanceledContext(t *testing.T) {
	fixture := newRedisFixture(t, redisOptions(t), jobq.NewRegistry(), "cancel-count")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fixture.CountDeliverable(ctx, fixture.queue); !errors.Is(err, context.Canceled) {
		t.Errorf("CountDeliverable() error = %v, want context.Canceled", err)
	}
}
