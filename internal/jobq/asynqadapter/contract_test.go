package asynqadapter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/contracttest"
)

// TestContract は process death ケースで孤児の回収を実時間で待つ。並列で重ねてよい根拠は testQueue の doc comment。
func TestContract(t *testing.T) {
	t.Parallel()
	options := redisOptions(t)
	contracttest.Run(t, func(t *testing.T, registry *jobq.Registry, worker jobq.Worker[contracttest.Args], name string) contracttest.Fixture {
		jobq.Register(registry, worker)
		return newRedisFixture(t, options, registry, name)
	})
}

func TestEnqueuePreservesLongWorkTimeout(t *testing.T) {
	fixture := newRedisFixture(t, redisOptions(t), jobq.NewRegistry(), "default")
	enqueuedAt := time.Now()
	enqueueTestJob(t, fixture.Adapter, contracttest.Args{TimeoutMS: 43_200_000})

	tasks, err := fixture.inspector.ListPendingTasks(fixture.queue)
	if err != nil {
		t.Fatalf("ListPendingTasks() error = %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("pending tasks = %d, want 1", len(tasks))
	}
	task := tasks[0]
	if task.Timeout < 12*time.Hour {
		t.Errorf("asynq timeout = %s, want at least 12h", task.Timeout)
	}
	if !task.Deadline.IsZero() && task.Deadline.Before(enqueuedAt.Add(12*time.Hour)) {
		t.Errorf("asynq deadline = %s, want at least 12h after enqueue or unset", task.Deadline)
	}
}

// TestCrashExhaustsOnlyAttempt は MaxAttempts 1 の Work がプロセス死で最後の試行を使い切り、
// 回収後に再配送されないことを確かめる。並列で重ねてよい根拠は testQueue の doc comment。
// 回収を待つのは、使い切った後の状態が回収後にしか現れず、lease 切れの時刻は asynq の公開設定ではないため。
func TestCrashExhaustsOnlyAttempt(t *testing.T) {
	t.Parallel()
	options := redisOptions(t)
	var calls atomic.Int32
	started := make(chan struct{})
	registry := jobq.NewRegistry()
	jobq.Register(registry, testWorker(func(ctx context.Context, _ contracttest.Args) error {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}))
	fixture := newRedisFixture(t, options, registry, "default")
	startTestAdapter(t, fixture.Adapter)
	enqueueTestJob(t, fixture.Adapter, contracttest.Args{TimeoutMS: 5000, MaxAttempts: 1})
	await(t, started, 4*time.Second)

	crashCtx, cancelCrash := context.WithTimeout(t.Context(), time.Second)
	err := fixture.Crash(crashCtx)
	cancelCrash()
	if err != nil {
		t.Fatalf("Crash() error = %v", err)
	}
	startTestAdapter(t, fixture.Adapter)
	recoveryCtx, cancelRecovery := context.WithTimeout(t.Context(), 5*time.Second+fixture.RecoveryGrace()+10*time.Second)
	defer cancelRecovery()
	if err := fixture.WaitIdle(recoveryCtx); err != nil {
		t.Fatalf("WaitIdle() after crash: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Work calls after recovery = %d, want 1", got)
	}
	info, err := fixture.inspector.GetQueueInfo(fixture.queue)
	if err != nil {
		t.Fatalf("GetQueueInfo() error = %v", err)
	}
	if info.Size != 0 || info.Completed != 0 {
		t.Errorf("queue after recovery: size=%d completed=%d, want empty queue", info.Size, info.Completed)
	}
}

type redisFixture struct {
	*Adapter
	t         *testing.T
	queue     string
	inspector *asynq.Inspector
	options   *redis.Options
	commands  *crashCommandHook
	registry  *jobq.Registry
	config    Config
	queueFor  QueueFunc
	crashedAt time.Time
}

func newRedisFixture(t *testing.T, options *redis.Options, registry *jobq.Registry, name string) *redisFixture {
	t.Helper()
	queue := testQueue(name)
	fixture := &redisFixture{
		t: t, queue: queue, inspector: testInspector(t, options, queue), options: options,
		registry: registry,
		config: Config{Spec: jobq.WorkerSpec{Queues: []jobq.QueueSpec{{
			Logical: name, Physical: []string{queue}, MaxWorkers: 1,
		}}}},
		queueFor: func(jobq.Args) (string, error) { return queue, nil },
	}
	fixture.connect()
	return fixture
}

func (fixture *redisFixture) connect() {
	fixture.t.Helper()
	connection := redis.NewClient(fixture.options)
	commandHook := &crashCommandHook{}
	connection.AddHook(commandHook)
	adapter := testAdapter(fixture.t, connection, fixture.registry, fixture.queueFor, fixture.config)
	fixture.Adapter = adapter
	fixture.commands = commandHook
	fixture.t.Cleanup(func() {
		if err := adapter.Stop(context.Background()); err != nil {
			fixture.t.Errorf("stopping asynq before closing Redis: %v", err)
			return
		}
		closeRedis(fixture.t, connection)
	})
}

var errCrashedRedis = errors.New("blocked Redis commands from crashed worker")

type crashCommandHook struct {
	blocked atomic.Bool
}

// DialHook は PubSub が使う接続を保つため、接続処理をそのまま通す。
func (*crashCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }

// ProcessHook はクラッシュ後の Redis コマンドを送信前に失敗させる。
func (hook *crashCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if hook.blocked.Load() {
			return errCrashedRedis
		}
		return next(ctx, cmd)
	}
}

