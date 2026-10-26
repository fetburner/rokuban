package worker

import (
	"context"
	"crypto/sha256"
	"errors"
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
		name          string
		markerPresent bool
		writeStore    string
	}{
		{name: "missing"},
		{name: "different store", markerPresent: true, writeStore: "osaka"},
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
			} else {
				wantEntries := 0
				if tc.markerPresent {
					wantEntries = 1
					if len(entries) > 0 && entries[0].Name() != mediapath.StoreMarkerFileName {
						t.Errorf("media directory entry = %q, want only marker %q", entries[0].Name(), mediapath.StoreMarkerFileName)
					}
				}
				if len(entries) != wantEntries {
					t.Errorf("media directory entries = %v, want %d entries and no ingest writes", entries, wantEntries)
				}
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

type checkpointInterruptContext struct {
	context.Context
	done     <-chan struct{}
	firstErr bool
}

type syncCountingIngestFile struct {
	*os.File
	syncCalls int
}

func (f *syncCountingIngestFile) Sync() error {
	f.syncCalls++
	return f.File.Sync()
}

func (c *checkpointInterruptContext) Done() <-chan struct{} { return c.done }
func (c *checkpointInterruptContext) Err() error {
	if c.firstErr {
		c.firstErr = false
		return nil
	}
	return context.DeadlineExceeded
}

func TestInterruptedIngestReplayRechecksMarkerBeforeCheckpoint(t *testing.T) {
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(mediaDir, "sites", "tokyo", ".rokuban-ingest-test")
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempPath, []byte("partial ingest data"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(tempPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	temp := &syncCountingIngestFile{File: file}

	done := make(chan struct{})
	close(done)
	ctx := &checkpointInterruptContext{Context: context.Background(), done: done, firstErr: true}
	metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))
	check := func() error {
		mismatchStoreMarker(t, mediaDir, "missing")
		return requireMediaStoreMarker(mediaDir, "tokyo", "saving replay ingest checkpoint")
	}
	offset, complete, err := replayIngestTempFileWithCheckpointAndCheck(ctx, tempPath, temp, sha256.New(), check)
	if !errors.Is(err, mediapath.ErrStoreMarkerMismatch) || complete {
		t.Fatalf("interrupted replay = offset:%d complete:%t err:%v, want marker mismatch and incomplete", offset, complete, err)
	}
	if temp.syncCalls != 0 {
		t.Fatalf("temp Sync calls after marker mismatch = %d, want 0", temp.syncCalls)
	}
	if _, err := os.Stat(ingestCheckpointPath(tempPath)); !os.IsNotExist(err) {
		t.Fatalf("checkpoint after marker disappeared = stat error %v, want absent", err)
	}
	if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
		t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
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

func writeMismatchedStoreMarker(t *testing.T, mediaDir, state string) {
	t.Helper()
	if state == "different store" {
		if err := mediapath.WriteStoreMarker(mediaDir, "osaka"); err != nil {
			t.Fatal(err)
		}
	}
}

func mismatchStoreMarker(t *testing.T, mediaDir, state string) {
	t.Helper()
	markerPath := filepath.Join(mediaDir, mediapath.StoreMarkerFileName)
	switch state {
	case "missing":
		if err := os.Remove(markerPath); err != nil {
			t.Errorf("removing store marker: %v", err)
		}
	case "different store":
		if err := os.WriteFile(markerPath, []byte("osaka"), 0o600); err != nil {
			t.Errorf("changing store marker: %v", err)
		}
	default:
		t.Errorf("unknown marker mismatch state %q", state)
	}
}

func TestDeleteMediaAssetWithholdsUnlinkWhenMarkerDisappears(t *testing.T) {
	for _, state := range []string{"missing", "different store"} {
		t.Run(state, func(t *testing.T) {
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
			beforeMediaAssetUnlink = func(string) { mismatchStoreMarker(t, mediaDir, state) }
			t.Cleanup(func() { beforeMediaAssetUnlink = previous })
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

			worker.deleteMediaAsset(context.Background(), store.MediaDeleteAsset{
				ID: 42, RecordingID: 7, RelPath: relPath, Kind: store.AssetKindOriginal,
			}, "trash")

			if _, err := os.Stat(path); err != nil {
				t.Fatalf("asset file after marker mismatch = %v, want it left in place", err)
			}
			if mediaDelete.markDeletingCalls != 1 || mediaDelete.markDeletedCalls != 0 {
				t.Errorf("state transitions = deleting:%d deleted:%d, want deleting:1 deleted:0", mediaDelete.markDeletingCalls, mediaDelete.markDeletedCalls)
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
			}
		})
	}
}

func TestDeleteMediaAssetWithholdsMarkDeletedWhenMarkerDisappears(t *testing.T) {
	for _, state := range []string{"missing", "different store"} {
		t.Run(state, func(t *testing.T) {
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
			beforeMediaAssetMarkDeleted = func(int64) { mismatchStoreMarker(t, mediaDir, state) }
			t.Cleanup(func() { beforeMediaAssetMarkDeleted = previous })
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

			worker.deleteMediaAsset(context.Background(), store.MediaDeleteAsset{
				ID: 43, RecordingID: 7, RelPath: relPath, Kind: store.AssetKindOriginal,
			}, "trash")

			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("asset file after unlink = stat error %v, want absent", err)
			}
			if mediaDelete.markDeletingCalls != 1 || mediaDelete.markDeletedCalls != 0 {
				t.Errorf("state transitions = deleting:%d deleted:%d, want deleting:1 deleted:0", mediaDelete.markDeletingCalls, mediaDelete.markDeletedCalls)
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
			}
		})
	}
}

func TestStoreMarkerMismatchWithholdsMissingAssetAndOrphanActions(t *testing.T) {
	for _, state := range []string{"missing", "different store"} {
		t.Run(state, func(t *testing.T) {
			mediaDir := t.TempDir()
			writeMismatchedStoreMarker(t, mediaDir, state)
			storage := &markerTestStorageObservation{
				activeAssets: []store.ActiveMediaAsset{{ID: 17, RelPath: "sites/tokyo/missing.ts"}},
			}
			worker := &DeleteReconcileWorker{Storage: storage, MediaDir: mediaDir, MediaStoreName: "tokyo"}
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))
			if _, err := worker.reconcileMissingAssets(context.Background(), nil); err == nil {
				t.Fatal("reconcileMissingAssets() succeeded with a mismatched store marker")
			}
			if storage.activeListCalls != 0 || storage.missingUpsertCalls != 0 || storage.missingDeleteCalls != 0 {
				t.Fatalf("missing-asset DB calls with marker mismatch = list:%d upsert:%d delete:%d, want 0:0:0", storage.activeListCalls, storage.missingUpsertCalls, storage.missingDeleteCalls)
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
				t.Fatalf("orphan DB deletes with marker mismatch = %d, want 0", storage.orphanDeleteCalls)
			}
			if _, err := worker.agedUnpublishedOrphans(context.Background(), time.Second, time.Second); err == nil {
				t.Fatal("agedUnpublishedOrphans() succeeded with a mismatched store marker")
			}
			if storage.orphanListCalls != 0 {
				t.Fatalf("aged orphan DB reads with marker mismatch = %d, want 0", storage.orphanListCalls)
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+3 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+3)
			}
		})
	}
}

