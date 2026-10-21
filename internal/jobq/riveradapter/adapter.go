// Package riveradapter implements the jobq contract with River and PostgreSQL.
package riveradapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobq"
)

const adapterWorkerKey = "river"

// pendingJobStates limits coalescing to jobs that have not reached a terminal state.
// Keeping River's state types here leaves the jobq and jobs contracts adapter-independent.
var pendingJobStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// Adapter implements jobq.Enqueuer and jobq.Runner with one River client.
type Adapter struct {
	// lifecycleMu serializes Start and Stop without blocking client snapshots from Work.
	lifecycleMu sync.Mutex
	// mu protects client and stopped (Stop was called, so the next Start needs a new
	// client); never hold it while calling River.
	mu       sync.Mutex
	pool     *pgxpool.Pool
	registry *jobq.Registry
	queueFor QueueFunc
	config   Config
	client   *river.Client[pgx.Tx]
	stopped  bool
	// started records that client has been Started. River's Stopped() channel is nil
	// until Start, so only a client that was started needs rebuilding after Stop.
	started bool
}

// NewInsertOnlyEnqueuer creates an Adapter with an empty worker registry and does not start
// delivery. With no registered workers River refuses to Start, so the adapter cannot deliver;
// TestNewInsertOnlyEnqueuerDoesNotStartWorkers fixes that Start fails.
//
// queueFor は投入先の物理キューを決める。nil は拒む（名前をそのまま使う扱いに倒すと、
// 投入側が論理名へ入れ、worker が site 付きの物理名を引く「黙って動かない」になる）。
func NewInsertOnlyEnqueuer(pool *pgxpool.Pool, queueFor QueueFunc) (*Adapter, error) {
	if queueFor == nil {
		return nil, errors.New("riveradapter: queueFor is required")
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("creating insert-only River client: %w", err)
	}
	return &Adapter{pool: pool, registry: jobq.NewRegistry(), queueFor: queueFor, client: client}, nil
}

// NewAdapter builds River workers from registry and creates a PostgreSQL-backed adapter.
// It rejects an invalid cfg.Spec, a periodic job kind the registry cannot resolve, and a
// registered kind whose queueFor fails (for example a site-bound Args without a site).
func NewAdapter(pool *pgxpool.Pool, registry *jobq.Registry, queueFor QueueFunc, cfg Config) (*Adapter, error) {
	if registry == nil {
		return nil, errors.New("riveradapter: registry is required")
	}
	if queueFor == nil {
		return nil, errors.New("riveradapter: queueFor is required")
	}
	client, err := newRegistryClient(pool, registry, queueFor, cfg)
	if err != nil {
		return nil, err
	}
	return &Adapter{pool: pool, registry: registry, queueFor: queueFor, config: cfg, client: client}, nil
}

// workerTarget は Register が預けた builder に渡す、River の登録先と投入先の解決関数。
type workerTarget struct {
	workers  *river.Workers
	queueFor QueueFunc
}

// Register adds a jobq worker to registry and records how River should register it.
func Register[T jobq.Args](registry *jobq.Registry, worker jobq.Worker[T]) {
	args := zeroArgs[T]()
	kind := args.Kind()
	jobq.Register(registry, worker)
	err := registry.AddAdapterWorker(adapterWorkerKey, kind, func(dst any) error {
		target, ok := dst.(*workerTarget)
		if !ok || target == nil || target.workers == nil {
			return fmt.Errorf("expected *workerTarget, got %T", dst)
		}
		// 投入先が決まらない Args（site 単位のキューなのに site を持たない等）は、
		// NewAdapter の時点で、注入された queueFor によって拒む。
		if _, err := target.queueFor(args); err != nil {
			return err
		}
		river.AddWorker(target.workers, &riverWorker[T]{registry: registry})
		return nil
	})
	if err != nil {
		panic(fmt.Sprintf("riveradapter: registering %q: %v", kind, err))
	}
}

