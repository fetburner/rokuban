package tsscan

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// tsScanMarkerRecorder は全件パスの完走記録を残す。err を設定すると書き込みが失敗する。
type tsScanMarkerRecorder struct {
	passes []string
	err    error
}

func (m *tsScanMarkerRecorder) MarkReconcilePass(_ context.Context, pass string) error {
	if m.err != nil {
		return m.err
	}
	m.passes = append(m.passes, pass)
	return nil
}

func (m *tsScanMarkerRecorder) ReadReconcilePassMarkers(context.Context) (map[string]time.Time, error) {
	return nil, errors.New("not used by the worker")
}

func TestReconcileWorkerMarker_FullPassMarksAfterSuccess(t *testing.T) {
	pool := testutil.SetupDB(t)
	markers := &tsScanMarkerRecorder{}
	w := &ReconcileWorker{Enqueuer: &recordingEnqueuer{}, Media: pgstoretest.Media(pool), Locator: pgstoretest.Media(pool), Markers: markers}
	if err := runWorkerViaJobQFake(context.Background(), w, ReconcileArgs{}); err != nil {
		t.Fatalf("ReconcileWorker.Work: %v", err)
	}
	if !slices.Equal(markers.passes, []string{store.ReconcilePassTSScan}) {
		t.Fatalf("recorded passes = %v, want [ts_scan]", markers.passes)
	}
}

func TestReconcileWorkerMarker_FailedListingDoesNotMark(t *testing.T) {
	pool := testutil.SetupDB(t)
	markers := &tsScanMarkerRecorder{}
	w := &ReconcileWorker{Enqueuer: &recordingEnqueuer{}, Media: failingTSScanListing{pgstoretest.Media(pool)}, Locator: pgstoretest.Media(pool), Markers: markers}
	if err := runWorkerViaJobQFake(context.Background(), w, ReconcileArgs{}); err == nil {
		t.Fatal("ReconcileWorker.Work = nil, want the listing error")
	}
	if len(markers.passes) != 0 {
		t.Fatalf("recorded passes = %v after a failed listing, want none", markers.passes)
	}
}

func TestReconcileWorkerMarker_WriteFailureFailsThePass(t *testing.T) {
	pool := testutil.SetupDB(t)
	markers := &tsScanMarkerRecorder{err: errors.New("marker write rejected")}
	w := &ReconcileWorker{Enqueuer: &recordingEnqueuer{}, Media: pgstoretest.Media(pool), Locator: pgstoretest.Media(pool), Markers: markers}
	if err := runWorkerViaJobQFake(context.Background(), w, ReconcileArgs{}); err == nil {
		t.Fatal("ReconcileWorker.Work = nil, want the marker write error")
	}
}

// failingTSScanListing は未計測の原本の列挙だけを失敗させる。
type failingTSScanListing struct {
	store.TSScan
}

func (failingTSScanListing) ListMissingTSScanRecordings(context.Context, int64, int32) ([]int64, error) {
	return nil, errors.New("listing rejected")
}
