package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
)

// 窓が満杯で継続を積むパスでは、完走の記録が失敗しても継続は積まれたままでなければならない。
// 記録を継続の投入より前に置くと、記録の失敗で連鎖が止まる。
func TestEncodeReconcileMarker_ContinuationIsEnqueuedBeforeMarkerWrite(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	var ids []int64
	for i := range 3 {
		ids = append(ids, prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir,
			fmt.Sprintf("marker/window%d.m2ts", i), []string{"h265"}, []byte("x")))
	}
	markers := &markerRecorder{err: errors.New("marker write rejected")}
	enqueuer := newRecordingEnqueuer()
	w := &EncodeReconcileWorker{Enqueuer: enqueuer, Profiles: encodeConfigWith("h265"),
		Media: pgstoretest.Media(pool), RowLimit: 2, Markers: markers}
	if err := runWorkerViaJobQFake(context.Background(), w, EncodeReconcileArgs{}); err == nil {
		t.Fatal("EncodeReconcileWorker.Work = nil, want the marker write error")
	}
	continuations := enqueuedArgs[EncodeReconcileArgs](enqueuer, EncodeReconcileArgs{}.Kind())
	if len(continuations) != 1 || continuations[0].AfterRecordingID != ids[1] {
		t.Fatalf("continuations = %+v, want one continuation after recording %d", continuations, ids[1])
	}
}

func TestThumbnailReconcileMarker_ContinuationIsEnqueuedBeforeMarkerWrite(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
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
	enqueuer := newRecordingEnqueuer()
	w := &ThumbnailReconcileWorker{Enqueuer: enqueuer, Media: pgstoretest.Media(pool), RowLimit: 2, Markers: markers}
	if err := runWorkerViaJobQFake(context.Background(), w, ThumbnailReconcileArgs{}); err == nil {
		t.Fatal("ThumbnailReconcileWorker.Work = nil, want the marker write error")
	}
	continuations := enqueuedArgs[ThumbnailReconcileArgs](enqueuer, ThumbnailReconcileArgs{}.Kind())
	if len(continuations) != 1 || continuations[0].AfterRecordingID != ids[1] {
		t.Fatalf("continuations = %+v, want one continuation after recording %d", continuations, ids[1])
	}
}

// 候補の投入がすべて失敗しても、一覧を完走したパスは記録する（encode 版と対）。
func TestThumbnailReconcileMarker_CandidateEnqueueFailureStillMarks(t *testing.T) {
	t.Parallel()
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	seedOriginalAsset(t, pool, mediaDir, dbtest.Recording(t, pool), "marker/thumb-enqueue.m2ts", []byte("x"))
	markers := &markerRecorder{}
	w := &ThumbnailReconcileWorker{Enqueuer: markerRejectingEnqueuer{}, Media: pgstoretest.Media(pool), Markers: markers}
	if err := runWorkerViaJobQFake(context.Background(), w, ThumbnailReconcileArgs{}); err != nil {
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
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	recordingID := dbtest.Recording(t, pool)
	seedOriginalAsset(t, pool, mediaDir, recordingID, "marker/tiles-lookup.m2ts", []byte("x"))
	seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindThumbnail, nil, "marker/tiles-lookup.jpg", []byte("jpg"))

	calls := 0
	markers := &markerRecorder{}
	w := &ThumbnailReconcileWorker{
		Enqueuer: newRecordingEnqueuer(),
		Media:    lookupFailingMedia{MediaReconcile: pgstoretest.Media(pool), calls: &calls},
		Markers:  markers,
	}
	if err := runWorkerViaJobQFake(context.Background(), w, ThumbnailReconcileArgs{}); err != nil {
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
