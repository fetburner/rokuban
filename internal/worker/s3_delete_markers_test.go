package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// emptyDeleteStores は空の置き場を返す。呼ばない更新メソッドは埋め込みで panic させる。
type emptyDeleteStores struct {
	store.MediaDelete
	store.StorageObservation
	store.PendingCopyReclaim
	store.RecordingPurge
	store.Breakers
	deleteCalls int
	stopped     bool
	candidate   bool
}

func (f *emptyDeleteStores) ListPendingDeleteAssets(ctx context.Context, graceCutoff, overlapCutoff time.Time, originalRules []byte, derivedStore string, limit int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListUnqualifiedDeletingAssets(ctx context.Context, graceCutoff, overlapCutoff time.Time, originalRules []byte, derivedStore string, limit int32) ([]store.MediaDeleteRevertCandidate, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListOldGenerationMediaAssetCopiesToDelete(ctx context.Context, overlapCutoff time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListTrashAssetsToDelete(ctx context.Context, graceCutoff time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	if f.candidate {
		return []store.MediaDeleteAsset{{ID: 1}}, nil
	}
	return nil, nil
}
func (f *emptyDeleteStores) ListUntilEncodedOriginalsToDelete(ctx context.Context, limit int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListRemovedEncodedAssetsToDelete(ctx context.Context, limit int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListAgedPendingMediaAssetCopies(ctx context.Context, before time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListStorageSync(ctx context.Context) ([]store.StorageSync, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListAllMediaAssetRelPaths(ctx context.Context) ([]string, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListAllOrphanFiles(ctx context.Context) ([]store.OrphanFile, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListAgedOrphanFiles(ctx context.Context, age time.Duration) ([]string, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListActiveMediaAssets(ctx context.Context) ([]store.ActiveMediaAsset, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListAllMissingMediaAssetIDs(ctx context.Context) ([]int64, error) {
	return nil, nil
}
func (f *emptyDeleteStores) ListAgedMissingMediaAssets(ctx context.Context, age time.Duration) ([]store.AgedMissingMediaAsset, error) {
	return nil, nil
}
func (f *emptyDeleteStores) MarkPurged(ctx context.Context, graceCutoff time.Time) ([]store.PurgedRecording, error) {
	return nil, nil
}
func (f *emptyDeleteStores) IsTripped(_ context.Context, scope, name string) (bool, error) {
	return f.stopped && scope == "s3-a", nil
}

func (f *emptyDeleteStores) MarkDeleting(context.Context, int64, string) (bool, error) {
	f.deleteCalls++
	return false, nil
}

type selectiveMarkers struct {
	markerRecorder
	reject bool
}

func (m *selectiveMarkers) MarkDeleteReconcile(ctx context.Context, name string) error {
	if m.reject && name == "s3-a" {
		return errors.New("marker rejected")
	}
	return m.markerRecorder.MarkDeleteReconcile(ctx, name)
}
func TestS3DeleteMarkers(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		fail, stopped, reject, absent, nonOwner bool
	}{
		{name: "success"}, {name: "failure", fail: true}, {name: "breaker", stopped: true}, {name: "marker_failure", reject: true}, {name: "unconfigured", absent: true}, {name: "non_owner", nonOwner: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			testutil.RequireNoError(t, mediapath.WriteStoreMarker(dir, "fs-a"), "mediapath.WriteStoreMarker")
			empty := &emptyDeleteStores{stopped: tc.stopped}
			markers := &selectiveMarkers{reject: tc.reject}
			w := &DeleteReconcileWorker{MediaDir: dir, MediaStoreName: "fs-a", MediaDelete: empty, Storage: empty, RecordingPurge: empty, Breakers: empty, Markers: markers}
			if !tc.absent {
				w.DerivedObjectStoreName = "s3-a"
				w.DerivedObjects = testFilesystemObjectStore(t, t.TempDir())
				w.DerivedStorage = empty
				w.DerivedPendingReclaim = empty
				w.DerivedMediaDelete = &emptyDeleteStores{candidate: tc.stopped}
				if tc.fail {
					w.DerivedMediaDelete = failingPendingDelete{empty}
				}
			}
			if tc.nonOwner {
				w.DerivedMediaDelete = nil
				w.DerivedStorage = nil
				w.DerivedPendingReclaim = nil
			}
			for i := 1; i <= 2; i++ {
				err := runWorkerViaJobQFake(context.Background(), w, DeleteReconcileArgs{})
				if (err != nil) != (tc.fail || tc.reject) {
					t.Fatalf("Work error = %v", err)
				}
				if tc.stopped && w.DerivedMediaDelete.(*emptyDeleteStores).deleteCalls != 0 {
					t.Fatal("breaker allowed deletion")
				}
				got := markers.recordedStores()
				fs, s3 := 0, 0
				for _, n := range got {
					if n == "fs-a" {
						fs++
					}
					if n == "s3-a" {
						s3++
					}
				}
				if fs != i {
					t.Fatalf("FS marker count = %d, want %d", fs, i)
				}
				want := i
				if tc.fail || tc.reject || tc.absent || tc.nonOwner {
					want = 0
				}
				if s3 != want {
					t.Fatalf("S3 marker count = %d, want %d; stores %v", s3, want, got)
				}
				if len(got) != fs+s3 {
					t.Fatalf("unexpected marker stores: %v", got)
				}
			}
		})
	}
}
