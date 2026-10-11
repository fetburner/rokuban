// Package jobqfake provides a deterministic in-memory job queue for contract tests.
package jobqfake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobq"
)

const defaultMaxAttempts = 25

// Config exposes deliberate violations of jobq's non-guarantees for adversarial tests.
// Whatever the other fields say, the fake never coalesces and delivers the newest entry first,
// ignoring priority, because that is the weakest behavior jobq's contract allows. Coalesce and
// InOrder strengthen it; they give guarantees the contract does not, so use them only in the
// fake's own tests. SnoozeDelay overrides the requested delay when non-nil; setting it to zero
// ignores delay.
type Config struct {
	// DuplicateDelivery queues one extra copy before the first Work call finishes.
	DuplicateDelivery bool
	// Coalesce merges an insertion whose Spec asks for it with identical pending or active work.
	// This is stronger than the contract; do not use it outside the fake's own tests.
	Coalesce bool
	// InOrder delivers by priority, then insertion order. This is stronger than the contract;
	// do not use it outside the fake's own tests.
	InOrder bool
	// RedeliverAfterSuccess queues one more delivery after Work returns nil.
	RedeliverAfterSuccess bool
	// SnoozeDelay overrides the requested delay; a pointer to zero ignores it.
	SnoozeDelay *time.Duration
	// NewIDEachDelivery assigns a different observed ID to every delivery.
	NewIDEachDelivery bool
}

// Delivery records one worker invocation. Attempt is the retry attempt number; snoozes keep
// the same number because they do not consume the failure budget.
type Delivery struct {
	JobID   string
	Kind    string
	Attempt int
	Args    []byte
	Started time.Time
}

type entry struct {
	id                 string
	kind               string
	queue              string
	coalesceKey        string
	payload            []byte
	priority           int
	maxAttempts        int
	availableAt        time.Time
	sequence           uint64
	failures           int
	duplicateRemaining int
	redelivered        bool
}

type execution struct {
	entry    *entry
	cancel   context.CancelFunc
	force    chan struct{}
	forced   bool
	finished bool
}

// Queue implements jobq.Enqueuer and jobq.Runner with one deterministic worker.
type Queue struct {
	mu         sync.Mutex
	registry   *jobq.Registry
	config     Config
	entries    []*entry
	active     *execution
	deliveries []Delivery
	sequence   uint64
	idSequence uint64
	changed    chan struct{}
	stop       chan struct{}
	loopDone   chan struct{}
	started    bool
	stopping   bool
}

// New creates a fake queue using registry and config. Unless Config.Coalesce or
// Config.InOrder is set, it does not coalesce and ignores insertion order and priority,
// as tested by TestKnobsKeepWeakestDefaults.
func New(registry *jobq.Registry, config Config) *Queue {
	if config.SnoozeDelay != nil {
		delay := *config.SnoozeDelay
		config.SnoozeDelay = &delay
	}
	return &Queue{registry: registry, config: config, changed: make(chan struct{})}
}

// Enqueue serializes args, applies the Spec, and schedules it in memory.
func (queue *Queue) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return jobq.Result{}, err
	}
	payload, spec, priority, err := prepare(args, opts...)
	if err != nil {
		return jobq.Result{}, err
	}
	return queue.enqueue(payload, args.Kind(), spec, priority)
}

// EnqueueTx defers scheduling until the fake transaction commits.
func (queue *Queue) EnqueueTx(ctx context.Context, tx db.Tx, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	if err := ctxErr(ctx); err != nil {
		return jobq.Result{}, err
	}
	transaction, ok := tx.(*Tx)
	if !ok || transaction == nil {
		return jobq.Result{}, errors.New("jobqfake: EnqueueTx requires a jobqfake transaction")
	}
	payload, spec, priority, err := prepare(args, opts...)
	if err != nil {
		return jobq.Result{}, err
	}
	if err := transaction.onCommit(func() {
		_, _ = queue.enqueue(payload, args.Kind(), spec, priority)
	}); err != nil {
		return jobq.Result{}, err
	}
	return jobq.Result{}, nil
}

