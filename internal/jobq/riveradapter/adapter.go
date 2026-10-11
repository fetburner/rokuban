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

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
)

const adapterWorkerKey = "river"

// Adapter implements jobq.Enqueuer and jobq.Runner with one River client.
type Adapter struct {
	// lifecycleMu serializes Start and Stop without blocking client snapshots from Work.
	lifecycleMu sync.Mutex
	// mu protects client and stopped (Stop was called, so the next Start needs a new
	// client); never hold it while calling River.
	mu       sync.Mutex
	pool     *pgxpool.Pool
	registry *jobq.Registry
	config   Config
	client   *river.Client[pgx.Tx]
	stopped  bool
}

// NewAdapter builds River workers from registry and creates a PostgreSQL-backed adapter.
func NewAdapter(pool *pgxpool.Pool, registry *jobq.Registry, cfg Config) (*Adapter, error) {
	if registry == nil {
		return nil, errors.New("riveradapter: registry is required")
	}
	client, err := newRiverClient(pool, registry, cfg)
	if err != nil {
		return nil, err
	}
	return &Adapter{pool: pool, registry: registry, config: cfg, client: client}, nil
}

func newRiverClient(pool *pgxpool.Pool, registry *jobq.Registry, cfg Config) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	if err := registry.RegisterAdapterWorkers(adapterWorkerKey, workers); err != nil {
		return nil, fmt.Errorf("riveradapter: building workers: %w", err)
	}
	riverCfg, err := BuildConfig(workers, cfg)
	if err != nil {
		return nil, err
	}
	client, err := river.NewClient(riverpgxv5.New(pool), riverCfg)
	if err != nil {
		return nil, fmt.Errorf("riveradapter: creating River client: %w", err)
	}
	return client, nil
}

// Register adds a jobq worker to registry and records how River should register it.
func Register[T jobq.Args](registry *jobq.Registry, worker jobq.Worker[T]) {
	jobq.Register(registry, worker)
	kind := zeroArgs[T]().Kind()
	err := registry.AddAdapterWorker(adapterWorkerKey, kind, func(dst any) error {
		workers, ok := dst.(*river.Workers)
		if !ok || workers == nil {
			return fmt.Errorf("expected *river.Workers, got %T", dst)
		}
		river.AddWorker(workers, &riverWorker[T]{registry: registry})
		return nil
	})
	if err != nil {
		panic(fmt.Sprintf("riveradapter: registering %q: %v", kind, err))
	}
}

// Enqueue inserts args using the options derived from its jobq Spec.
func (adapter *Adapter) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	client := adapter.clientSnapshot()
	if client == nil {
		return jobq.Result{}, errors.New("riveradapter: client is nil")
	}
	insertOpts, err := makeInsertOpts(args, opts...)
	if err != nil {
		return jobq.Result{}, err
	}
	result, err := client.Insert(ctx, riverEnvelope{args: args}, &insertOpts)
	if err != nil {
		return jobq.Result{}, fmt.Errorf("riveradapter: inserting %q: %w", args.Kind(), err)
	}
	return jobq.Result{Coalesced: result.UniqueSkippedAsDuplicate}, nil
}

// EnqueueTx inserts args in tx so delivery follows the transaction commit.
func (adapter *Adapter) EnqueueTx(ctx context.Context, tx db.Tx, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	client := adapter.clientSnapshot()
	if client == nil {
		return jobq.Result{}, errors.New("riveradapter: client is nil")
	}
	if tx == nil {
		return jobq.Result{}, errors.New("riveradapter: transaction is nil")
	}
	insertOpts, err := makeInsertOpts(args, opts...)
	if err != nil {
		return jobq.Result{}, err
	}
	result, err := client.InsertTx(ctx, tx, riverEnvelope{args: args}, &insertOpts)
	if err != nil {
		return jobq.Result{}, fmt.Errorf("riveradapter: inserting %q in transaction: %w", args.Kind(), err)
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
		newClient, err := newRiverClient(adapter.pool, adapter.registry, adapter.config)
		if err != nil {
			return err
		}
		adapter.mu.Lock()
		adapter.client = newClient
		adapter.stopped = false
		adapter.mu.Unlock()
		client = newClient
	}
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("riveradapter: starting River client: %w", err)
	}
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
	adapter.stopped = true
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

func makeInsertOpts(args jobq.Args, options ...jobq.EnqueueOpt) (river.InsertOpts, error) {
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
	insertOpts := jobs.RiverInsertOpts(spec, jobs.PhysicalQueueFor(args))
	insertOpts.Priority = jobq.EffectivePriority(spec, options...)
	return insertOpts, nil
}

var (
	_ jobq.Enqueuer = (*Adapter)(nil)
	_ jobq.Runner   = (*Adapter)(nil)
)