// ProcessPipelineHook はクラッシュ後のパイプライン内の全コマンドを送信前に失敗させる。
func (hook *crashCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if hook.blocked.Load() {
			for _, cmd := range cmds {
				cmd.SetErr(errCrashedRedis)
			}
			return errCrashedRedis
		}
		return next(ctx, cmds)
	}
}

func TestCrashCommandHookBlocksRedisCommands(t *testing.T) {
	var dialed atomic.Bool
	connection := redis.NewClient(&redis.Options{
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			dialed.Store(true)
			return nil, errors.New("unexpected Redis dial")
		},
	})
	t.Cleanup(func() { closeRedis(t, connection) })
	hook := &crashCommandHook{}
	connection.AddHook(hook)
	hook.blocked.Store(true)
	ctx := t.Context()
	if err := connection.Ping(ctx).Err(); !errors.Is(err, errCrashedRedis) {
		t.Errorf("Ping() error = %v, want crash error", err)
	}
	cmds, err := connection.Pipelined(ctx, func(pipeline redis.Pipeliner) error {
		pipeline.Ping(ctx)
		return nil
	})
	if !errors.Is(err, errCrashedRedis) || len(cmds) != 1 || !errors.Is(cmds[0].Err(), errCrashedRedis) {
		t.Errorf("Pipelined() returned %v, %v, want one command with crash error", cmds, err)
	}
	if dialed.Load() {
		t.Error("blocked commands attempted to dial Redis")
	}
}

