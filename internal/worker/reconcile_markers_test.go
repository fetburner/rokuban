package worker

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// markerRecorder は全件パスと削除 reconcile の完走記録を残す。err を設定すると書き込みが失敗する。
type markerRecorder struct {
	mu     sync.Mutex
	passes []string
	stores []string
	err    error
}

func (m *markerRecorder) MarkReconcilePass(_ context.Context, pass string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.passes = append(m.passes, pass)
	return nil
}

func (m *markerRecorder) MarkRulerPasses(context.Context, []string) error {
	return errors.New("not used by the worker")
}

func (m *markerRecorder) MarkRecordSweep(context.Context, string) error {
	return errors.New("not used by the worker")
}

func (m *markerRecorder) ReadLoopMarkers(context.Context, string) (map[string]time.Time, error) {
	return nil, errors.New("not used by the worker")
}

func (m *markerRecorder) MarkReconciler(context.Context, string) error {
	return errors.New("not used by the worker")
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

func (m *markerRecorder) recordedPasses() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.passes)
}

func (m *markerRecorder) recordedStores() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.stores)
}

// markerRejectingEnqueuer は全件の投入を失敗させる。候補ごとの失敗がパスを止めないことを見る。
type markerRejectingEnqueuer struct{}

func (markerRejectingEnqueuer) Enqueue(context.Context, jobq.Args, ...jobq.EnqueueOpt) (jobq.Result, error) {
	return jobq.Result{}, errors.New("enqueue rejected")
}

// failingEncodeListing は候補の列挙だけを失敗させる。
type failingEncodeListing struct {
	store.MediaReconcile
}

func (failingEncodeListing) ListMissingEncodeProfiles(context.Context, store.MissingEncodeProfilesParams) ([]store.MissingEncodeProfile, error) {
	return nil, errors.New("listing rejected")
}

// failingSeekTilesListing は seek_tiles の候補の列挙だけを失敗させる。
type failingSeekTilesListing struct {
	store.MediaReconcile
}

func (failingSeekTilesListing) ListMissingSeekTilesRecordings(context.Context, store.MediaReconcilePage) ([]int64, error) {
	return nil, errors.New("seek tiles listing rejected")
}

func TestEncodeReconcileMarker_FullPassMarksOnlyAfterSuccess(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	w := &EncodeReconcileWorker{Enqueuer: newRecordingEnqueuer(), Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, EncodeReconcileArgs{}), "EncodeReconcileWorker.Work")
	if got := markers.recordedPasses(); !slices.Equal(got, []string{store.ReconcilePassEncode}) {
		t.Fatalf("recorded passes = %v, want [encode] after a completed full pass", got)
	}
}

func TestEncodeReconcileMarker_FailedListingDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	w := &EncodeReconcileWorker{Enqueuer: newRecordingEnqueuer(), Profiles: encodeConfigWith("h265"),
		Media: failingEncodeListing{pgstoretest.Media(pool)}, Markers: markers}
	if err := runWorkerViaJobQFake(context.Background(), w, EncodeReconcileArgs{}); err == nil {
		t.Fatal("EncodeReconcileWorker.Work = nil, want the listing error")
	}
	if got := markers.recordedPasses(); len(got) != 0 {
		t.Fatalf("recorded passes = %v, want none for an incomplete pass", got)
	}
}

func TestEncodeReconcileMarker_TargetedPassDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "marker/targeted.m2ts",
		[]string{"h265"}, []byte("x"))
	markers := &markerRecorder{}
	w := &EncodeReconcileWorker{Enqueuer: newRecordingEnqueuer(), Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, EncodeReconcileArgs{RecordingID: recordingID}), "EncodeReconcileWorker.Work(targeted)")
	if got := markers.recordedPasses(); len(got) != 0 {
		t.Fatalf("recorded passes = %v after a targeted pass, want none", got)
	}
}

// 候補の投入が全部失敗しても、一覧を完走したパスは記録する（候補ごとの失敗でパスを止めない）。
func TestEncodeReconcileMarker_CandidateEnqueueFailureStillMarks(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "marker/enqueue.m2ts",
		[]string{"h265"}, []byte("x"))
	markers := &markerRecorder{}
	w := &EncodeReconcileWorker{Enqueuer: markerRejectingEnqueuer{}, Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, EncodeReconcileArgs{}), "EncodeReconcileWorker.Work, want nil when only candidate enqueues fail")
	if got := markers.recordedPasses(); !slices.Equal(got, []string{store.ReconcilePassEncode}) {
		t.Fatalf("recorded passes = %v, want [encode]", got)
	}
}

