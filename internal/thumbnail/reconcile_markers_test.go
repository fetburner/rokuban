package thumbnail

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestThumbnailReconcileMarker_FullPassMarksOnlyAfterSuccess(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	w := &ThumbnailReconcileWorker{Enqueuer: jobqfaketest.NewRecordingEnqueuer(), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.ThumbnailReconcileArgs{}), "ThumbnailReconcileWorker.Work")
	if got := markers.recordedPasses(); !slices.Equal(got, []string{store.ReconcilePassThumbnail}) {
		t.Fatalf("recorded passes = %v, want [thumbnail]", got)
	}
}

// シークタイルの一覧が取れなかったパスは、完走として記録しない。ジョブ自体は失敗にしない（従来どおり）。
func TestThumbnailReconcileMarker_FailedSeekTilesListingDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	w := &ThumbnailReconcileWorker{Enqueuer: jobqfaketest.NewRecordingEnqueuer(),
		Media: failingSeekTilesListing{pgstoretest.Media(pool)}, Markers: markers}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.ThumbnailReconcileArgs{}), "ThumbnailReconcileWorker.Work, want nil (the seek tiles failure is counted, not returned)")
	if got := markers.recordedPasses(); len(got) != 0 {
		t.Fatalf("recorded passes = %v after a failed seek tiles listing, want none", got)
	}
}

func TestThumbnailReconcileMarker_TargetedPassDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	recordingID := dbtest.Recording(t, pool)
	w := &ThumbnailReconcileWorker{Enqueuer: jobqfaketest.NewRecordingEnqueuer(), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.ThumbnailReconcileArgs{RecordingID: recordingID}), "ThumbnailReconcileWorker.Work(targeted)")
	if got := markers.recordedPasses(); len(got) != 0 {
		t.Fatalf("recorded passes = %v after a targeted pass, want none", got)
	}
}

// failingSeekTilesListing は seek_tiles の候補の列挙だけを失敗させる。
type failingSeekTilesListing struct {
	store.MediaReconcile
}

func (failingSeekTilesListing) ListMissingSeekTilesRecordings(context.Context, store.MediaReconcilePage) ([]int64, error) {
	return nil, errors.New("seek tiles listing rejected")
}

func TestThumbnailReconcileMarker_ContinuationIsEnqueuedBeforeMarkerWrite(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	var ids []int64
	for i := range 3 {
		id := dbtest.Recording(t, pool)
		seedOriginalAsset(t, pool, mediaDir, id, fmt.Sprintf("marker/thumb%d.m2ts", i), []byte("x"))
		ids = append(ids, id)
	}
	markers := &markerRecorder{err: errors.New("marker write rejected")}
	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	w := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 2, Markers: markers}
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.ThumbnailReconcileArgs{}); err == nil {
		t.Fatal("ThumbnailReconcileWorker.Work = nil, want the marker write error")
	}
	continuations := recordedArgs[jobs.ThumbnailReconcileArgs](t, enqueuer.CallsOfKind("thumbnail_reconcile"))
	if len(continuations) != 1 || continuations[0].AfterRecordingID != ids[1] {
		t.Fatalf("continuations = %+v, want one continuation after recording %d", continuations, ids[1])
	}
}

// 候補の投入がすべて失敗しても、一覧を完走したパスは記録する（encode 版と対）。
func TestThumbnailReconcileMarker_CandidateEnqueueFailureStillMarks(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	seedOriginalAsset(t, pool, mediaDir, dbtest.Recording(t, pool), "marker/thumb-enqueue.m2ts", []byte("x"))
	markers := &markerRecorder{}
	w := &ThumbnailReconcileWorker{Enqueuer: markerRejectingEnqueuer{}, Media: pgstoretest.Media(pool), Markers: markers}
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.ThumbnailReconcileArgs{}); err != nil {
		t.Fatalf("ThumbnailReconcileWorker.Work: %v, want nil when only candidate enqueues fail", err)
	}
	if got := markers.recordedPasses(); !slices.Equal(got, []string{store.ReconcilePassThumbnail}) {
		t.Fatalf("recorded passes = %v, want [thumbnail]", got)
	}
}

// seek_tiles の候補は見つかるが、その原本の置き場を引けないパスは記録しない。
// missing thumbnail の候補は空にして（サムネイルは既にある）、seek_tiles の引き当てだけを失敗させる。
func TestThumbnailReconcileMarker_FailedSeekTileStoreLookupDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, recordingID, "marker/tiles-lookup.m2ts", []byte("x"))
	seedMediaAsset(t, pool, mediaDir, recordingID, store.AssetKindThumbnail, nil, "marker/tiles-lookup.jpg", []byte("jpg"))

	calls := 0
	markers := &markerRecorder{}
	w := &ThumbnailReconcileWorker{
		Enqueuer: jobqfaketest.NewRecordingEnqueuer(),
		Media:    lookupFailingMedia{MediaReconcile: pgstoretest.Media(pool), calls: &calls},
		Markers:  markers,
	}
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.ThumbnailReconcileArgs{}); err != nil {
		t.Fatalf("ThumbnailReconcileWorker.Work: %v, want nil (the lookup failure is counted, not returned)", err)
	}
	if calls == 0 {
		t.Fatal("seek tiles candidates never reached the original lookup; the test does not exercise the path")
	}
	if got := markers.recordedPasses(); len(got) != 0 {
		t.Fatalf("recorded passes = %v after a failed seek tiles store lookup, want none", got)
	}
}

// lookupFailingMedia は空でない原本の置き場の引き当てだけを失敗させる。候補の一覧は本物を使う。
type lookupFailingMedia struct {
	store.MediaReconcile
	calls *int
}

func (m lookupFailingMedia) OriginalStores(ctx context.Context, ids []int64) (map[int64]string, error) {
	if len(ids) == 0 {
		return m.MediaReconcile.OriginalStores(ctx, ids)
	}
	*m.calls++
	return nil, errors.New("original store lookup rejected")
}

type markerRecorder struct {
	store.LoopMarkers
	mu     sync.Mutex
	passes []string
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

func (m *markerRecorder) recordedPasses() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.passes)
}

// markerRejectingEnqueuer は全件の投入を失敗させる。候補ごとの失敗がパスを止めないことを見る。
type markerRejectingEnqueuer struct{}

func (markerRejectingEnqueuer) Enqueue(context.Context, jobq.Args, ...jobq.EnqueueOpt) (jobq.Result, error) {
	return jobq.Result{}, errors.New("enqueue rejected")
}

// failingEncodeListing は候補の列挙だけを失敗させる。
