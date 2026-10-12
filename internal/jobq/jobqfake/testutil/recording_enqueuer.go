package testutil

import (
	"context"
	"sync"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobq"
)

// EnqueueCall records one invocation of a RecordingEnqueuer.
type EnqueueCall struct {
	Args          jobq.Args
	Options       []jobq.EnqueueOpt
	Transactional bool
	Tx            db.Tx
}

// RecordingEnqueuer records enqueue requests for tests of jobq callers.
type RecordingEnqueuer struct {
	mu    sync.Mutex
	calls []EnqueueCall
}

// NewRecordingEnqueuer creates an empty enqueue recorder.
func NewRecordingEnqueuer() *RecordingEnqueuer {
	return &RecordingEnqueuer{}
}

// Enqueue records an enqueue request and returns the adapter's neutral result.
func (enqueuer *RecordingEnqueuer) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	if err := ctx.Err(); err != nil {
		return jobq.Result{}, err
	}
	enqueuer.record(EnqueueCall{Args: args, Options: append([]jobq.EnqueueOpt(nil), opts...)})
	return jobq.Result{}, nil
}

// EnqueueTx records a transactional enqueue request and returns the adapter's neutral result.
func (enqueuer *RecordingEnqueuer) EnqueueTx(ctx context.Context, tx db.Tx, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	if err := ctx.Err(); err != nil {
		return jobq.Result{}, err
	}
	enqueuer.record(EnqueueCall{
		Args: args, Options: append([]jobq.EnqueueOpt(nil), opts...), Transactional: true, Tx: tx,
	})
	return jobq.Result{}, nil
}

// Calls returns a snapshot of all recorded enqueue requests.
func (enqueuer *RecordingEnqueuer) Calls() []EnqueueCall {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	return cloneCalls(enqueuer.calls)
}

// CallsOfKind returns recorded enqueue requests whose Args have kind.
func (enqueuer *RecordingEnqueuer) CallsOfKind(kind string) []EnqueueCall {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	calls := make([]EnqueueCall, 0, len(enqueuer.calls))
	for _, call := range enqueuer.calls {
		if call.Args != nil && call.Args.Kind() == kind {
			calls = append(calls, call)
		}
	}
	return cloneCalls(calls)
}

// Reset discards all recorded enqueue requests.
func (enqueuer *RecordingEnqueuer) Reset() {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	enqueuer.calls = nil
}

func (enqueuer *RecordingEnqueuer) record(call EnqueueCall) {
	enqueuer.mu.Lock()
	defer enqueuer.mu.Unlock()
	enqueuer.calls = append(enqueuer.calls, call)
}

func cloneCalls(calls []EnqueueCall) []EnqueueCall {
	cloned := make([]EnqueueCall, len(calls))
	for i, call := range calls {
		call.Options = append([]jobq.EnqueueOpt(nil), call.Options...)
		cloned[i] = call
	}
	return cloned
}

var _ jobq.Enqueuer = (*RecordingEnqueuer)(nil)