func prepare(args jobq.Args, opts ...jobq.EnqueueOpt) ([]byte, jobq.Spec, int, error) {
	if args == nil {
		return nil, jobq.Spec{}, 0, errors.New("jobqfake: args are nil")
	}
	kind := args.Kind()
	if kind == "" {
		return nil, jobq.Spec{}, 0, errors.New("jobqfake: args kind is empty")
	}
	spec := args.Spec()
	if spec.Timeout == nil || spec.Timeout(args) <= 0 {
		return nil, jobq.Spec{}, 0, fmt.Errorf("jobqfake: %s timeout must be positive", kind)
	}
	if spec.MaxAttempts < 0 {
		return nil, jobq.Spec{}, 0, fmt.Errorf("jobqfake: %s max attempts must not be negative", kind)
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return nil, jobq.Spec{}, 0, fmt.Errorf("jobqfake: encoding %s args: %w", kind, err)
	}
	return payload, spec, jobq.EffectivePriority(spec, opts...), nil
}

func (queue *Queue) enqueue(payload []byte, kind string, spec jobq.Spec, priority int) (jobq.Result, error) {
	if queue == nil {
		return jobq.Result{}, errors.New("jobqfake: queue is nil")
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.config.SnoozeDelay != nil && *queue.config.SnoozeDelay < 0 {
		return jobq.Result{}, errors.New("jobqfake: snooze delay override must not be negative")
	}
	coalesceKey := kind + "\x00" + spec.Queue + "\x00" + string(payload)
	if spec.Coalesce && queue.config.Coalesce && queue.findCoalescedLocked(coalesceKey) {
		return jobq.Result{Coalesced: true}, nil
	}
	queue.sequence++
	entry := &entry{
		id:                 queue.nextIDLocked(),
		kind:               kind,
		queue:              spec.Queue,
		coalesceKey:        coalesceKey,
		payload:            slices.Clone(payload),
		priority:           priority,
		maxAttempts:        spec.MaxAttempts,
		availableAt:        time.Now(),
		sequence:           queue.sequence,
		duplicateRemaining: boolInt(queue.config.DuplicateDelivery),
	}
	queue.entries = append(queue.entries, entry)
	queue.notifyLocked()
	return jobq.Result{}, nil
}

func (queue *Queue) findCoalescedLocked(key string) bool {
	if queue.active != nil && queue.active.entry.coalesceKey == key {
		return true
	}
	for _, entry := range queue.entries {
		if entry.coalesceKey == key {
			return true
		}
	}
	return false
}

// Start starts the fake's single-worker delivery loop.
func (queue *Queue) Start(ctx context.Context) error {
	if queue == nil {
		return errors.New("jobqfake: queue is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.started {
		return errors.New("jobqfake: runner is already started")
	}
	if queue.registry == nil {
		return errors.New("jobqfake: registry is nil")
	}
	queue.started = true
	queue.stopping = false
	queue.stop = make(chan struct{})
	queue.loopDone = make(chan struct{})
	stop := queue.stop
	loopDone := queue.loopDone
	go queue.run(ctx, stop, loopDone)
	return nil
}

func (queue *Queue) run(ctx context.Context, stop <-chan struct{}, done chan struct{}) {
	defer func() {
		queue.mu.Lock()
		queue.started = false
		queue.stopping = false
		queue.stop = nil
		queue.loopDone = nil
		close(done)
		queue.notifyLocked()
		queue.mu.Unlock()
	}()
	for {
		active, wait, changed, stopped := queue.next()
		if stopped {
			return
		}
		if active != nil {
			queue.execute(ctx, active)
			continue
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-changed:
				stopTimer(timer)
			case <-stop:
				stopTimer(timer)
				return
			case <-ctx.Done():
				stopTimer(timer)
				queue.requestStop()
				return
			}
			continue
		}
		select {
		case <-changed:
		case <-stop:
			return
		case <-ctx.Done():
			queue.requestStop()
			return
		}
	}
}

func (queue *Queue) next() (*execution, time.Duration, <-chan struct{}, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.stopping {
		return nil, 0, queue.changed, true
	}
	if len(queue.entries) == 0 {
		return nil, 0, queue.changed, false
	}
	now := time.Now()
	chosen := -1
	var earliest time.Time
	for i, candidate := range queue.entries {
		if chosen == -1 || candidate.availableAt.Before(earliest) {
			earliest = candidate.availableAt
		}
		if candidate.availableAt.After(now) {
			continue
		}
		if chosen == -1 || queue.precedes(candidate, queue.entries[chosen]) {
			chosen = i
		}
	}
	if chosen == -1 {
		return nil, time.Until(earliest), queue.changed, false
	}
	entry := queue.entries[chosen]
	queue.entries = append(queue.entries[:chosen], queue.entries[chosen+1:]...)
	if entry.duplicateRemaining > 0 {
		entry.duplicateRemaining--
		queue.entries = append(queue.entries, entry)
	}
	active := &execution{entry: entry, force: make(chan struct{})}
	queue.active = active
	queue.notifyLocked()
	return active, 0, queue.changed, false
}

func (queue *Queue) precedes(candidate, current *entry) bool {
	if !queue.config.InOrder {
		return candidate.sequence > current.sequence
	}
	if candidate.priority != current.priority {
		return candidate.priority < current.priority
	}
	return candidate.sequence < current.sequence
}

func (queue *Queue) execute(parent context.Context, active *execution) {
	entry := active.entry
	args, err := queue.registry.Decode(entry.kind, entry.payload)
	if err != nil {
		queue.finish(entry, active, err)
		return
	}
	spec := args.Spec()
	timeout := spec.Timeout(args)
	if timeout <= 0 {
		queue.finish(entry, active, fmt.Errorf("jobqfake: %s timeout must be positive", entry.kind))
		return
	}
	workCtx, cancel := context.WithTimeout(parent, timeout)
	queue.mu.Lock()
	if queue.active != active || active.forced {
		queue.mu.Unlock()
		cancel()
		queue.finish(entry, active, context.Canceled)
		return
	}
	active.cancel = cancel
	deliveryID := entry.id
	if queue.config.NewIDEachDelivery {
		deliveryID = queue.nextIDLocked()
	}
	queue.deliveries = append(queue.deliveries, Delivery{
		JobID: deliveryID, Kind: entry.kind, Attempt: entry.failures + 1,
		Args: slices.Clone(entry.payload), Started: time.Now(),
	})
	queue.notifyLocked()
	queue.mu.Unlock()

	result := make(chan error, 1)
	go func() { result <- queue.registry.Work(workCtx, args) }()
	var workErr error
	select {
	case workErr = <-result:
		if workCtx.Err() == context.DeadlineExceeded {
			workErr = context.DeadlineExceeded
		}
	case <-workCtx.Done():
		workErr = workCtx.Err()
	case <-active.force:
		workErr = context.Canceled
	}
	queue.mu.Lock()
	forced := active.forced
	queue.mu.Unlock()
	if forced {
		workErr = context.Canceled
	}
	cancel()
	queue.finish(entry, active, workErr)
}

func (queue *Queue) finish(entry *entry, active *execution, workErr error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if active != nil && queue.active != active {
		return
	}
	if active != nil {
		active.finished = true
		queue.active = nil
	}

	if errors.Is(workErr, jobq.ErrDiscard) {
		queue.notifyLocked()
		return
	}
	if delay, ok := jobq.SnoozeDelay(workErr); ok {
		if queue.config.SnoozeDelay != nil {
			delay = *queue.config.SnoozeDelay
		}
		queue.rescheduleLocked(entry, delay)
		return
	}
	if workErr == nil {
		switch {
		case queue.config.RedeliverAfterSuccess && !entry.redelivered:
			entry.redelivered = true
			queue.rescheduleLocked(entry, 0)
		default:
			queue.notifyLocked()
		}
		return
	}

	entry.failures++
	maxAttempts := entry.maxAttempts
	if maxAttempts == 0 {
		maxAttempts = defaultMaxAttempts
	}
	if entry.failures < maxAttempts {
		queue.rescheduleLocked(entry, 0)
		return
	}
	queue.notifyLocked()
}

func (queue *Queue) rescheduleLocked(entry *entry, delay time.Duration) {
	entry.availableAt = time.Now().Add(delay)
	queue.entries = append(queue.entries, entry)
	queue.notifyLocked()
}

func (queue *Queue) requestStop() {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.started && !queue.stopping {
		queue.stopping = true
		close(queue.stop)
		queue.notifyLocked()
	}
}

// Stop stops claiming jobs and waits for active Work. If ctx expires first, the active Work
// context is cancelled and the job is retried when its failure budget allows it.
func (queue *Queue) Stop(ctx context.Context) error {
	if queue == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	queue.requestStop()
	queue.mu.Lock()
	if !queue.started {
		queue.mu.Unlock()
		return nil
	}
	done := queue.loopDone
	queue.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}

	queue.mu.Lock()
	active := queue.active
	if active != nil && !active.forced && !active.finished {
		active.forced = true
		if active.cancel != nil {
			active.cancel()
		}
		close(active.force)
		queue.notifyLocked()
	}
	queue.mu.Unlock()
	<-done
	return ctx.Err()
}

