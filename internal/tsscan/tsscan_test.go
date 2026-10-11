package tsscan

import (
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobq"
)

func TestArgsSpecAndRiverProjection(t *testing.T) {
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
			insertOpts := tt.args.(river.JobArgsWithInsertOpts).InsertOpts()
			if insertOpts.Queue != "ts_scan" {
				t.Errorf("InsertOpts().Queue = %q, want literal ts_scan", insertOpts.Queue)
			}
			if !insertOpts.UniqueOpts.ByArgs || !insertOpts.UniqueOpts.ByQueue {
				t.Errorf("InsertOpts().UniqueOpts = %+v, want args and queue coalescing", insertOpts.UniqueOpts)
			}
			if len(insertOpts.UniqueOpts.ByState) != 5 || insertOpts.UniqueOpts.ByState[3] != rivertype.JobStateRunning {
				t.Errorf("InsertOpts().UniqueOpts.ByState = %v, want five unfinished states including running", insertOpts.UniqueOpts.ByState)
			}
		})
	}
}

func TestScanTimeoutComesFromSpec(t *testing.T) {
	if got := (&ScanWorker{}).Timeout(nil); got != 6*time.Hour {
		t.Errorf("ScanWorker.Timeout(nil) = %s, want literal 6h", got)
	}
	if got := (&ReconcileWorker{}).Timeout(nil); got != 5*time.Minute {
		t.Errorf("ReconcileWorker.Timeout(nil) = %s, want literal 5m", got)
	}
}
