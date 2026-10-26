package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/store"
)

func TestIngestWorkerWithholdsWhenStoreMarkerDoesNotMatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		writeStore string
	}{
		{name: "missing"},
		{name: "different store", writeStore: "osaka"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mediaDir := t.TempDir()
			if tc.writeStore != "" {
				if err := mediapath.WriteStoreMarker(mediaDir, tc.writeStore); err != nil {
					t.Fatal(err)
				}
			}
			worker := &IngestWorker{
				MirakcClients:  map[string]*mirakc.Client{"tokyo": nil},
				MediaDir:       mediaDir,
				MediaStoreName: "tokyo",
			}
			before := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))
			err := worker.Work(context.Background(), jobs.IngestJobArgs{Site: "tokyo"})
			if err == nil {
				t.Fatal("Work() succeeded with an unverified media store, want retryable error")
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != before+1 {
				t.Errorf("store marker metric = %v, want %v", got, before+1)
			}
			if entries, err := os.ReadDir(mediaDir); err != nil {
				t.Fatal(err)
			} else if tc.writeStore == "" && len(entries) != 0 {
				t.Errorf("media directory entries = %v, want no writes", entries)
			}
		})
	}
}

func TestGeneratedMediaPublishDoesNotStageWhenStoreMarkerIsMissing(t *testing.T) {
	mediaDir := t.TempDir()
	scratch := filepath.Join(t.TempDir(), "thumbnail.jpg")
	if err := os.WriteFile(scratch, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	_, _, err := publishGeneratedMediaAsset(context.Background(), mediaDir, "tokyo", "thumbnails/1.jpg", scratch,
		func(int64, func() error) (bool, error) {
			called = true
			return false, nil
		})
	if err == nil {
		t.Fatal("publishGeneratedMediaAsset() succeeded without a store marker")
	}
	if called {
		t.Fatal("commit callback ran without a store marker")
	}
	if _, err := os.Stat(filepath.Join(mediaDir, "thumbnails", "1.jpg")); !os.IsNotExist(err) {
		t.Fatalf("generated output exists after marker rejection: stat error = %v", err)
	}
}

type markerTestMediaDelete struct {
	markDeletingCalls int
	markDeletedCalls  int
}

func (*markerTestMediaDelete) ListPendingDeleteAssets(context.Context, time.Time, int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (*markerTestMediaDelete) ListUnqualifiedDeletingAssets(context.Context, time.Time, int32) ([]store.MediaDeleteRevertCandidate, error) {
	return nil, nil
}
func (*markerTestMediaDelete) ListTrashAssetsToDelete(context.Context, time.Time, int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (*markerTestMediaDelete) ListUntilEncodedOriginalsToDelete(context.Context, int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (*markerTestMediaDelete) ListRemovedEncodedAssetsToDelete(context.Context, int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (s *markerTestMediaDelete) MarkDeleting(context.Context, int64) error {
	s.markDeletingCalls++
	return nil
}
func (*markerTestMediaDelete) MarkUntilEncodedDeleting(context.Context, int64, int64) (bool, error) {
	return true, nil
}
func (s *markerTestMediaDelete) MarkDeleted(context.Context, int64) error {
	s.markDeletedCalls++
	return nil
}
func (*markerTestMediaDelete) RevertToActive(context.Context, int64, time.Time) (bool, error) {
	return false, nil
}

type markerTestStorageObservation struct {
	store.StorageObservation
	onListActive       func()
	activeAssets       []store.ActiveMediaAsset
	agedOrphans        []string
	activeListCalls    int
	missingUpsertCalls int
	missingDeleteCalls int
	orphanListCalls    int
	orphanDeleteCalls  int
}

func (*markerTestStorageObservation) ListAllMediaAssetRelPaths(context.Context) ([]string, error) {
	return nil, nil
}
func (s *markerTestStorageObservation) ListAgedOrphanFiles(context.Context, time.Duration) ([]string, error) {
	s.orphanListCalls++
	return s.agedOrphans, nil
}
func (s *markerTestStorageObservation) ListActiveMediaAssets(context.Context) ([]store.ActiveMediaAsset, error) {
	s.activeListCalls++
	if s.onListActive != nil {
		s.onListActive()
	}
	return s.activeAssets, nil
}
func (*markerTestStorageObservation) ListAllMissingMediaAssetIDs(context.Context) ([]int64, error) {
	return nil, nil
}
func (s *markerTestStorageObservation) UpsertMissingMediaAsset(context.Context, int64) error {
	s.missingUpsertCalls++
	return nil
}
func (s *markerTestStorageObservation) DeleteMissingMediaAsset(context.Context, int64) error {
	s.missingDeleteCalls++
	return nil
}
func (s *markerTestStorageObservation) DeleteOrphanFile(context.Context, string) error {
	s.orphanDeleteCalls++
	return nil
}

func TestDeleteMediaAssetWithholdsUnlinkWhenMarkerDisappears(t *testing.T) {
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
		t.Fatal(err)
	}
	relPath := "sites/tokyo/original.ts"
	path := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	mediaDelete := &markerTestMediaDelete{}
	worker := &DeleteReconcileWorker{MediaDelete: mediaDelete, MediaDir: mediaDir, MediaStoreName: "tokyo"}
	previous := beforeMediaAssetUnlink
	beforeMediaAssetUnlink = func(string) { _ = os.Remove(filepath.Join(mediaDir, mediapath.StoreMarkerFileName)) }
	t.Cleanup(func() { beforeMediaAssetUnlink = previous })
	metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

	worker.deleteMediaAsset(context.Background(), store.MediaDeleteAsset{
		ID: 42, RecordingID: 7, RelPath: relPath, Kind: store.AssetKindOriginal,
	}, "trash")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("asset file after marker removal = %v, want it left in place", err)
	}
	if mediaDelete.markDeletingCalls != 1 || mediaDelete.markDeletedCalls != 0 {
		t.Errorf("state transitions = deleting:%d deleted:%d, want deleting:1 deleted:0", mediaDelete.markDeletingCalls, mediaDelete.markDeletedCalls)
	}
	if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
		t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
	}
}

func TestDeleteMediaAssetWithholdsMarkDeletedWhenMarkerDisappears(t *testing.T) {
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
		t.Fatal(err)
	}
	relPath := "sites/tokyo/original.ts"
	path := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	mediaDelete := &markerTestMediaDelete{}
	worker := &DeleteReconcileWorker{MediaDelete: mediaDelete, MediaDir: mediaDir, MediaStoreName: "tokyo"}
	previous := beforeMediaAssetMarkDeleted
	beforeMediaAssetMarkDeleted = func(int64) { _ = os.Remove(filepath.Join(mediaDir, mediapath.StoreMarkerFileName)) }
	t.Cleanup(func() { beforeMediaAssetMarkDeleted = previous })

	worker.deleteMediaAsset(context.Background(), store.MediaDeleteAsset{
		ID: 43, RecordingID: 7, RelPath: relPath, Kind: store.AssetKindOriginal,
	}, "trash")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("asset file after unlink = stat error %v, want absent", err)
	}
	if mediaDelete.markDeletingCalls != 1 || mediaDelete.markDeletedCalls != 0 {
		t.Errorf("state transitions = deleting:%d deleted:%d, want deleting:1 deleted:0", mediaDelete.markDeletingCalls, mediaDelete.markDeletedCalls)
	}
}