// WaitIdle は配送前・実行中・Snooze やエラー後の待機中のタスクが無くなるまで待つ。
func (fixture *redisFixture) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := fixture.inspector.GetQueueInfo(fixture.queue)
		if err != nil {
			return fmt.Errorf("inspecting asynq contract queue: %w", err)
		}
		if info.Pending+info.Active+info.Scheduled+info.Retry == 0 {
			if !fixture.crashedAt.IsZero() {
				fixture.t.Logf("crash to completed redelivery: %s", time.Since(fixture.crashedAt))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for asynq contract queue (pending=%d active=%d scheduled=%d retry=%d): %w",
				info.Pending, info.Active, info.Scheduled, info.Retry, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Crash は worker 専用のクライアントのコマンドをフックで遮断してから Work を取り消す。
// Work を同じプロセスで観測する共通テストのまま、完了・再投入・lease 更新が
// Redis に届かない状態を作る。別接続で active を確認し、再接続後は実際の lease 回収を待つ。
// asynq v0.26.0 では Server の停止前にクライアントを閉じると、subscriber が
// 閉じた PubSub チャネルから nil を受信して panic しうる。クライアントは停止後の cleanup で閉じる。
func (fixture *redisFixture) Crash(ctx context.Context) error {
	fixture.crashedAt = time.Now()
	fixture.commands.blocked.Store(true)
	stopCtx, cancel := context.WithCancel(ctx)
	cancel()
	err := fixture.Stop(stopCtx)
	if err == nil {
		return errors.New("stopping disconnected worker: expected context cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		return fmt.Errorf("stopping disconnected worker: %w", err)
	}
	fixture.connect()
	info, err := fixture.inspector.GetQueueInfo(fixture.queue)
	if err != nil {
		return fmt.Errorf("inspecting disconnected worker's queue: %w", err)
	}
	if info.Active != 1 || info.Pending+info.Scheduled+info.Retry != 0 {
		return fmt.Errorf("crashed queue: got active=%d pending=%d scheduled=%d retry=%d, want one active task only",
			info.Active, info.Pending, info.Scheduled, info.Retry)
	}
	return ctx.Err()
}

// RecoveryGrace は共通契約テストに adapter の回収猶予を渡す。
func (*redisFixture) RecoveryGrace() time.Duration { return RecoveryGrace }

func (fixture *redisFixture) SnoozeClockSkew(ctx context.Context) (time.Duration, error) {
	connection := redis.NewClient(fixture.options)
	defer closeRedis(fixture.t, connection)

	// serverNow has no monotonic reading, so Sub compares wall-clock values. Sampling
	// localBefore before the command includes the outbound delay and command time,
	// deliberately overestimating Redis clock's lead and widening the test allowance.
	// This measures Redis time without assuming which clock Asynq uses for snooze eligibility.
	localBefore := time.Now()
	serverNow, err := connection.Time(ctx).Result()
	if err != nil {
		return 0, fmt.Errorf("reading Redis clock for snooze contract: %w", err)
	}
	if skew := serverNow.Sub(localBefore); skew > 0 {
		return skew, nil
	}
	return 0, nil
}

func redisOptions(t *testing.T) *redis.Options {
	t.Helper()
	url := os.Getenv("ROKUBAN_TEST_REDIS_URL")
	if url == "" {
		t.Fatal("ROKUBAN_TEST_REDIS_URL is required for asynq integration tests")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal("invalid ROKUBAN_TEST_REDIS_URL")
	}
	return options
}

// testQueue はテストごとに分かれた物理キュー名を返す。UUID の接頭辞で、同じ Redis DB を
// 共有するテスト同士のキューが衝突しない。回収を待つテストを t.Parallel で重ねてよい根拠は、
// 次の asynq v0.26.0 の公開文書による。
//   - TaskInfo.IsOrphaned の godoc: 孤児は「そのタスクのキューに対して server を走らせる」ことで回収される。
//     よって、あるテストの server は自分のキューの孤児だけを回収し、別テストのキューには触れない。
//   - Config.Queues の godoc: server が処理するのは列挙したキューだけで、nil なら "default" だけ。
//     各テストの server は自分のキュー 1 本だけを列挙するので、別テストのキューを処理しない。
//   - asynq.Unique の godoc: タスクの一意性は Task Type・Task Payload・Queue Name に基づく。
//     同じ payload のタスクが別テストのキューで合流することはない。
func testQueue(name string) string {
	prefix := "test-" + uuid.NewString() + "-"
	return prefix + name[:min(len(name), jobq.MaxQueueNameLen-len(prefix))]
}

func testInspector(t *testing.T, options *redis.Options, queues ...string) *asynq.Inspector {
	t.Helper()
	connection := redis.NewClient(options)
	inspector := asynq.NewInspectorFromRedisClient(connection)
	t.Cleanup(func() {
		for _, queue := range queues {
			if err := inspector.DeleteQueue(queue, true); err != nil && !errors.Is(err, asynq.ErrQueueNotFound) {
				t.Errorf("deleting test queue %q: %v", queue, err)
			}
		}
		closeRedis(t, connection)
	})
	return inspector
}

func closeRedis(t *testing.T, connection *redis.Client) {
	t.Helper()
	if err := connection.Close(); err != nil && !errors.Is(err, redis.ErrClosed) {
		t.Errorf("closing test Redis client: %v", err)
	}
}
