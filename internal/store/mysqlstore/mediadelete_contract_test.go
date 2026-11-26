package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestMediaDeleteStoreContract(t *testing.T) {
	contracttest.RunMediaDelete(t, newMediaDeleteContractFixture)
}

// mediaDeleteContractBoundStore は fixture の worker の置き場の名前である。
const mediaDeleteContractBoundStore = "tokyo"

// newMediaDeleteContractFixture は MySQL の実装と、RunMediaDelete が使う状態作りのフックを組み立てる。
// 状態作りは test 側の SQL で行う（store の契約ではない）。
//
//nolint:funlen // 契約の各フックを 1 か所に並べる表。分割すると、フックと状態作りの対応が追えなくなる。
func newMediaDeleteContractFixture(t *testing.T) contracttest.MediaDeleteFixture {
	t.Helper()
	db := mysqltest.SetupDB(t)
	zero := store.StoragePolicy{}
	bound := mediaDeleteContractBoundStore
	return contracttest.MediaDeleteFixture{
		Store:      NewMediaInStore(db, bound, store.NewStoragePolicy(nil, nil, bound)),
		BoundStore: bound,
		StoreWithDerived: func(derivedStore string) store.MediaDelete {
			return NewMediaInStore(db, bound, store.NewStoragePolicy(nil, nil, derivedStore))
		},
		StoreWithPlacement: func(rules []store.OriginalStoreRule, derivedStore string, enabled bool) store.MediaDelete {
			return NewMediaInStore(db, bound, store.NewStoragePolicy(rules, nil, derivedStore).WithPlacementEnabled(enabled))
		},
		Serving: NewMediaInStore(db, bound, zero),
		Catalog: NewCatalog(db, []string{store.DefaultSite}, bound),
		MarkMissing: func(t *testing.T, copyID int64) {
			t.Helper()
			execFixture(t, db, `
INSERT INTO missing_media_assets (media_asset_copy_id) VALUES (?)
ON DUPLICATE KEY UPDATE media_asset_copy_id = media_asset_copy_id`, copyID)
		},
		NewOriginal: func(t *testing.T) (int64, int64) {
			t.Helper()
			recordingID := tsScanContractRecording(t, db, "media delete contract original")
			return recordingID, mediaDeleteAsset(t, db, recordingID, store.AssetKindOriginal, nil, bound)
		},
		NewOldGeneration: func(t *testing.T) (int64, int64) {
			t.Helper()
			recordingID := tsScanContractRecording(t, db, "media delete contract old generation")
			assetID := mediaDeleteAsset(t, db, recordingID, "thumbnail", nil, "")
			oldCopy := mediaDeleteCopy(t, db, assetID, bound, fmt.Sprintf("sites/%s/media-delete-contract/%d-g0.jpg", store.DefaultSite, recordingID), 0, "active")
			execFixture(t, db, `UPDATE media_assets SET derived_generation = 1 WHERE id = ?`, assetID)
			current := mediaDeleteCopy(t, db, assetID, bound, fmt.Sprintf("sites/%s/media-delete-contract/%d-g1.jpg", store.DefaultSite, recordingID), 1, "active")
			return oldCopy, current
		},
		NewRetainedEncoded: func(t *testing.T, recordingID int64) int64 {
			t.Helper()
			execFixture(t, db, `UPDATE recording_processing_policy SET encode_profiles = JSON_ARRAY('p2') WHERE recording_id = ?`, recordingID)
			profile := "p2"
			return mediaDeleteAsset(t, db, recordingID, store.AssetKindEncoded, &profile, bound)
		},
		SetAssetState: func(t *testing.T, assetID int64, state string) {
			t.Helper()
			execFixture(t, db, `
UPDATE media_asset_copies
SET state = ?, deleted_at = CASE WHEN ? = 'deleted' THEN COALESCE(deleted_at, CURRENT_TIMESTAMP(6)) ELSE NULL END
WHERE media_asset_id = ?`, state, state, assetID)
		},
		NewUntilEncodedOriginal: func(t *testing.T) (int64, int64) {
			t.Helper()
			recordingID := tsScanContractRecording(t, db, "media delete contract until-encoded")
			originalID := mediaDeleteAsset(t, db, recordingID, store.AssetKindOriginal, nil, bound)
			profile := "h264"
			mediaDeleteAsset(t, db, recordingID, store.AssetKindEncoded, &profile, bound)
			mediaDeleteAsset(t, db, recordingID, store.AssetKindThumbnail, nil, bound)
			mediaDeleteAsset(t, db, recordingID, store.AssetKindSeekTiles, nil, bound)
			execFixture(t, db, `
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
SELECT id, size_bytes FROM media_assets WHERE id = ?`, originalID)
			execFixture(t, db, `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES (?, 'until_encoded', JSON_ARRAY('h264'), FALSE)`, recordingID)
			return recordingID, originalID
		},
		AssetOfKind: func(t *testing.T, recordingID int64, kind string) int64 {
			t.Helper()
			var assetID int64
			if err := db.QueryRowContext(context.Background(), `SELECT id FROM media_assets WHERE recording_id = ? AND kind = ?`, recordingID, kind).Scan(&assetID); err != nil {
				t.Fatalf("reading %s asset of recording %d: %v", kind, recordingID, err)
			}
			return assetID
		},
		BeginUntilEncodedInvalidation: func(t *testing.T, recordingID int64) func() {
			t.Helper()
			return beginFixtureTx(t, db, func(tx *sql.Tx) error {
				ctx := context.Background()
				if _, err := tx.ExecContext(ctx, `UPDATE recordings SET updated_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, recordingID); err != nil {
					return fmt.Errorf("locking recording for rebuild request: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `
INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
VALUES (?, 'h264', JSON_ARRAY(JSON_OBJECT('startMs', 0, 'endMs', 60000)))`, recordingID); err != nil {
					return fmt.Errorf("seeding pending rebuild request: %w", err)
				}
				return nil
			})
		},
		BeginCopyDeletion: func(t *testing.T, copyID int64) func() {
			t.Helper()
			return beginFixtureTx(t, db, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(context.Background(), `
UPDATE media_asset_copies SET state = 'deleted', deleted_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, copyID)
				return err
			})
		},
		NewDeletedAsset: func(t *testing.T) (int64, int64) {
			t.Helper()
			recordingID := tsScanContractRecording(t, db, "media delete contract deleted")
			assetID := mediaDeleteAsset(t, db, recordingID, store.AssetKindOriginal, nil, bound)
			execFixture(t, db, `
UPDATE media_asset_copies
SET state = 'deleted', deleted_at = CURRENT_TIMESTAMP(6)
WHERE media_asset_id = ?`, assetID)
			return recordingID, assetID
		},
		NewRemovedEncoded: func(t *testing.T) (int64, int64) {
			t.Helper()
			recordingID := tsScanContractRecording(t, db, "media delete contract removed encoded")
			mediaDeleteAsset(t, db, recordingID, store.AssetKindOriginal, nil, bound)
			execFixture(t, db, `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES (?, 'always', JSON_ARRAY(), FALSE)`, recordingID)
			profile := "h264"
			assetID := mediaDeleteAsset(t, db, recordingID, store.AssetKindEncoded, &profile, bound)
			execFixture(t, db, `INSERT INTO encoded_asset_removal_requests (recording_id, profile) VALUES (?, 'h264')`, recordingID)
			return recordingID, assetID
		},
		TrashRecording: func(t *testing.T, recordingID int64, deletedAt time.Time, purgeRequested bool) {
			t.Helper()
			execFixture(t, db, `UPDATE recordings SET deleted_at = ? WHERE id = ?`, deletedAt, recordingID)
			if purgeRequested {
				execFixture(t, db, `INSERT INTO recording_purge_requests (recording_id) VALUES (?)`, recordingID)
			}
		},
		CopyID: func(t *testing.T, assetID int64) int64 {
			t.Helper()
			var copyID int64
			if err := db.QueryRowContext(context.Background(), `SELECT id FROM media_asset_copies WHERE media_asset_id = ? AND store = ?`, assetID, bound).Scan(&copyID); err != nil {
				t.Fatalf("getting copy ID for media asset %d in %s: %v", assetID, bound, err)
			}
			return copyID
		},
		CopyState: func(t *testing.T, copyID int64) string {
			t.Helper()
			var state string
			if err := db.QueryRowContext(context.Background(), `SELECT state FROM media_asset_copies WHERE id = ?`, copyID).Scan(&state); err != nil {
				t.Fatalf("reading state of copy %d: %v", copyID, err)
			}
			return state
		},
		MoveCopy: func(t *testing.T, assetID int64, from, to string) int64 {
			t.Helper()
			execFixture(t, db, `UPDATE media_asset_copies SET store = ? WHERE media_asset_id = ? AND store = ?`, to, assetID, from)
			var copyID int64
			if err := db.QueryRowContext(context.Background(), `SELECT id FROM media_asset_copies WHERE media_asset_id = ? AND store = ?`, assetID, to).Scan(&copyID); err != nil {
				t.Fatalf("getting moved copy of asset %d in %s: %v", assetID, to, err)
			}
			return copyID
		},
		AdvanceDerivedGeneration: func(t *testing.T, assetID int64) {
			t.Helper()
			execFixture(t, db, `UPDATE media_assets SET derived_generation = derived_generation + 1 WHERE id = ?`, assetID)
		},
		AddCurrentCopy: func(t *testing.T, assetID int64, storeName string) int64 {
			t.Helper()
			return fixtureID(t, execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
SELECT id, ?, ?, derived_generation, 'active' FROM media_assets WHERE id = ?`,
				storeName, fmt.Sprintf("sites/%s/media-delete-contract/current/%d/%s", store.DefaultSite, assetID, storeName), assetID))
		},
		RemovalRequestExists: func(t *testing.T, recordingID int64, profile string) bool {
			t.Helper()
			var exists bool
			if err := db.QueryRowContext(context.Background(), `
SELECT EXISTS (SELECT 1 FROM encoded_asset_removal_requests WHERE recording_id = ? AND profile = ?)`, recordingID, profile).Scan(&exists); err != nil {
				t.Fatalf("reading encoded asset removal request: %v", err)
			}
			return exists
		},
		WaitForLockWaiter: func(t *testing.T, operation string) {
			t.Helper()
			waitForMediaDeleteLockWaiter(t, db, operation)
		},
	}
}

// mediaDeleteAsset は録画の kind の asset を 1 件作り、storeName の active コピー（gen 0）を持たせる。
// storeName が "" ならコピーは作らない。profile は encoded のときだけ渡す。
func mediaDeleteAsset(t *testing.T, db *sql.DB, recordingID int64, kind string, profile *string, storeName string) int64 {
	t.Helper()
	var profileArg any
	profileKey := kind
	if profile != nil {
		profileArg = *profile
		profileKey += "-" + *profile
	}
	assetID := fixtureID(t, execFixture(t, db, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES (?, ?, ?, 1, 0)`, recordingID, kind, profileArg))
	if storeName != "" {
		mediaDeleteCopy(t, db, assetID, storeName, fmt.Sprintf("sites/%s/media-delete-contract/%d/%s", store.DefaultSite, recordingID, profileKey), 0, "active")
	}
	return assetID
}

// mediaDeleteCopy は assetID のコピー行を 1 つ作り、その ID を返す。
func mediaDeleteCopy(t *testing.T, db *sql.DB, assetID int64, storeName, relPath string, generation int64, state string) int64 {
	t.Helper()
	return fixtureID(t, execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES (?, ?, ?, ?, ?)`, assetID, storeName, relPath, generation, state))
}

// beginFixtureTx は、fn を行う tx を開いたまま返す。返す関数が commit する。tx は後始末で閉じる。
func beginFixtureTx(t *testing.T, db *sql.DB, fn func(*sql.Tx) error) func() {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("beginning fixture transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if err := fn(tx); err != nil {
		t.Fatalf("writing fixture in transaction: %v", err)
	}
	return func() {
		if err := tx.Commit(); err != nil {
			t.Errorf("committing fixture transaction: %v", err)
		}
	}
}

// waitForMediaDeleteLockWaiter は、録画の行かコピーの行の FOR UPDATE で待っている文があるまで待つ。
func waitForMediaDeleteLockWaiter(t *testing.T, db *sql.DB, operation string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM performance_schema.data_lock_waits w
    JOIN performance_schema.data_locks l
      ON l.ENGINE = w.ENGINE AND l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
    JOIN performance_schema.events_statements_current st
      ON st.THREAD_ID = w.REQUESTING_THREAD_ID
    WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME IN ('recordings', 'media_asset_copies')
      AND st.SQL_TEXT LIKE '%FOR UPDATE%'
)`).Scan(&waiting); err != nil {
			t.Fatalf("checking the lock wait for %s: %v", operation, err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s did not wait on a FOR UPDATE of a recording or copy row: %v", operation, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
