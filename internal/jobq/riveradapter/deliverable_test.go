package riveradapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobq"
	riveradaptertest "github.com/fetburner/rokuban/internal/jobq/riveradapter/testutil"
	"github.com/fetburner/rokuban/internal/testutil"
)

const deliverableTestQueue = "count_q"

// countArgs は件数テスト用の jobq Args。Fail が真なら Work は失敗し、Block が真なら release まで戻らない。
type countArgs struct {
	Value string `json:"value"`
	Queue string `json:"queue"`
	Fail  bool   `json:"fail"`
	Block bool   `json:"block"`
}

func (countArgs) Kind() string { return "deliverable_count" }

func (args countArgs) Spec() jobq.Spec {
	return jobq.Spec{
		Queue:       args.Queue,
		Timeout:     func(jobq.Args) time.Duration { return time.Minute },
		MaxAttempts: 3,
	}
}

// riverCountArgs は River の公開 Insert で ScheduledAt を指定して投入するための引数。
// jobq を通さないのは、jobq の Enqueue が遅延投入を表せないため。
type riverCountArgs struct{}

func (riverCountArgs) Kind() string { return "deliverable_count_river" }

// countWorker は Fail / Block の指示に従う jobq.Worker。started は Block の Work が始まったことを知らせる。
type countWorker struct {
	started chan struct{}
	release chan struct{}
}

func (w countWorker) Work(ctx context.Context, args countArgs) error {
	if args.Fail {
		return errors.New("deliverable count test: planned failure")
	}
	if args.Block {
		select {
		case w.started <- struct{}{}:
		default:
		}
		select {
		case <-w.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// newCountEnqueuer は投入側だけの adapter を作る。件数は公開 JobList で数えるので、
// 投入側の adapter で足りる。時計は now に差し替える。
func newCountEnqueuer(t *testing.T, pool *pgxpool.Pool, now time.Time) *Adapter {
	t.Helper()
	adapter, err := NewInsertOnlyEnqueuer(pool, logicalQueueFor)
	if err != nil {
		t.Fatalf("NewInsertOnlyEnqueuer() error = %v", err)
	}
	adapter.now = func() time.Time { return now }
	return adapter
}

// newCountWorkerAdapter は deliverableTestQueue（または queue）を実際に購読する adapter を起動する。
// 止めるのは t.Cleanup に任せ、呼び出し側は Stop のタイミングを選べるように Adapter を返す。
func newCountWorkerAdapter(t *testing.T, pool *pgxpool.Pool, queue string, worker countWorker) *Adapter {
	t.Helper()
	registry := jobq.NewRegistry()
	Register(registry, worker)
	adapter, err := NewAdapter(pool, registry, logicalQueueFor, Config{
		Spec:                 contractSpec(queue),
		JobTimeout:           time.Millisecond,
		RescueStuckJobsAfter: riverContractRescueAfter,
		TestOnly:             true,
	})
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	startAdapter(t, adapter)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = adapter.Stop(ctx)
	})
	return adapter
}

func requireDeliverable(t *testing.T, adapter *Adapter, queue string, want int64) {
	t.Helper()
	got, err := adapter.CountDeliverable(t.Context(), queue)
	if err != nil {
		t.Fatalf("CountDeliverable(%q) error = %v", queue, err)
	}
	if got != want {
		t.Errorf("CountDeliverable(%q) = %d, want %d", queue, got, want)
	}
}

func TestCountDeliverableCountsAvailableJobs(t *testing.T) {
	pool := testutil.SetupDB(t)
	adapter := newCountEnqueuer(t, pool, time.Now())
	for _, value := range []string{"a", "b", "c"} {
		if _, err := adapter.Enqueue(t.Context(), countArgs{Value: value, Queue: deliverableTestQueue}); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}

	requireDeliverable(t, adapter, deliverableTestQueue, 3)
	requireDeliverable(t, adapter, "never_enqueued", 0)
}

func TestCountDeliverableExcludesRunningJobs(t *testing.T) {
	pool := testutil.SetupDB(t)
	worker := countWorker{started: make(chan struct{}, 1), release: make(chan struct{})}
	running := newCountWorkerAdapter(t, pool, deliverableTestQueue, worker)
	if _, err := running.Enqueue(t.Context(), countArgs{Value: "run", Queue: deliverableTestQueue, Block: true}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	select {
	case <-worker.started:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not start the blocking job")
	}
	t.Cleanup(func() { close(worker.release) })

	requireDeliverable(t, running, deliverableTestQueue, 0)
}

func TestCountDeliverableScheduledOnlyAfterDue(t *testing.T) {
	pool := testutil.SetupDB(t)
	dueAt := time.Now().Add(time.Hour)
	riverClient := riveradaptertest.NewRiverClient(t, pool)
	if _, err := riverClient.Insert(t.Context(), riverCountArgs{}, &river.InsertOpts{Queue: deliverableTestQueue, ScheduledAt: dueAt}); err != nil {
		t.Fatalf("River Insert() error = %v", err)
	}

	requireDeliverable(t, newCountEnqueuer(t, pool, dueAt.Add(-time.Minute)), deliverableTestQueue, 0)
	requireDeliverable(t, newCountEnqueuer(t, pool, dueAt.Add(time.Minute)), deliverableTestQueue, 1)
}

func TestCountDeliverableRetryableOnlyAfterDue(t *testing.T) {
	pool := testutil.SetupDB(t)
	worker := countWorker{started: make(chan struct{}, 1), release: make(chan struct{})}
	failing := newCountWorkerAdapter(t, pool, deliverableTestQueue, worker)
	if _, err := failing.Enqueue(t.Context(), countArgs{Value: "fail", Queue: deliverableTestQueue, Fail: true}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// retryable は River が失敗の後に決めた scheduled_at を持つ。その時刻を公開 JobList で読み、
	// 時計をその前後に置く。
	client := riveradaptertest.NewRiverClient(t, pool)
	retryable := waitForRiverState(t, client, deliverableTestQueue, rivertype.JobStateRetryable)
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := failing.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	dueAt := retryable.ScheduledAt

	requireDeliverable(t, newCountEnqueuer(t, pool, dueAt.Add(-time.Minute)), deliverableTestQueue, 0)
	requireDeliverable(t, newCountEnqueuer(t, pool, dueAt.Add(time.Minute)), deliverableTestQueue, 1)
}

func TestCountDeliverableReturnsErrorWhenDatabaseIsUnreachable(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://127.0.0.1:1/unreachable?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	t.Cleanup(pool.Close)
	adapter := newCountEnqueuer(t, pool, time.Now())

	got, err := adapter.CountDeliverable(t.Context(), deliverableTestQueue)
	if err == nil {
		t.Fatalf("CountDeliverable() = %d, nil error; want an error when the database is unreachable", got)
	}
}

// waitForRiverState は state の job が queue にできるまで公開 JobList を読む。
func waitForRiverState(t *testing.T, client *river.Client[pgx.Tx], queue string, state rivertype.JobState) *rivertype.JobRow {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rows := riveradaptertest.MustListRiverJobs(t, t.Context(), client, river.NewJobListParams().Queues(queue).States(state))
		if len(rows) > 0 {
			return rows[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s job on queue %q within 15s", state, queue)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
