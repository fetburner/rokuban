package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
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

func captureContinuationLogs(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})))
	t.Cleanup(func() { slog.SetDefault(previous) })
}

func TestEnqueueContinuation_LogsFailureAndSuccess(t *testing.T) {
	var logs bytes.Buffer
	captureContinuationLogs(t, &logs)

	args := EncodeReconcileArgs{AfterRecordingID: 42}
	cursor := []any{"after_recording_id", int64(42)}
	window := []any{"row_limit", int32(1)}
	failing := &continuationTestEnqueuer{err: errors.New("continuation unavailable")}
	EnqueueContinuation(context.Background(), failing, args, "candidate window is full; continuation enqueued", cursor, window)
	if failing.calls != 1 || !reflect.DeepEqual(failing.args, args) {
		t.Fatalf("failed enqueue calls/args = %d/%#v, want 1/%#v", failing.calls, failing.args, args)
	}
	wantFail := `level=ERROR msg="encode_reconcile: failed to enqueue continuation" after_recording_id=42 err="continuation unavailable"` + "\n"
	if got := logs.String(); got != wantFail {
		t.Fatalf("failure logs = %q, want %q", got, wantFail)
	}

	logs.Reset()
	succeeding := &continuationTestEnqueuer{}
	EnqueueContinuation(context.Background(), succeeding, args, "candidate window is full; continuation enqueued", cursor, window)
	if succeeding.calls != 1 || !reflect.DeepEqual(succeeding.args, args) {
		t.Fatalf("successful enqueue calls/args = %d/%#v, want 1/%#v", succeeding.calls, succeeding.args, args)
	}
	wantOK := `level=WARN msg="encode_reconcile: candidate window is full; continuation enqueued" row_limit=1 after_recording_id=42` + "\n"
	if got := logs.String(); got != wantOK {
		t.Fatalf("success logs = %q, want %q", got, wantOK)
	}
}

func TestEnqueueContinuation_AttrOrderWithManyAttrs(t *testing.T) {
	var logs bytes.Buffer
	captureContinuationLogs(t, &logs)

	EnqueueContinuation(context.Background(), &continuationTestEnqueuer{}, ThumbnailReconcileArgs{},
		"candidate windows are full; continuation enqueued",
		[]any{"after_recording_id", int64(1), "after_reselect_recording_id", int64(2), "after_seek_tiles_recording_id", int64(3)},
		[]any{"row_limit", int32(9), "missing_window_full", true, "reselection_window_full", false, "seek_tiles_window_full", true})
	want := `level=WARN msg="thumbnail_reconcile: candidate windows are full; continuation enqueued" row_limit=9 missing_window_full=true reselection_window_full=false seek_tiles_window_full=true after_recording_id=1 after_reselect_recording_id=2 after_seek_tiles_recording_id=3` + "\n"
	if got := logs.String(); got != want {
		t.Fatalf("logs = %q, want %q", got, want)
	}
}