// Crash abruptly stops delivery and cancels active Work to simulate process death.
func (queue *Queue) Crash(ctx context.Context) error {
	if queue == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	queue.mu.Lock()
	if !queue.started {
		queue.mu.Unlock()
		return nil
	}
	if !queue.stopping {
		queue.stopping = true
		close(queue.stop)
	}
	active := queue.active
	if active != nil && !active.forced && !active.finished {
		active.forced = true
		if active.cancel != nil {
			active.cancel()
		}
		close(active.force)
	}
	queue.notifyLocked()
	done := queue.loopDone
	queue.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitIdle waits until no queued or active delivery remains.
func (queue *Queue) WaitIdle(ctx context.Context) error {
	if queue == nil {
		return errors.New("jobqfake: queue is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		queue.mu.Lock()
		if len(queue.entries) == 0 && queue.active == nil {
			queue.mu.Unlock()
			return nil
		}
		changed := queue.changed
		var wait time.Duration
		if queue.active == nil && len(queue.entries) > 0 {
			earliest := queue.entries[0].availableAt
			for _, entry := range queue.entries[1:] {
				if entry.availableAt.Before(earliest) {
					earliest = entry.availableAt
				}
			}
			wait = time.Until(earliest)
		}
		queue.mu.Unlock()
		if wait <= 0 {
			select {
			case <-changed:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-changed:
			stopTimer(timer)
		case <-ctx.Done():
			stopTimer(timer)
			return ctx.Err()
		}
	}
}

// RecoveryGrace reports the fake's recovery grace. It redelivers at once after Crash or
// Stop, so the grace is small.
func (queue *Queue) RecoveryGrace() time.Duration { return time.Second }

// Deliveries returns a copy of the deliveries observed so far.
func (queue *Queue) Deliveries() []Delivery {
	if queue == nil {
		return nil
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	deliveries := slices.Clone(queue.deliveries)
	for i := range deliveries {
		deliveries[i].Args = slices.Clone(deliveries[i].Args)
	}
	return deliveries
}

func (queue *Queue) nextIDLocked() string {
	queue.idSequence++
	return fmt.Sprintf("fake-%06d", queue.idSequence)
}

func (queue *Queue) notifyLocked() {
	close(queue.changed)
	queue.changed = make(chan struct{})
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// BeginTx creates a fake transaction whose commit or rollback controls EnqueueTx.
func (queue *Queue) BeginTx(context.Context) (db.Tx, error) {
	return &Tx{}, nil
}

// Tx captures EnqueueTx callbacks until Commit or Rollback.
type Tx struct {
	// The fake only needs transaction completion methods. Embedded pgx.Tx keeps this value
	// compatible with db.Tx without duplicating pgx's unused query methods.
	pgx.Tx
	mu        sync.Mutex
	callbacks []func()
	finished  bool
}

func (tx *Tx) onCommit(callback func()) error {
	if tx == nil {
		return errors.New("jobqfake: transaction is nil")
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.finished {
		return errors.New("jobqfake: transaction is already finished")
	}
	tx.callbacks = append(tx.callbacks, callback)
	return nil
}

// Commit runs callbacks registered by EnqueueTx.
func (tx *Tx) Commit(context.Context) error {
	if tx == nil {
		return errors.New("jobqfake: transaction is nil")
	}
	tx.mu.Lock()
	if tx.finished {
		tx.mu.Unlock()
		return errors.New("jobqfake: transaction is already finished")
	}
	tx.finished = true
	callbacks := slices.Clone(tx.callbacks)
	tx.callbacks = nil
	tx.mu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
	return nil
}

// Rollback discards callbacks registered by EnqueueTx.
func (tx *Tx) Rollback(context.Context) error {
	if tx == nil {
		return errors.New("jobqfake: transaction is nil")
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.finished {
		return errors.New("jobqfake: transaction is already finished")
	}
	tx.finished = true
	tx.callbacks = nil
	return nil
}