func buildRegistryConfig(registry *jobq.Registry, queueFor QueueFunc, cfg Config) (*river.Config, error) {
	if err := cfg.Spec.CheckKinds(registry); err != nil {
		return nil, fmt.Errorf("riveradapter: %w", err)
	}
	if err := cfg.Spec.CheckQueueFor(queueFor); err != nil {
		return nil, fmt.Errorf("riveradapter: %w", err)
	}
	workers := river.NewWorkers()
	if err := registry.RegisterAdapterWorkers(adapterWorkerKey, &workerTarget{workers: workers, queueFor: queueFor}); err != nil {
		return nil, fmt.Errorf("riveradapter: building workers: %w", err)
	}
	return buildConfig(workers, queueFor, cfg)
}

func newRegistryClient(pool *pgxpool.Pool, registry *jobq.Registry, queueFor QueueFunc, cfg Config) (*river.Client[pgx.Tx], error) {
	riverCfg, err := buildRegistryConfig(registry, queueFor, cfg)
	if err != nil {
		return nil, err
	}
	client, err := river.NewClient(riverpgxv5.New(pool), riverCfg)
	if err != nil {
		return nil, fmt.Errorf("riveradapter: creating River client: %w", err)
	}
	return client, nil
}

// Enqueue inserts args using the options derived from its jobq Spec.
func (adapter *Adapter) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	client := adapter.clientSnapshot()
	if client == nil {
		return jobq.Result{}, errors.New("riveradapter: client is nil")
	}
	insertOpts, err := makeInsertOpts(adapter.queueFor, args, opts...)
	if err != nil {
		return jobq.Result{}, err
	}
	result, err := client.Insert(ctx, riverEnvelope{args: args}, &insertOpts)
	if err != nil {
		return jobq.Result{}, fmt.Errorf("riveradapter: inserting %q: %w", args.Kind(), err)
	}
	return jobq.Result{Coalesced: result.UniqueSkippedAsDuplicate}, nil
}

// Start starts River delivery.
func (adapter *Adapter) Start(ctx context.Context) error {
	if adapter == nil {
		return errors.New("riveradapter: client is nil")
	}
	adapter.lifecycleMu.Lock()
	defer adapter.lifecycleMu.Unlock()

	adapter.mu.Lock()
	client := adapter.client
	stopped := adapter.stopped
	adapter.mu.Unlock()
	if client == nil {
		return errors.New("riveradapter: client is nil")
	}
	if stopped {
		select {
		case <-client.Stopped():
		case <-ctx.Done():
			return ctx.Err()
		}
		newClient, err := newRegistryClient(adapter.pool, adapter.registry, adapter.queueFor, adapter.config)
		if err != nil {
			return err
		}
		adapter.mu.Lock()
		adapter.client = newClient
		adapter.stopped = false
		adapter.started = false
		adapter.mu.Unlock()
		client = newClient
	}
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("riveradapter: starting River client: %w", err)
	}
	adapter.mu.Lock()
	adapter.started = true
	adapter.mu.Unlock()
	return nil
}

// Stop stops River delivery and waits for active work until ctx expires.
func (adapter *Adapter) Stop(ctx context.Context) error {
	if adapter == nil {
		return errors.New("riveradapter: client is nil")
	}
	adapter.lifecycleMu.Lock()
	defer adapter.lifecycleMu.Unlock()

	adapter.mu.Lock()
	client := adapter.client
	adapter.mu.Unlock()
	if client == nil {
		return errors.New("riveradapter: client is nil")
	}
	// River does not guarantee that a client can Start again after Stop, so the next Start
	// always builds a new client, whether or not this Stop succeeds.
	adapter.mu.Lock()
	adapter.stopped = adapter.started
	adapter.mu.Unlock()
	if err := client.Stop(ctx); err != nil {
		cancelErr := client.StopAndCancel(ctx)
		if cancelErr != nil && !errors.Is(cancelErr, err) && !errors.Is(cancelErr, context.DeadlineExceeded) {
			return errors.Join(err, fmt.Errorf("riveradapter: cancelling stopped work: %w", cancelErr))
		}
		return fmt.Errorf("riveradapter: stopping River client: %w", err)
	}
	return nil
}