func TestMissingAssetRecordRechecksStoreMarkerImmediatelyBeforeWrite(t *testing.T) {
	for _, state := range []string{"missing", "different store"} {
		t.Run(state, func(t *testing.T) {
			mediaDir := t.TempDir()
			if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
				t.Fatal(err)
			}
			storage := &markerTestStorageObservation{
				activeAssets: []store.ActiveMediaAsset{{ID: 18, RelPath: "sites/tokyo/missing.ts"}},
				onListActive: func() { mismatchStoreMarker(t, mediaDir, state) },
			}
			worker := &DeleteReconcileWorker{Storage: storage, MediaDir: mediaDir, MediaStoreName: "tokyo"}
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))
			if _, err := worker.reconcileMissingAssets(context.Background(), nil); err == nil {
				t.Fatal("reconcileMissingAssets() succeeded after the marker changed")
			}
			if storage.missingUpsertCalls != 0 || storage.missingDeleteCalls != 0 {
				t.Fatalf("missing-asset DB writes after marker mismatch = upsert:%d delete:%d, want 0:0", storage.missingUpsertCalls, storage.missingDeleteCalls)
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
			}
		})
	}
}

func TestOrphanUnlinkRechecksStoreMarkerImmediatelyBeforeRemoval(t *testing.T) {
	for _, state := range []string{"missing", "different store"} {
		t.Run(state, func(t *testing.T) {
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
			beforeOrphanFileUnlink = func(string) { mismatchStoreMarker(t, mediaDir, state) }
			t.Cleanup(func() { beforeOrphanFileUnlink = previous })
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

			worker.deleteOrphanFile(filepath.ToSlash(relPath), time.Second)
			if _, err := os.Stat(fullPath); err != nil {
				t.Fatalf("orphan file after marker rejection = %v, want it left in place", err)
			}
			if storage.orphanDeleteCalls != 0 {
				t.Fatalf("orphan DB deletes after marker mismatch = %d, want 0", storage.orphanDeleteCalls)
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
			}
		})
	}
}