func TestMissingStoreMarkerWithholdsMissingAssetAndOrphanActions(t *testing.T) {
	mediaDir := t.TempDir()
	storage := &markerTestStorageObservation{
		activeAssets: []store.ActiveMediaAsset{{ID: 17, RelPath: "sites/tokyo/missing.ts"}},
	}
	worker := &DeleteReconcileWorker{Storage: storage, MediaDir: mediaDir, MediaStoreName: "tokyo"}
	if _, err := worker.reconcileMissingAssets(context.Background(), nil); err == nil {
		t.Fatal("reconcileMissingAssets() succeeded without a store marker")
	}
	if storage.activeListCalls != 0 || storage.missingUpsertCalls != 0 {
		t.Fatalf("missing-asset DB calls without marker = list:%d upsert:%d, want 0:0", storage.activeListCalls, storage.missingUpsertCalls)
	}

	relPath := filepath.Join("thumbnails", mediapath.GeneratedAssetTempFilePrefix+"orphan")
	fullPath := filepath.Join(mediaDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(fullPath, old, old); err != nil {
		t.Fatal(err)
	}
	worker.deleteOrphanFile(filepath.ToSlash(relPath), time.Second)
	if _, err := os.Stat(fullPath); err != nil {
		t.Fatalf("orphan file after marker rejection = %v, want it left in place", err)
	}
	if storage.orphanDeleteCalls != 0 {
		t.Fatalf("orphan DB deletes without marker = %d, want 0", storage.orphanDeleteCalls)
	}
	if _, err := worker.agedUnpublishedOrphans(context.Background(), time.Second, time.Second); err == nil {
		t.Fatal("agedUnpublishedOrphans() succeeded without a store marker")
	}
	if storage.orphanListCalls != 0 {
		t.Fatalf("aged orphan DB reads without marker = %d, want 0", storage.orphanListCalls)
	}
}

func TestMissingAssetRecordRechecksStoreMarkerImmediatelyBeforeWrite(t *testing.T) {
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
		t.Fatal(err)
	}
	storage := &markerTestStorageObservation{
		activeAssets: []store.ActiveMediaAsset{{ID: 18, RelPath: "sites/tokyo/missing.ts"}},
		onListActive: func() {
			if err := os.Remove(filepath.Join(mediaDir, mediapath.StoreMarkerFileName)); err != nil {
				t.Errorf("removing marker during audit: %v", err)
			}
		},
	}
	worker := &DeleteReconcileWorker{Storage: storage, MediaDir: mediaDir, MediaStoreName: "tokyo"}
	if _, err := worker.reconcileMissingAssets(context.Background(), nil); err == nil {
		t.Fatal("reconcileMissingAssets() succeeded after the marker disappeared")
	}
	if storage.missingUpsertCalls != 0 {
		t.Fatalf("missing-asset DB writes after marker disappeared = %d, want 0", storage.missingUpsertCalls)
	}
}

