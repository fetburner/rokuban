package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/tsscan"
)

type tsScanRouteStore struct {
	store.TSScan
	err error
}

func (s tsScanRouteStore) GetActiveOriginalForTSScan(context.Context, int64) (store.TSScanSource, error) {
	return store.TSScanSource{}, s.err
}

func TestNewWorkersTSScanUsesStoreScopedMedia(t *testing.T) {
	t.Parallel()

	unscopedErr := errors.New("unscoped TS scan store called")
	scopedErr := errors.New("store-scoped TS scan store called")
	registry := NewWorkers(&Deps{
		UnscopedTSScan: tsScanRouteStore{err: unscopedErr},
		PerStore: map[string]*StoreDeps{
			"tokyo": {MediaStoreName: "tokyo", TSScan: tsScanRouteStore{err: scopedErr}},
		},
	})

	err := registry.Work(t.Context(), tsscan.ScanArgs{Store: "tokyo", RecordingID: 1})
	if !errors.Is(err, scopedErr) {
		t.Fatalf("TS scan media error = %v, want store-scoped error %v", err, scopedErr)
	}
}

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
