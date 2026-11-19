package worker

import (
	"testing"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/tsscan"
)

func TestTSScanArgsAndSiteBinding(t *testing.T) {
	t.Parallel()
	scanArgs := tsscan.ScanArgs{RecordingID: 123}
	if got, want := scanArgs.Kind(), "ts_scan"; got != want {
		t.Errorf("ScanArgs.Kind() = %q, want %q", got, want)
	}
	if got := scanArgs.Spec().Queue; got != "ts_scan" {
		t.Errorf("ScanArgs.Spec().Queue = %q, want %q", got, "ts_scan")
	}
	reconcileArgs := tsscan.ReconcileArgs{AfterRecordingID: 456}
	if got, want := reconcileArgs.Kind(), "ts_scan_reconcile"; got != want {
		t.Errorf("ReconcileArgs.Kind() = %q, want %q", got, want)
	}
	if got := reconcileArgs.Spec().Queue; got != "cleanup" {
		t.Errorf("ReconcileArgs.Spec().Queue = %q, want %q", got, "cleanup")
	}

	if jobs.RequiresSiteBinding([]string{jobs.TSScanQueue}) {
		t.Error("ts_scan must be site independent")
	}
}
