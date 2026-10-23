package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/jobq"
)

type continuationTestEnqueuer struct {
	args  jobq.Args
	err   error
	calls int
}

func (e *continuationTestEnqueuer) Enqueue(_ context.Context, args jobq.Args, _ ...jobq.EnqueueOpt) (jobq.Result, error) {
	e.args = args
	e.calls++
	return jobq.Result{}, e.err
}

func TestEnqueueContinuation_LogsFailureAndSuccess(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	args := EncodeReconcileArgs{AfterRecordingID: 42}
	failing := &continuationTestEnqueuer{err: errors.New("continuation unavailable")}
	EnqueueContinuation(
		context.Background(), failing, args,
		"encode_reconcile", "candidate window is full; continuation enqueued",
		[]any{"after_recording_id", int64(42)},
		[]any{"row_limit", int32(1), "after_recording_id", int64(42)},
	)
	if failing.calls != 1 || !reflect.DeepEqual(failing.args, args) {
		t.Fatalf("failed enqueue calls/args = %d/%#v, want 1/%#v", failing.calls, failing.args, args)
	}
	if got := logs.String(); !strings.Contains(got, `level=ERROR msg="encode_reconcile: failed to enqueue continuation"`) ||
		!strings.Contains(got, "after_recording_id=42") || !strings.Contains(got, "continuation unavailable") ||
		strings.Contains(got, "level=WARN") {
		t.Fatalf("failure logs = %q, want one Error log with the enqueue error", got)
	}

	logs.Reset()
	succeeding := &continuationTestEnqueuer{}
	EnqueueContinuation(
		context.Background(), succeeding, args,
		"encode_reconcile", "candidate window is full; continuation enqueued",
		[]any{"after_recording_id", int64(42)},
		[]any{"row_limit", int32(1), "after_recording_id", int64(42)},
	)
	if succeeding.calls != 1 || !reflect.DeepEqual(succeeding.args, args) {
		t.Fatalf("successful enqueue calls/args = %d/%#v, want 1/%#v", succeeding.calls, succeeding.args, args)
	}
	if got := logs.String(); !strings.Contains(got, `level=WARN msg="encode_reconcile: candidate window is full; continuation enqueued"`) ||
		!strings.Contains(got, "row_limit=1") || !strings.Contains(got, "after_recording_id=42") ||
		strings.Contains(got, "level=ERROR") {
		t.Fatalf("success logs = %q, want one Warn log with the continuation attributes", got)
	}
}
