package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/fsink"
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
	// mkdir も temp の作成も行わない（置き場の目印が無い root には何も作らない）。
	if _, err := os.Stat(filepath.Join(mediaDir, "thumbnails")); !os.IsNotExist(err) {
		t.Fatalf("generated output directory exists after marker rejection: stat error = %v", err)
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

func TestInterruptedIngestReplayWithholdsCheckpointWhenMarkerDisappears(t *testing.T) {
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
	offset, complete, err := fsink.ReplayTempFileWithCheckpoint(ctx, tempPath, temp, sha256.New(), check)
	if !errors.Is(err, mediapath.ErrStoreMarkerMismatch) || complete {
		t.Fatalf("interrupted replay = offset:%d complete:%t err:%v, want marker mismatch and incomplete", offset, complete, err)
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
	revertCalls       int
}

func (*markerTestMediaDelete) ListPendingDeleteAssets(context.Context, time.Time, time.Time, int32) ([]store.MediaDeleteAsset, error) {
	return nil, nil
}
func (*markerTestMediaDelete) ListUnqualifiedDeletingAssets(context.Context, time.Time, time.Time, int32) ([]store.MediaDeleteRevertCandidate, error) {
	return nil, nil
}
func (*markerTestMediaDelete) ListOldGenerationMediaAssetCopiesToDelete(context.Context, time.Time, int32) ([]store.MediaDeleteAsset, error) {
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
func (s *markerTestMediaDelete) MarkDeleting(context.Context, int64, string) (bool, error) {
	s.markDeletingCalls++
	return true, nil
}
func (*markerTestMediaDelete) MarkUntilEncodedDeleting(context.Context, int64, int64, int64) (bool, error) {
	return true, nil
}
func (s *markerTestMediaDelete) MarkDeleted(context.Context, int64) error {
	s.markDeletedCalls++
	return nil
}
func (s *markerTestMediaDelete) RevertToActive(context.Context, int64, time.Time, time.Time) (bool, error) {
	s.revertCalls++
	return false, nil
}

type markerTestStorageObservation struct {
	store.StorageObservation
	onListKnown        func()
	onListActive       func()
	orphanUpsertCalls  int
	activeAssets       []store.ActiveMediaAsset
	agedOrphans        []string
	activeListCalls    int
	missingUpsertCalls int
	missingDeleteCalls int
	orphanListCalls    int
	orphanDeleteCalls  int
}

func (s *markerTestStorageObservation) ListAllMediaAssetRelPaths(context.Context) ([]string, error) {
	if s.onListKnown != nil {
		s.onListKnown()
	}
	return nil, nil
}
func (s *markerTestStorageObservation) UpsertOrphanFile(context.Context, string) error {
	s.orphanUpsertCalls++
	return nil
}
func (*markerTestStorageObservation) ListAllOrphanFiles(context.Context) ([]store.OrphanFile, error) {
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
			relPath := filepath.Join("thumbnails", mediapath.GeneratedAssetTempFilePrefix+"orphan")
			storage := &markerTestStorageObservation{
				activeAssets: []store.ActiveMediaAsset{{ID: 17, RelPath: "sites/tokyo/missing.ts"}},
				agedOrphans:  []string{filepath.ToSlash(relPath)},
			}
			worker := &DeleteReconcileWorker{Storage: storage, MediaDir: mediaDir, MediaStoreName: "tokyo"}
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))
			// 実体無しの記録（walk の結果 → DB）。
			if _, err := worker.reconcileMissingAssets(context.Background(), nil); err == nil {
				t.Fatal("reconcileMissingAssets() succeeded with a mismatched store marker")
			}
			if storage.missingUpsertCalls != 0 || storage.missingDeleteCalls != 0 {
				t.Fatalf("missing-asset DB writes with marker mismatch = upsert:%d delete:%d, want 0:0", storage.missingUpsertCalls, storage.missingDeleteCalls)
			}

			// 公開前の孤児の unlink。
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

			// 「無い」を観測した孤児の記録。ファイルを消して Stat が ErrNotExist になるようにする。
			if err := os.Remove(fullPath); err != nil {
				t.Fatal(err)
			}
			if _, err := worker.agedUnpublishedOrphans(context.Background(), time.Second, time.Second); err == nil {
				t.Fatal("agedUnpublishedOrphans() recorded a vanished orphan with a mismatched store marker")
			}
			if storage.orphanDeleteCalls != 0 {
				t.Fatalf("orphan DB deletes with marker mismatch = %d, want 0", storage.orphanDeleteCalls)
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

// 以下の 3 つは、unlink / Stat の後に目印が消えたとき、「無い」を事実として DB へ書かないことを固定する。
// 同期点は syscall の後ろに置く。手前の照合が先に捕まえてしまい、後ろの照合を外しても通るのを避けるため。

func TestDeleteMediaAssetWithholdsMarkDeletedAfterErrNotExistWhenMarkerDisappears(t *testing.T) {
	for _, state := range []string{"missing", "different store"} {
		t.Run(state, func(t *testing.T) {
			mediaDir := t.TempDir()
			if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
				t.Fatal(err)
			}
			// ファイルは最初から無い。unlink は ErrNotExist になる。
			mediaDelete := &markerTestMediaDelete{}
			worker := &DeleteReconcileWorker{MediaDelete: mediaDelete, MediaDir: mediaDir, MediaStoreName: "tokyo"}
			previous := beforeMediaAssetMarkDeleted
			beforeMediaAssetMarkDeleted = func(int64) { mismatchStoreMarker(t, mediaDir, state) }
			t.Cleanup(func() { beforeMediaAssetMarkDeleted = previous })
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

			worker.deleteMediaAsset(context.Background(), store.MediaDeleteAsset{
				ID: 44, RecordingID: 7, RelPath: "sites/tokyo/gone.ts", Kind: store.AssetKindOriginal,
			}, "trash")

			if mediaDelete.markDeletedCalls != 0 {
				t.Errorf("MarkDeleted calls = %d, want 0", mediaDelete.markDeletedCalls)
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
			}
		})
	}
}

func TestResolveDeletingAssetWithholdsDecisionWhenMarkerDisappearsAfterStat(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fileExists bool
	}{
		{name: "file absent", fileExists: false},
		{name: "file present", fileExists: true},
	} {
		for _, state := range []string{"missing", "different store"} {
			t.Run(tc.name+"/"+state, func(t *testing.T) {
				mediaDir := t.TempDir()
				if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
					t.Fatal(err)
				}
				relPath := "sites/tokyo/deleting.ts"
				if tc.fileExists {
					path := filepath.Join(mediaDir, filepath.FromSlash(relPath))
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				mediaDelete := &markerTestMediaDelete{}
				worker := &DeleteReconcileWorker{MediaDelete: mediaDelete, MediaDir: mediaDir, MediaStoreName: "tokyo"}
				previous := afterDeletingAssetStat
				afterDeletingAssetStat = func(string) { mismatchStoreMarker(t, mediaDir, state) }
				t.Cleanup(func() { afterDeletingAssetStat = previous })
				metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

				worker.resolveUnqualifiedDeletingAsset(context.Background(),
					store.MediaDeleteRevertCandidate{ID: 45, RecordingID: 7, RelPath: relPath}, time.Now(), time.Time{})

				if mediaDelete.markDeletedCalls != 0 || mediaDelete.revertCalls != 0 {
					t.Errorf("DB calls = MarkDeleted:%d RevertToActive:%d, want 0:0", mediaDelete.markDeletedCalls, mediaDelete.revertCalls)
				}
				if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
					t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
				}
			})
		}
	}
}