func TestOrphanUnlinkRechecksStoreMarkerImmediatelyBeforeRemoval(t *testing.T) {
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
		t.Fatal(err)
	}
	relPath := filepath.Join("thumbnails", mediapath.GeneratedAssetTempFilePrefix+"orphan")
	fullPath := filepath.Join(mediaDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(fullPath, old, old); err != nil {
		t.Fatal(err)
	}
	storage := &markerTestStorageObservation{}
	worker := &DeleteReconcileWorker{Storage: storage, MediaDir: mediaDir, MediaStoreName: "tokyo"}
	previous := beforeOrphanFileUnlink
	beforeOrphanFileUnlink = func(string) {
		if err := os.Remove(filepath.Join(mediaDir, mediapath.StoreMarkerFileName)); err != nil {
			t.Errorf("removing marker before orphan unlink: %v", err)
		}
	}
	t.Cleanup(func() { beforeOrphanFileUnlink = previous })

	worker.deleteOrphanFile(filepath.ToSlash(relPath), time.Second)
	if _, err := os.Stat(fullPath); err != nil {
		t.Fatalf("orphan file after marker rejection = %v, want it left in place", err)
	}
	if storage.orphanDeleteCalls != 0 {
		t.Fatalf("orphan DB deletes after marker disappeared = %d, want 0", storage.orphanDeleteCalls)
	}
}
