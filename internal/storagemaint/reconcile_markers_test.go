package storagemaint

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// markerRecorder は削除 reconcile の完走記録を残す。err を設定すると書き込みが失敗する。
type markerRecorder struct {
	store.LoopMarkers
	mu     sync.Mutex
	stores []string
	err    error
}

func (m *markerRecorder) MarkDeleteReconcile(_ context.Context, storeName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.stores = append(m.stores, storeName)
	return nil
}

func (m *markerRecorder) recordedStores() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.stores)
}

func TestDeleteReconcileMarker_FullPassMarksTheStore(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "fs-a"), "writing store marker")
	markers := &markerRecorder{}
	w := buildDeleteReconcileWorker(t, pool, mediaDir, withMarkerStore(t, mediaDir, markers))
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
	if got := markers.recordedStores(); !slices.Equal(got, []string{"fs-a"}) {
		t.Fatalf("recorded delete markers = %v, want [fs-a]", got)
	}
}

// ブレーカーで止めたパスも完走として記録する（今のプロセス内ゲージと同じ扱い）。
// 2 件の候補に対して上限 1 にすると、必ずブレーカーが発動する。削除は行われない。
func TestDeleteReconcileMarker_BreakerStopStillMarks(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "fs-a"), "writing store marker")
	recordingID := dbtest.Recording(t, pool)
	assetID := seedOriginalAsset(t, pool, mediaDir, recordingID, "breaker/original.m2ts", []byte("data"))
	profile := "h264"
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindEncoded, &profile, "breaker/original_h264.mp4", []byte("enc"))
	markRecordingTrashed(t, pool, recordingID)

	markers := &markerRecorder{}
	w := buildDeleteReconcileWorker(t, pool, mediaDir, withMarkerStore(t, mediaDir, markers))
	w.MaxDeletesPerPass = 1
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
	if got := singleCopyState(t, pool, assetID); got == "deleted" {
		t.Fatal("asset was deleted, want the breaker to withhold the pass")
	}
	if got := markers.recordedStores(); !slices.Equal(got, []string{"fs-a"}) {
		t.Fatalf("recorded delete markers after a breaker stop = %v, want [fs-a]", got)
	}
}

func TestDeleteReconcileMarker_FailedPassDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "fs-a"), "writing store marker")
	markers := &markerRecorder{}
	w := buildDeleteReconcileWorker(t, pool, mediaDir, withMarkerStore(t, mediaDir, markers))
	w.MediaDelete = failingPendingDelete{pgstoretest.Media(pool)}
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.DeleteReconcileArgs{}); err == nil {
		t.Fatal("DeleteReconcileWorker.Work = nil, want the listing error")
	}
	if got := markers.recordedStores(); len(got) != 0 {
		t.Fatalf("recorded delete markers = %v after a failed pass, want none", got)
	}
}

func TestDeleteReconcileMarker_WriteFailureFailsThePass(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "fs-a"), "writing store marker")
	markers := &markerRecorder{err: errors.New("marker write rejected")}
	w := buildDeleteReconcileWorker(t, pool, mediaDir, withMarkerStore(t, mediaDir, markers))
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.DeleteReconcileArgs{}); err == nil {
		t.Fatal("DeleteReconcileWorker.Work = nil, want the marker write error")
	}
}

// failingPendingDelete は pending の列挙だけを失敗させる。パスが完走する前に止まる経路を作る。
type failingPendingDelete struct {
	store.MediaDelete
}

func (failingPendingDelete) ListPendingDeleteAssets(context.Context, time.Time, time.Time, store.StoragePolicy, int32) ([]store.MediaDeleteAsset, error) {
	return nil, errors.New("listing pending rejected")
}

// markerTestStore は削除 reconcile のテストで使う FS の置き場の名前。
const markerTestStore = "fs-a"

// withMarkerStore は、置き場 markerTestStore 1 つを名乗り、完走を markers に記録する構成に上書きする。
func withMarkerStore(t *testing.T, mediaDir string, markers store.LoopMarkers) func(*DeleteReconcileWorker) {
	return func(w *DeleteReconcileWorker) {
		w.MediaStoreName = markerTestStore
		w.StorageObjects = testStorageObjects(t, markerTestStore, mediaDir)
		w.Markers = markers
	}
}