func (adapter *Adapter) clientSnapshot() *river.Client[pgx.Tx] {
	if adapter == nil {
		return nil
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.client
}

type riverEnvelope struct {
	args jobq.Args
}

func (envelope riverEnvelope) Kind() string {
	return envelope.args.Kind()
}

func (envelope riverEnvelope) MarshalJSON() ([]byte, error) {
	return json.Marshal(envelope.args)
}

type riverArgs[T jobq.Args] struct {
	args T
}

func (riverArgs[T]) Kind() string {
	return zeroArgs[T]().Kind()
}

func (args *riverArgs[T]) UnmarshalJSON(data []byte) error {
	var decoded T
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if got, want := decoded.Kind(), zeroArgs[T]().Kind(); got != want {
		return fmt.Errorf("riveradapter: decoded kind %q, want %q", got, want)
	}
	args.args = decoded
	return nil
}

func (args riverArgs[T]) MarshalJSON() ([]byte, error) {
	return json.Marshal(args.args)
}

type riverWorker[T jobq.Args] struct {
	river.WorkerDefaults[riverArgs[T]]
	registry *jobq.Registry
}

func (worker *riverWorker[T]) Timeout(job *river.Job[riverArgs[T]]) time.Duration {
	return job.Args.args.Spec().Timeout(job.Args.args)
}

func (worker *riverWorker[T]) Work(ctx context.Context, job *river.Job[riverArgs[T]]) error {
	err := worker.registry.Work(ctx, job.Args.args)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	return mapWorkerError(err)
}

func mapWorkerError(err error) error {
	if err == nil {
		return nil
	}
	if delay, ok := jobq.SnoozeDelay(err); ok {
		return river.JobSnooze(delay)
	}
	if errors.Is(err, jobq.ErrDiscard) {
		return river.JobCancel(err)
	}
	return err
}

func zeroArgs[T jobq.Args]() T {
	var args T
	typeOf := reflect.TypeOf(args)
	if typeOf != nil && typeOf.Kind() == reflect.Pointer {
		args = reflect.New(typeOf.Elem()).Interface().(T)
	}
	return args
}

func makeInsertOpts(queueFor QueueFunc, args jobq.Args, options ...jobq.EnqueueOpt) (river.InsertOpts, error) {
	if args == nil {
		return river.InsertOpts{}, errors.New("riveradapter: args are nil")
	}
	value := reflect.ValueOf(args)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return river.InsertOpts{}, errors.New("riveradapter: args are nil")
	}
	kind := args.Kind()
	if kind == "" {
		return river.InsertOpts{}, errors.New("riveradapter: args kind is empty")
	}
	spec := args.Spec()
	if spec.Queue == "" {
		return river.InsertOpts{}, fmt.Errorf("riveradapter: %s queue is empty", kind)
	}
	if spec.Timeout == nil || spec.Timeout(args) <= 0 {
		return river.InsertOpts{}, fmt.Errorf("riveradapter: %s timeout must be positive", kind)
	}
	if spec.MaxAttempts < 0 {
		return river.InsertOpts{}, fmt.Errorf("riveradapter: %s max attempts must not be negative", kind)
	}
	physicalQueue, err := queueFor(args)
	if err != nil {
		return river.InsertOpts{}, err
	}
	insertOpts := river.InsertOpts{
		Queue:       physicalQueue,
		MaxAttempts: spec.MaxAttempts,
		Priority:    spec.Priority,
	}
	if spec.Coalesce {
		insertOpts.UniqueOpts = river.UniqueOpts{
			ByArgs:  true,
			ByQueue: true,
			ByState: append([]rivertype.JobState(nil), pendingJobStates...),
		}
	}
	insertOpts.Priority = jobq.EffectivePriority(spec, options...)
	return insertOpts, nil
}

var (
	_ jobq.Enqueuer = (*Adapter)(nil)
	_ jobq.Runner   = (*Adapter)(nil)
)
