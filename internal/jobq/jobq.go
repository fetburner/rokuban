// Package jobq defines the job queue contract owned by Rokuban.
package jobq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fetburner/rokuban/internal/db"
)

// MaxQueueNameLen is the maximum length, in characters, of a physical queue name
// accepted by every jobq adapter.
//
// Every adapter also accepts a physical queue name made of lowercase ASCII letters and digits
// separated by single "_" or "-" characters (^[a-z0-9]([_-]?[a-z0-9])*$) up to this length.
// Names outside that character set are outside the contract and an adapter may reject them.
const MaxQueueNameLen = 64

// Args identifies a job kind and supplies the options shared by enqueue and run.
type Args interface {
	Kind() string
	Spec() Spec
}

// Spec describes the logical queue and the adapter-independent execution policy for a job.
// Coalescing uses kind, logical queue, and the complete JSON representation of Args as its
// intended identity. Args are execution inputs, so selecting only some fields could merge
// different work; the serialized payload keeps this identity usable across adapters. Coalesce
// and Priority are hints; neither guarantees exclusivity or order.
type Spec struct {
	// Queue is the adapter-independent logical queue name.
	Queue string
	// Timeout returns the maximum execution time for Args. Work's context must end with
	// context.DeadlineExceeded when it expires. If the process dies while Work runs, the
	// adapter must make the job eligible for redelivery within this duration plus its
	// recovery grace, which the adapter declares (the shared contract tests take it from
	// Fixture.RecoveryGrace). The prior invocation stopping and retaining the same job ID
	// are not guaranteed. Work that returns after the deadline is treated as failed
	// regardless of its return value (including nil, Snooze, and ErrDiscard).
	Timeout func(Args) time.Duration
	// MaxAttempts is the maximum number of failures before the adapter stops delivery.
	// Failures include non-nil errors, timeout, and process death. Zero selects the
	// adapter default. Snooze does not consume an attempt; adapters may count attempts
	// differently.
	MaxAttempts int
	// Coalesce asks the adapter to merge pending work with identical intended identity.
	Coalesce bool
	// Priority is a scheduling hint; 1 is highest and zero selects 1.
	Priority int
}

// EnqueueOpt changes one optional enqueue hint. Priority is the only supported option.
type EnqueueOpt func(*enqueueOpts)

type enqueueOpts struct {
	priority int
}

// Priority returns an option that asks the adapter to use priority n, where 1 is highest.
func Priority(n int) EnqueueOpt {
	return func(opts *enqueueOpts) { opts.priority = n }
}

// EffectivePriority applies options to a Spec and returns the effective priority.
// Enqueue implementations use this helper because the option's state is private to jobq.
func EffectivePriority(spec Spec, opts ...EnqueueOpt) int {
	priority := spec.Priority
	if priority == 0 {
		priority = 1
	}
	resolved := enqueueOpts{priority: priority}
	for _, opt := range opts {
		if opt != nil {
			opt(&resolved)
		}
	}
	if resolved.priority == 0 {
		return 1
	}
	return resolved.priority
}

// Result reports whether an adapter says the enqueue was coalesced. It is informational;
// callers must not rely on coalescing for correctness.
type Result struct {
	Coalesced bool
}

// Enqueuer inserts jobs. Enqueue must eventually deliver an accepted job at least once,
// with Args equivalent after a JSON round trip. EnqueueTx must make insertion atomic with
// tx: commit retains it, work cannot run before commit, and rollback removes it.
type Enqueuer interface {
	// Enqueue schedules args for asynchronous execution.
	Enqueue(ctx context.Context, args Args, opts ...EnqueueOpt) (Result, error)
	// EnqueueTx schedules args in the transaction so delivery follows its commit.
	EnqueueTx(ctx context.Context, tx db.Tx, args Args, opts ...EnqueueOpt) (Result, error)
}

// ErrDiscard marks a job result that must not be delivered again.
var ErrDiscard = errors.New("jobq: discard")

type snoozeError struct {
	delay time.Duration
}

func (e snoozeError) Error() string { return fmt.Sprintf("jobq: snooze for %s", e.delay) }

// Snooze asks the adapter to redeliver the same Args after at least d. Snoozes do not consume
// MaxAttempts. A negative delay returns an ordinary error.
func Snooze(d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("jobq: snooze duration must not be negative: %s", d)
	}
	return snoozeError{delay: d}
}

