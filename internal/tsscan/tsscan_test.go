package tsscan

import (
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/jobq"
)

func TestArgsSpec(t *testing.T) {
	tests := []struct {
		name    string
		args    jobq.Args
		kind    string
		timeout time.Duration
	}{
		{"scan", ScanArgs{RecordingID: 1}, "ts_scan", 6 * time.Hour},
		{"reconcile", ReconcileArgs{AfterRecordingID: 10}, "ts_scan_reconcile", 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := tt.args.Spec()
			if got := tt.args.Kind(); got != tt.kind {
				t.Errorf("Kind() = %q, want %q", got, tt.kind)
			}
			if spec.Queue != "ts_scan" {
				t.Errorf("Spec().Queue = %q, want literal ts_scan", spec.Queue)
			}
			if got := spec.Timeout(tt.args); got != tt.timeout {
				t.Errorf("Spec().Timeout(args) = %s, want literal %s", got, tt.timeout)
			}
			if spec.MaxAttempts != 0 || !spec.Coalesce || spec.Priority != 1 {
				t.Errorf("Spec() attempts/coalesce/priority = %d/%v/%d, want 0/true/1",
					spec.MaxAttempts, spec.Coalesce, spec.Priority)
			}
		})
	}
}

func TestArgsLegacyRiverInsertOptsPreserveTSScanQueueAndCoalescing(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts river.InsertOpts
	}{
		{"scan", ScanArgs{}.InsertOpts()},
		{"reconcile", ReconcileArgs{}.InsertOpts()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.opts.Queue != "ts_scan" {
				t.Errorf("InsertOpts().Queue = %q, want literal %q", tt.opts.Queue, "ts_scan")
			}
			if !tt.opts.UniqueOpts.ByArgs || !tt.opts.UniqueOpts.ByQueue || len(tt.opts.UniqueOpts.ByState) == 0 {
				t.Errorf("InsertOpts().UniqueOpts = %+v, want argument and queue coalescing over unfinished states", tt.opts.UniqueOpts)
			}
		})
	}
}

func TestScanTimeoutComesFromSpec(t *testing.T) {
	args := ScanArgs{}
	if got := args.Spec().Timeout(args); got != 6*time.Hour {
		t.Errorf("ScanArgs.Spec timeout = %s, want literal 6h", got)
	}
	reconcileArgs := ReconcileArgs{}
	if got := reconcileArgs.Spec().Timeout(reconcileArgs); got != 5*time.Minute {
		t.Errorf("ReconcileArgs.Spec timeout = %s, want literal 5m", got)
	}
}
