package mysqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestTSScanStoreContract(t *testing.T) {
	contracttest.RunTSScan(t, newTSScanContractFixture)
}

// newTSScanContractFixture は MySQL の実装と、RunTSScan が使う状態作りのフックを組み立てる。
// 状態作りは test 側の SQL で行う（store の契約ではない）。
func newTSScanContractFixture(t *testing.T) contracttest.TSScanFixture {
	t.Helper()
	db := mysqltest.SetupDB(t)
	zero := store.StoragePolicy{}
	media := NewMediaInStore(db, "tokyo", zero)
	inPlaceEvent := mediaContractEventSeq.Add(1)
	return contracttest.TSScanFixture{
		Store:             media,
		Catalog:           NewCatalog(db, []string{store.DefaultSite}, "tokyo"),
		InPlace:           media,
		OtherStoreInPlace: NewMediaInStore(db, "other-store", zero),
		NewOriginal: func(t *testing.T, sizeBytes int64, trashed bool) (int64, int64) {
			t.Helper()
			recordingID := tsScanContractRecording(t, db, "TS scan store contract fixture")
			assetID := fixtureID(t, execFixture(t, db, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES (?, 'original', NULL, ?, 0)`, recordingID, sizeBytes))
			execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES (?, 'tokyo', ?, 0, 'active')`, assetID, fmt.Sprintf("sites/%s/ts-scan-contract/%d.ts", store.DefaultSite, recordingID))
			if trashed {
				execFixture(t, db, `UPDATE recordings SET deleted_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, recordingID)
			}
			return recordingID, assetID
		},
		NewEncodedOnlyRecording: func(t *testing.T) (int64, int64) {
			t.Helper()
			recordingID := tsScanContractRecording(t, db, "TS scan store contract encoded-only fixture")
			assetID := fixtureID(t, execFixture(t, db, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES (?, 'encoded', 'h264', 1, 0)`, recordingID))
			execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES (?, 'tokyo', ?, 0, 'active')`, assetID, fmt.Sprintf("sites/%s/ts-scan-contract/%d.mp4", store.DefaultSite, recordingID))
			return recordingID, assetID
		},
		MarkOriginalMissing: func(t *testing.T, assetID int64) {
			t.Helper()
			execFixture(t, db, `
INSERT INTO missing_media_assets (media_asset_copy_id)
SELECT id FROM media_asset_copies WHERE media_asset_id = ? AND store = 'tokyo'
ON DUPLICATE KEY UPDATE media_asset_copy_id = media_asset_copy_id`, assetID)
		},
		SetAssetSize: func(t *testing.T, assetID, sizeBytes int64) {
			t.Helper()
			execFixture(t, db, `UPDATE media_assets SET size_bytes = ? WHERE id = ?`, sizeBytes, assetID)
		},
		SetAssetState: func(t *testing.T, assetID int64, state string) {
			t.Helper()
			execFixture(t, db, `
UPDATE media_asset_copies
SET state = ?, deleted_at = CASE WHEN ? = 'deleted' THEN COALESCE(deleted_at, CURRENT_TIMESTAMP(6)) ELSE NULL END
WHERE media_asset_id = ?`, state, state, assetID)
		},
		SetScannedSize: func(t *testing.T, assetID, scannedSizeBytes int64) {
			t.Helper()
			execFixture(t, db, `
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes) VALUES (?, ?) AS incoming
ON DUPLICATE KEY UPDATE scanned_size_bytes = incoming.scanned_size_bytes`, assetID, scannedSizeBytes)
		},
		SeedSnapshot: func(t *testing.T, assetID int64, snapshot contracttest.TSScanSnapshot) {
			t.Helper()
			for _, stat := range snapshot.Stats {
				execFixture(t, db, `
INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled, pid_type)
VALUES (?, ?, ?, ?, ?, ?, ?)`, assetID, stat.Pid, stat.Packets, stat.Drops, stat.Errors, stat.Scrambled, optionalValue(stat.PidType))
			}
			for _, position := range snapshot.Positions {
				execFixture(t, db, `
INSERT INTO drop_positions (media_asset_id, byte_offset, pid, elapsed_ms) VALUES (?, ?, ?, ?)`,
					assetID, position.ByteOffset, position.Pid, optionalValue(position.ElapsedMs))
			}
			if snapshot.ScannedSizeBytes >= 0 {
				execFixture(t, db, `INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes) VALUES (?, ?)`,
					assetID, snapshot.ScannedSizeBytes)
			}
		},
		SetAssetSHA256: func(t *testing.T, assetID int64) {
			t.Helper()
			execFixture(t, db, `UPDATE media_assets SET sha256 = ? WHERE id = ?`, bytes.Repeat([]byte{0xab}, 32), assetID)
		},
		NewInPlaceOriginalInput: func(t *testing.T, relPath string) store.InPlaceInput {
			t.Helper()
			return store.InPlaceInput{
				Recording: store.InPlaceRecording{
					Source: "manual", Site: store.DefaultSite, NetworkID: 32678, ServiceID: 5168,
					EventID: inPlaceEvent, ServiceName: "TS scan in-place fixture",
					ChannelType: "GR", Channel: "27", Title: "TS scan in-place fixture",
					ProgramStartAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ProgramDurationMs: 60_000, Status: "finished",
				},
				Assets: []store.InPlaceAsset{{Kind: store.AssetKindOriginal, RelPath: relPath, SizeBytes: 188}},
			}
		},
		BeginAssetStateChange: func(t *testing.T, assetID int64, state string) func() {
			return beginTSScanAssetStateChange(t, db, assetID, state)
		},
		WaitForLockWaiter: func(t *testing.T, operation string) {
			waitForTSScanAssetLock(t, db, operation)
		},
	}
}

// tsScanContractRecording は RunTSScan の原本・encoded の録画を 1 件作る。
func tsScanContractRecording(t *testing.T, db *sql.DB, title string) int64 {
	t.Helper()
	genres, genreLv1, err := recordingQueryGenres(nil)
	if err != nil {
		t.Fatalf("encoding fixture genres: %v", err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return fixtureID(t, execFixture(t, db, `
INSERT INTO recordings (site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, genres, is_free, program_start_at, program_duration_ms, status, source, genre_lv1)
VALUES (?, 32678, 5168, ?, 'TS scan contract service', 'GR', 'TS scan contract channel',
    ?, ?, TRUE, ?, 60000, 'finished', 'manual', ?)`,
		store.DefaultSite, mediaContractEventSeq.Add(1), title, genres, start, genreLv1))
}

// beginTSScanAssetStateChange は資産の行を排他で取った tx で状態を変え、commit 前の状態を保持する。
// 資産の行を先に取ってから copy を更新するので、待つ側は資産の行か copy の行で止まる。
func beginTSScanAssetStateChange(t *testing.T, db *sql.DB, assetID int64, state string) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("beginning media asset state change: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(ctx, `SELECT id FROM media_assets WHERE id = ? FOR UPDATE`, assetID); err != nil {
		t.Fatalf("locking media asset %d: %v", assetID, err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE media_asset_copies
SET state = ?, deleted_at = CASE WHEN ? = 'deleted' THEN COALESCE(deleted_at, CURRENT_TIMESTAMP(6)) ELSE NULL END
WHERE media_asset_id = ?`, state, state, assetID); err != nil {
		t.Fatalf("setting media asset %d and copy state to %s: %v", assetID, state, err)
	}
	return func() {
		if err := tx.Commit(); err != nil {
			t.Errorf("committing media asset state change: %v", err)
		}
	}
}

// waitForTSScanAssetLock は、資産の行か copy の行のロックで待つ文が FOR UPDATE であるまで待つ。
// 待ちの有無だけでは、ロックを取らずに判定する実装を区別できないので、待っている文まで見る。
func waitForTSScanAssetLock(t *testing.T, db *sql.DB, operation string) {
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
    WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME IN ('media_assets', 'media_asset_copies')
      AND st.SQL_TEXT LIKE '%media_assets%' AND st.SQL_TEXT LIKE '%FOR UPDATE%'
)`).Scan(&waiting); err != nil {
			t.Fatalf("checking the media asset lock wait: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s did not wait on a FOR UPDATE of the media asset: %v", operation, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