func TestEncodeReconcileMarker_WriteFailureFailsThePass(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{err: errors.New("marker write rejected")}
	w := &EncodeReconcileWorker{Enqueuer: newRecordingEnqueuer(), Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Markers: markers}
	if err := runWorkerViaJobQFake(context.Background(), w, EncodeReconcileArgs{}); err == nil {
		t.Fatal("EncodeReconcileWorker.Work = nil, want the marker write error")
	}
}

func TestThumbnailReconcileMarker_FullPassMarksOnlyAfterSuccess(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	w := &ThumbnailReconcileWorker{Enqueuer: newRecordingEnqueuer(), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, ThumbnailReconcileArgs{}), "ThumbnailReconcileWorker.Work")
	if got := markers.recordedPasses(); !slices.Equal(got, []string{store.ReconcilePassThumbnail}) {
		t.Fatalf("recorded passes = %v, want [thumbnail]", got)
	}
}

// シークタイルの一覧が取れなかったパスは、完走として記録しない。ジョブ自体は失敗にしない（従来どおり）。
func TestThumbnailReconcileMarker_FailedSeekTilesListingDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	w := &ThumbnailReconcileWorker{Enqueuer: newRecordingEnqueuer(),
		Media: failingSeekTilesListing{pgstoretest.Media(pool)}, Markers: markers}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, ThumbnailReconcileArgs{}), "ThumbnailReconcileWorker.Work, want nil (the seek tiles failure is counted, not returned)")
	if got := markers.recordedPasses(); len(got) != 0 {
		t.Fatalf("recorded passes = %v after a failed seek tiles listing, want none", got)
	}
}

func TestThumbnailReconcileMarker_TargetedPassDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	recordingID := dbtest.Recording(t, pool)
	w := &ThumbnailReconcileWorker{Enqueuer: newRecordingEnqueuer(), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, ThumbnailReconcileArgs{RecordingID: recordingID}), "ThumbnailReconcileWorker.Work(targeted)")
	if got := markers.recordedPasses(); len(got) != 0 {
		t.Fatalf("recorded passes = %v after a targeted pass, want none", got)
	}
}

func TestDeleteReconcileMarker_FullPassMarksTheStore(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "fs-a"), "writing store marker")
	markers := &markerRecorder{}
	w := buildDeleteReconcileWorker(t, pool, mediaDir, withMarkerStore(t, mediaDir, markers))
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
	if got := markers.recordedStores(); !slices.Equal(got, []string{"fs-a"}) {
		t.Fatalf("recorded delete markers = %v, want [fs-a]", got)
	}
}

// ブレーカーで止めたパスも完走として記録する（今のプロセス内ゲージと同じ扱い）。
// 2 件の候補に対して上限 1 にすると、必ずブレーカーが発動する。削除は行われない。
func TestDeleteReconcileMarker_BreakerStopStillMarks(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
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
	testutil.RequireNoError(t, runWorkerViaJobQFake(context.Background(), w, DeleteReconcileArgs{}), "DeleteReconcileWorker.Work")
	if got := singleCopyState(t, pool, assetID); got == "deleted" {
		t.Fatal("asset was deleted, want the breaker to withhold the pass")
	}
	if got := markers.recordedStores(); !slices.Equal(got, []string{"fs-a"}) {
		t.Fatalf("recorded delete markers after a breaker stop = %v, want [fs-a]", got)
	}
}

func TestDeleteReconcileMarker_FailedPassDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "fs-a"), "writing store marker")
	markers := &markerRecorder{}
	w := buildDeleteReconcileWorker(t, pool, mediaDir, withMarkerStore(t, mediaDir, markers))
	w.MediaDelete = failingPendingDelete{pgstoretest.Media(pool)}
	if err := runWorkerViaJobQFake(context.Background(), w, DeleteReconcileArgs{}); err == nil {
		t.Fatal("DeleteReconcileWorker.Work = nil, want the listing error")
	}
	if got := markers.recordedStores(); len(got) != 0 {
		t.Fatalf("recorded delete markers = %v after a failed pass, want none", got)
	}
}

func TestDeleteReconcileMarker_WriteFailureFailsThePass(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "fs-a"), "writing store marker")
	markers := &markerRecorder{err: errors.New("marker write rejected")}
	w := buildDeleteReconcileWorker(t, pool, mediaDir, withMarkerStore(t, mediaDir, markers))
	if err := runWorkerViaJobQFake(context.Background(), w, DeleteReconcileArgs{}); err == nil {
		t.Fatal("DeleteReconcileWorker.Work = nil, want the marker write error")
	}
}

// failingPendingDelete は pending の列挙だけを失敗させる。パスが完走する前に止まる経路を作る。
type failingPendingDelete struct {
	store.MediaDelete
}

func (failingPendingDelete) ListPendingDeleteAssets(context.Context, time.Time, time.Time, []byte, string, int32) ([]store.MediaDeleteAsset, error) {
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
