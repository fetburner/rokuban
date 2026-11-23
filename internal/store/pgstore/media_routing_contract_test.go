package pgstore

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func configureMediaRoutingFixture(pool *pgxpool.Pool, fixture *contracttest.MediaFixture) {
	media := testMedia(pool, "tokyo", "")
	fixture.WriterState = media
	fixture.OpenTransfer = func(name string, policy store.StoragePolicy) store.StorageTransfer {
		return NewMediaInStore(pool, name, policy)
	}
	fixture.SetCopyCreatedAt = func(t *testing.T, copyID int64, createdAt time.Time) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `UPDATE media_asset_copies SET created_at = $2 WHERE id = $1`, copyID, createdAt); err != nil {
			t.Fatalf("setting copy creation time: %v", err)
		}
	}
	policy := store.NewStoragePolicy(nil, []string{"tokyo", "osaka"}, "remote")
	fixture.OpenStore = func(name string) contracttest.MediaStoreScope {
		scoped := NewMediaUnboundWithPolicy(pool, policy)
		if name != "" {
			scoped = NewMediaInStore(pool, name, policy)
		}
		return contracttest.MediaStoreScope{Locator: scoped, Storage: scoped, StorageSync: scoped, Publisher: scoped, Reclaim: scoped}
	}
	fixture.SeedOwnedTransferPath = func(t *testing.T, copyID int64, relPath string) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `
INSERT INTO media_asset_transfer_objects (copy_id, rel_path) VALUES ($1, $2)`, copyID, relPath); err != nil {
			t.Fatalf("seeding transfer ownership: %v", err)
		}
	}
	fixture.WaitForPublishBlocked = func(t *testing.T) {
		t.Helper()
		waitForContractLockWaiter(t, pool, "PublishThumbnail")
	}
	fixture.NewIngestStoreChooser = func(policy store.StoragePolicy) store.IngestStoreChooser {
		return NewMediaUnboundWithPolicy(pool, policy)
	}
	fixture.AddCopy = func(t *testing.T, assetID int64, storeName, relPath string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(t.Context(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
SELECT id, $2, $3, derived_generation, 'active' FROM media_assets WHERE id = $1
RETURNING id`, assetID, storeName, relPath).Scan(&id); err != nil {
			t.Fatalf("seeding active copy: %v", err)
		}
		return id
	}
	fixture.SetIngestCheckpoint = func(t *testing.T, recordingID, writerGeneration int64) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), `
INSERT INTO recording_ingest_checkpoints (recording_id, lease_owner, lease_expires_at, writer_generation)
VALUES ($1, '30ea6174-d005-4d9b-b029-7d35a2a84127', now() + interval '1 hour', $2)`, recordingID, writerGeneration); err != nil {
			t.Fatalf("seeding ingest checkpoint: %v", err)
		}
	}
	fixture.SetIngestDisplayPath = func(t *testing.T, recordingID int64, relPath string) {
		t.Helper()
		if err := media.UpsertIngestProgress(t.Context(), store.IngestProgress{
			RecordingID: recordingID, WrittenBytes: 17, TempRelPath: relPath, TempGeneration: 5,
		}); err != nil {
			t.Fatalf("seeding ingest display path: %v", err)
		}
	}
}