// SnoozeDelay reports the delay carried by an error returned from Snooze.
func SnoozeDelay(err error) (time.Duration, bool) {
	var snooze snoozeError
	if !errors.As(err, &snooze) {
		return 0, false
	}
	return snooze.delay, true
}

// Worker executes one decoded Args value. Its context deadline is Spec.Timeout(args).
// If the process dies while Work is running, the adapter must make the job eligible for
// redelivery within that timeout plus its recovery grace; it does not guarantee that the
// prior invocation has stopped before redelivery.
type Worker[T Args] interface {
	// Work processes args and returns nil, ErrDiscard, Snooze, or a retryable error.
	// The context ends with DeadlineExceeded at the Args Spec timeout.
	Work(ctx context.Context, args T) error
}

type workerHandler struct {
	decode func([]byte) (Args, error)
	work   func(context.Context, Args) error
}

// Registry maps each job kind to its Args decoder and worker.
type Registry struct {
	mu             sync.RWMutex
	handlers       map[string]workerHandler
	adapterWorkers map[string]map[string]func(any) error
}

// NewRegistry creates an empty registry for job workers.
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]workerHandler)}
}

// Register adds a worker for T. It panics when T has no kind, its zero-value timeout is not
// positive, or a worker for that kind has already been registered.
func Register[T Args](registry *Registry, worker Worker[T]) {
	if registry == nil {
		panic("jobq: registering worker in a nil registry")
	}
	if worker == nil {
		panic("jobq: registering a nil worker")
	}

	var zero T
	if typeOf := reflect.TypeOf(zero); typeOf == nil {
		panic("jobq: Args type must not be an interface")
	} else if typeOf.Kind() == reflect.Pointer {
		zero = reflect.New(typeOf.Elem()).Interface().(T)
	}
	kind := zero.Kind()
	if kind == "" {
		panic("jobq: registering Args with an empty kind")
	}
	if err := validateSpec(zero.Spec(), zero); err != nil {
		panic(fmt.Sprintf("jobq: registering %q: %v", kind, err))
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.handlers == nil {
		registry.handlers = make(map[string]workerHandler)
	}
	if _, exists := registry.handlers[kind]; exists {
		panic(fmt.Sprintf("jobq: worker for kind %q is already registered", kind))
	}
	registry.handlers[kind] = workerHandler{
		decode: func(payload []byte) (Args, error) {
			var args T
			if err := json.Unmarshal(payload, &args); err != nil {
				return nil, fmt.Errorf("jobq: decoding %q args: %w", kind, err)
			}
			if args.Kind() != kind {
				return nil, fmt.Errorf("jobq: decoded kind %q, want %q", args.Kind(), kind)
			}
			if err := validateSpec(args.Spec(), args); err != nil {
				return nil, fmt.Errorf("jobq: decoded %q args: %w", kind, err)
			}
			return args, nil
		},
		work: func(ctx context.Context, args Args) error {
			typed, ok := args.(T)
			if !ok {
				return fmt.Errorf("jobq: worker %q received %T", kind, args)
			}
			return worker.Work(ctx, typed)
		},
	}
}

// Decode returns the Args value registered for kind after a JSON round trip.
func (registry *Registry) Decode(kind string, payload []byte) (Args, error) {
	handler, err := registry.handler(kind)
	if err != nil {
		return nil, err
	}
	return handler.decode(payload)
}

// Work runs the registered worker for args.Kind.
func (registry *Registry) Work(ctx context.Context, args Args) error {
	if args == nil {
		return errors.New("jobq: cannot run nil args")
	}
	handler, err := registry.handler(args.Kind())
	if err != nil {
		return err
	}
	return handler.work(ctx, args)
}

// AddAdapterWorker associates a registered kind with a runtime-specific worker builder.
// Adapters call it from their typed registration helper so Registry can remain independent
// of the queue library's worker types.
func (registry *Registry) AddAdapterWorker(adapter, kind string, build func(any) error) error {
	if registry == nil {
		return errors.New("jobq: nil registry")
	}
	if adapter == "" || kind == "" {
		return errors.New("jobq: adapter and kind are required")
	}
	if build == nil {
		return errors.New("jobq: adapter worker builder is required")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, ok := registry.handlers[kind]; !ok {
		return fmt.Errorf("jobq: no worker registered for adapter kind %q", kind)
	}
	if registry.adapterWorkers == nil {
		registry.adapterWorkers = make(map[string]map[string]func(any) error)
	}
	workers := registry.adapterWorkers[adapter]
	if workers == nil {
		workers = make(map[string]func(any) error)
		registry.adapterWorkers[adapter] = workers
	}
	if _, exists := workers[kind]; exists {
		return fmt.Errorf("jobq: adapter %q worker for kind %q is already registered", adapter, kind)
	}
	workers[kind] = build
	return nil
}

// RegisterAdapterWorkers builds the adapter-specific workers registered for adapter into dst.
func (registry *Registry) RegisterAdapterWorkers(adapter string, dst any) error {
	if registry == nil {
		return errors.New("jobq: nil registry")
	}
	registry.mu.RLock()
	workers := registry.adapterWorkers[adapter]
	kinds := make([]string, 0, len(workers))
	for kind := range workers {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	var missing []string
	for kind := range registry.handlers {
		if _, ok := workers[kind]; !ok {
			missing = append(missing, kind)
		}
	}
	slices.Sort(missing)
	builders := make([]func(any) error, len(kinds))
	for i, kind := range kinds {
		builders[i] = workers[kind]
	}
	registry.mu.RUnlock()
	if len(missing) > 0 {
		return fmt.Errorf("jobq: kinds registered without an adapter %q worker: %s", adapter, strings.Join(missing, ", "))
	}
	if len(builders) == 0 {
		return fmt.Errorf("jobq: no workers registered for adapter %q", adapter)
	}
	for i, build := range builders {
		if err := build(dst); err != nil {
			return fmt.Errorf("jobq: registering %s worker for adapter %q: %w", kinds[i], adapter, err)
		}
	}
	return nil
}

func (registry *Registry) handler(kind string) (workerHandler, error) {
	if registry == nil {
		return workerHandler{}, errors.New("jobq: nil registry")
	}
	registry.mu.RLock()
	handler, ok := registry.handlers[kind]
	registry.mu.RUnlock()
	if !ok {
		return workerHandler{}, fmt.Errorf("jobq: no worker registered for kind %q", kind)
	}
	return handler, nil
}

func validateSpec(spec Spec, args Args) error {
	if spec.Timeout == nil {
		return errors.New("timeout function is required")
	}
	if timeout := spec.Timeout(args); timeout <= 0 {
		return fmt.Errorf("timeout must be positive, got %s", timeout)
	}
	if spec.MaxAttempts < 0 {
		return fmt.Errorf("max attempts must not be negative, got %d", spec.MaxAttempts)
	}
	return nil
}

// Runner starts and stops asynchronous delivery. Stop waits for active Work until ctx expires;
// work still running at that point has its context cancelled and is eligible for redelivery.
type Runner interface {
	// Start starts asynchronous delivery and returns after startup.
	Start(ctx context.Context) error
	// Stop stops claiming work and waits for active Work until ctx expires.
	Stop(ctx context.Context) error
}

// Deferred forwards Enqueue calls to an Enqueuer installed later in construction.
// Calls made before Set return an error. Its atomic pointer is to an interface value because
// atomic.Pointer cannot store an interface value directly.
type Deferred struct {
	enqueuer atomic.Pointer[Enqueuer]
}

// Set installs the Enqueuer used by future calls. Passing nil clears it.
func (deferred *Deferred) Set(enqueuer Enqueuer) {
	if deferred == nil || enqueuer == nil {
		if deferred != nil {
			deferred.enqueuer.Store(nil)
		}
		return
	}
	value := enqueuer
	deferred.enqueuer.Store(&value)
}

// Enqueue forwards to the installed Enqueuer or reports that Set has not been called.
func (deferred *Deferred) Enqueue(ctx context.Context, args Args, opts ...EnqueueOpt) (Result, error) {
	enqueuer, err := deferred.target()
	if err != nil {
		return Result{}, err
	}
	return (*enqueuer).Enqueue(ctx, args, opts...)
}

// EnqueueTx forwards to the installed Enqueuer or reports that Set has not been called.
func (deferred *Deferred) EnqueueTx(ctx context.Context, tx db.Tx, args Args, opts ...EnqueueOpt) (Result, error) {
	enqueuer, err := deferred.target()
	if err != nil {
		return Result{}, err
	}
	return (*enqueuer).EnqueueTx(ctx, tx, args, opts...)
}

func (deferred *Deferred) target() (*Enqueuer, error) {
	if deferred == nil {
		return nil, errors.New("jobq: deferred enqueuer is nil")
	}
	target := deferred.enqueuer.Load()
	if target == nil || *target == nil {
		return nil, errors.New("jobq: deferred enqueuer has not been set")
	}
	return target, nil
}