func TestOrphanAbsentAfterUnlinkWithholdsRecordWhenMarkerDisappears(t *testing.T) {
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
			// 再確認の後に別の誰かがファイルを消す。unlink は ErrNotExist になる。
			previousBefore := beforeOrphanFileUnlink
			beforeOrphanFileUnlink = func(string) {
				if err := os.Remove(fullPath); err != nil {
					t.Errorf("removing orphan before unlink: %v", err)
				}
			}
			previousAfter := afterOrphanFileUnlink
			afterOrphanFileUnlink = func(string) { mismatchStoreMarker(t, mediaDir, state) }
			t.Cleanup(func() { beforeOrphanFileUnlink = previousBefore; afterOrphanFileUnlink = previousAfter })
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

			worker.deleteOrphanFile(filepath.ToSlash(relPath), time.Second)

			if storage.orphanDeleteCalls != 0 {
				t.Fatalf("orphan DB deletes after ErrNotExist with marker mismatch = %d, want 0", storage.orphanDeleteCalls)
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
			}
		})
	}
}

type failingIngestTarget struct{ calls int }

func (f *failingIngestTarget) GetRecordSyncIngestTarget(context.Context, string, string) (store.RecordIngestTarget, error) {
	f.calls++
	return store.RecordIngestTarget{}, errors.New("stop after the storage probe")
}

// 起動時に probe を保留した worker は、目印の照合が通った最初の ingest で probe を走らせる。
// probe の失敗は固定せず、次のジョブが再試行する。
func TestIngestWorkerProbesStorageAfterMarkerCheckAndRetriesFailure(t *testing.T) {
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
		t.Fatal(err)
	}
	target := &failingIngestTarget{}
	worker := &IngestWorker{
		MirakcClients:  map[string]*mirakc.Client{"tokyo": nil},
		RecordTarget:   target,
		MediaDir:       mediaDir,
		MediaStoreName: "tokyo",
	}
	args := jobs.IngestJobArgs{Site: "tokyo", RecordID: "r1"}

	// probe は media_dir/sites を作る。通常ファイルが居座っていると失敗する。
	sitesPath := filepath.Join(mediaDir, "sites")
	if err := os.WriteFile(sitesPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := worker.Work(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "validating ingest storage") {
		t.Fatalf("Work() with a failing probe error = %v, want validating ingest storage", err)
	}
	if target.calls != 0 {
		t.Fatalf("ingest target lookups after a failed probe = %d, want 0", target.calls)
	}

	if err := os.Remove(sitesPath); err != nil {
		t.Fatal(err)
	}
	err = worker.Work(context.Background(), args)
	if err == nil || strings.Contains(err.Error(), "validating ingest storage") {
		t.Fatalf("Work() after the probe recovered error = %v, want the later lookup error", err)
	}
	if target.calls != 1 {
		t.Fatalf("ingest target lookups after the probe recovered = %d, want 1", target.calls)
	}
	if info, err := os.Stat(sitesPath); err != nil || !info.IsDir() {
		t.Fatalf("media_dir/sites after the probe = %v, %v, want a directory", info, err)
	}

	// 成功した後は probe を繰り返さない。sites を通常ファイルに戻しても probe は走らない。
	if err := os.RemoveAll(sitesPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sitesPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = worker.Work(context.Background(), args)
	if err == nil || strings.Contains(err.Error(), "validating ingest storage") {
		t.Fatalf("Work() after a successful probe error = %v, want the lookup error without a second probe", err)
	}
}

// 起動時に probe が成功していれば、最初の ingest で二重に走らせない。
func TestIngestWorkerSkipsStorageProbeAlreadyDoneAtStartup(t *testing.T) {
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "sites"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := &failingIngestTarget{}
	worker := &IngestWorker{
		MirakcClients:  map[string]*mirakc.Client{"tokyo": nil},
		RecordTarget:   target,
		MediaDir:       mediaDir,
		MediaStoreName: "tokyo",
		storageProbed:  true,
	}
	if err := worker.Work(context.Background(), jobs.IngestJobArgs{Site: "tokyo", RecordID: "r1"}); err == nil || strings.Contains(err.Error(), "validating ingest storage") {
		t.Fatalf("Work() error = %v, want the lookup error without a probe", err)
	}
	if target.calls != 1 {
		t.Fatalf("ingest target lookups = %d, want 1", target.calls)
	}
}

// 走査の結果を孤児候補として DB へ書く直前に、目印が消えていないことを照合する。
func TestOrphanCandidateRecordRechecksStoreMarkerAfterWalk(t *testing.T) {
	for _, state := range []string{"missing", "different store"} {
		t.Run(state, func(t *testing.T) {
			mediaDir := t.TempDir()
			if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
				t.Fatal(err)
			}
			stray := filepath.Join(mediaDir, "sites", "tokyo", "stray.ts")
			if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stray, []byte("stray"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(stray, old, old); err != nil {
				t.Fatal(err)
			}
			storage := &markerTestStorageObservation{onListKnown: func() { mismatchStoreMarker(t, mediaDir, state) }}
			worker := &DeleteReconcileWorker{Storage: storage, MediaDir: mediaDir, MediaStoreName: "tokyo"}
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

			if _, err := worker.reconcileOrphanCandidates(context.Background(), time.Second); err == nil {
				t.Fatal("reconcileOrphanCandidates() succeeded after the marker changed")
			}
			if storage.orphanUpsertCalls != 0 {
				t.Fatalf("orphan candidate writes after marker mismatch = %d, want 0", storage.orphanUpsertCalls)
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
			}
		})
	}
}

type erroringEncodedPublisher struct {
	store.MediaPublish
	calls int
}

func (p *erroringEncodedPublisher) PublishEncoded(context.Context, store.EncodedPublishInput, func() error) (bool, error) {
	p.calls++
	return false, errors.New("publisher reached")
}

// 本体の stage の後に目印が消えたら、字幕の temp も rel_path の lock file も作らない。
func TestPublishEncodedCreatesNoSubtitleTempOrLockWhenMarkerDisappearsAfterBodyStage(t *testing.T) {
	for _, state := range []string{"missing", "different store"} {
		t.Run(state, func(t *testing.T) {
			mediaDir := t.TempDir()
			if err := mediapath.WriteStoreMarker(mediaDir, "tokyo"); err != nil {
				t.Fatal(err)
			}
			scratch := t.TempDir()
			body := filepath.Join(scratch, "out.mp4")
			subtitle := filepath.Join(scratch, "out.vtt")
			for _, p := range []string{body, subtitle} {
				if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			relPath := "sites/tokyo/out.mp4"
			finalPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
			// 照合が効けば publisher に届く前に返る。
			publisher := &erroringEncodedPublisher{}
			w := &EncodeWorker{MediaDir: mediaDir, MediaStoreName: "tokyo", Publisher: publisher}
			previous := afterEncodeBodyStaged
			afterEncodeBodyStaged = func(string) { mismatchStoreMarker(t, mediaDir, state) }
			t.Cleanup(func() { afterEncodeBodyStaged = previous })
			metricBefore := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

			_, published, err := w.publishEncoded(context.Background(), encodePublishInput{
				relPath: relPath, finalPath: finalPath, scratchOut: body, subtitleOut: subtitle, withSubtitles: true,
			})
			if !errors.Is(err, mediapath.ErrStoreMarkerMismatch) || published {
				t.Fatalf("publishEncoded() = published:%t err:%v, want marker mismatch", published, err)
			}
			if got := testutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
			}
			// 本体の temp は discard で消える。残るのは字幕の temp と lock file のどちらも無い状態。
			if entries, err := os.ReadDir(filepath.Dir(finalPath)); err != nil || len(entries) != 0 {
				t.Errorf("entries beside the canonical path = %v (err %v), want none", entries, err)
			}
			rootEntries, err := os.ReadDir(mediaDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range rootEntries {
				if strings.HasPrefix(e.Name(), mediapath.MediaRelPathLockFilePrefix) {
					t.Errorf("lock file %q was created after the marker disappeared", e.Name())
				}
			}
		})
	}
}
