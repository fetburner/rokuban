package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

var mediaContractEventSeq atomic.Int32

func TestMediaServingSelectionContract(t *testing.T) {
	contracttest.RunMediaServingSelection(t, newMediaContractFixture)
}

func TestMediaPublishContract(t *testing.T) {
	t.Parallel()
	contracttest.RunMediaPublish(t, newMediaContractFixture)
}

// newMediaContractFixture は MySQL の実装と、RunMediaPublish が使う状態作りのフックを組み立てる。
// 状態作りは test 側の SQL で行う（store の契約ではない）。Storage・Precheck・Reader は RunMediaPublish が使わないので nil のまま。
func newMediaContractFixture(t *testing.T) contracttest.MediaFixture {
	db := mysqltest.SetupDB(t)
	zero := store.StoragePolicy{}
	routing := store.NewStoragePolicy(nil, []string{"tokyo", "osaka"}, "remote")
	fixture := contracttest.MediaFixture{
		Serving:   NewMediaInStore(db, "tokyo", zero),
		Reconcile: NewMediaInStore(db, "tokyo", zero),
		Publisher: NewMediaInStore(db, "tokyo", zero),
		Query:     NewRecordingQueries(db, "tokyo"),
		Catalog:   NewCatalog(db, []string{store.DefaultSite}, "tokyo"),
		Chapters:  NewChapterEdits(db),
		Site:      store.DefaultSite,
		OpenStore: func(name string) contracttest.MediaStoreScope {
			scoped := NewMediaUnboundWithPolicy(db, routing)
			if name != "" {
				scoped = NewMediaInStore(db, name, routing)
			}
			return contracttest.MediaStoreScope{Publisher: scoped, Reclaim: scoped}
		},
		NewRecording: func(t *testing.T) int64 {
			t.Helper()
			genres, genreLv1, err := recordingQueryGenres(nil)
			if err != nil {
				t.Fatalf("encoding fixture genres: %v", err)
			}
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			return fixtureID(t, execFixture(t, db, `
INSERT INTO recordings (site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, genres, is_free, program_start_at, program_duration_ms, status, source, genre_lv1)
VALUES (?, 32678, 5168, ?, 'media contract service', 'GR', 'media contract channel',
    'store media contract fixture', ?, TRUE, ?, 60000, 'finished', 'manual', ?)`,
				store.DefaultSite, mediaContractEventSeq.Add(1), genres, start, genreLv1))
		},
	}
	configureMediaPublishFixtureMySQL(db, &fixture)
	return fixture
}

// configureMediaPublishFixtureMySQL は RunMediaPublish が使う状態作りのフックを SQL で実装する。
func configureMediaPublishFixtureMySQL(db *sql.DB, f *contracttest.MediaFixture) {
	f.SetRecordingTrashed = func(t *testing.T, recordingID int64) {
		t.Helper()
		execFixture(t, db, `UPDATE recordings SET deleted_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, recordingID)
	}
	f.SetRecordingPurged = func(t *testing.T, recordingID int64) {
		t.Helper()
		execFixture(t, db, `UPDATE recordings SET deleted_at = COALESCE(deleted_at, CURRENT_TIMESTAMP(6)), purged_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, recordingID)
	}
	f.SetEncodeProfiles = func(t *testing.T, recordingID int64, profiles []string) {
		t.Helper()
		if profiles == nil {
			profiles = []string{}
		}
		raw, err := json.Marshal(profiles)
		if err != nil {
			t.Fatalf("encoding profiles: %v", err)
		}
		execFixture(t, db, `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES (?, 'always', CAST(? AS JSON), FALSE) AS incoming
ON DUPLICATE KEY UPDATE encode_profiles = incoming.encode_profiles`, recordingID, string(raw))
	}
	f.BeginEncodeAttempt = func(t *testing.T, recordingID int64, profile string) int32 {
		t.Helper()
		execFixture(t, db, `
INSERT INTO recording_encode_attempts (recording_id, profile, state, error, attempted_at, attempt_count)
VALUES (?, ?, 'running', NULL, CURRENT_TIMESTAMP(6), 1)
ON DUPLICATE KEY UPDATE recording_id = recording_id`, recordingID, profile)
		var attempt int32
		if err := db.QueryRow(`SELECT attempt_count FROM recording_encode_attempts WHERE recording_id = ? AND profile = ?`,
			recordingID, profile).Scan(&attempt); err != nil {
			t.Fatalf("reading encode attempt for recording %d profile %q: %v", recordingID, profile, err)
		}
		return attempt
	}
	f.SetIngestProgress = func(t *testing.T, recordingID int64, written int64, expected *int64) {
		t.Helper()
		execFixture(t, db, `
INSERT INTO recording_ingest_progress (recording_id, written_bytes, expected_bytes, temp_rel_path, temp_generation, temp_store, observed_at)
VALUES (?, ?, ?, ?, 1, 'tokyo', CURRENT_TIMESTAMP(6)) AS incoming
ON DUPLICATE KEY UPDATE written_bytes = incoming.written_bytes, expected_bytes = incoming.expected_bytes,
    temp_rel_path = incoming.temp_rel_path, temp_generation = incoming.temp_generation, observed_at = CURRENT_TIMESTAMP(6)`,
			recordingID, written, expected, fmt.Sprintf("sites/%s/.rokuban-ingest-contract", store.DefaultSite))
	}
	f.NewInPlaceInput = func(t *testing.T, relPath string) store.InPlaceInput {
		t.Helper()
		return store.InPlaceInput{
			Recording: store.InPlaceRecording{
				Source: "manual", Site: store.DefaultSite, NetworkID: 32678, ServiceID: 5168,
				EventID: mediaContractEventSeq.Add(1), ServiceName: "in-place contract fixture",
				ChannelType: "GR", Channel: "27", Title: "in-place contract fixture",
				ProgramStartAt: time.Now().UTC().Truncate(time.Second), ProgramDurationMs: 60_000, Status: "finished",
			},
			Assets: []store.InPlaceAsset{{Kind: store.AssetKindOriginal, RelPath: relPath, SizeBytes: 1}},
		}
	}
	f.NewAsset = func(t *testing.T, recordingID int64, kind string, profile *string, relPath, state string) int64 {
		t.Helper()
		res := execFixture(t, db, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES (?, ?, ?, 1, 0)`, recordingID, kind, optionalValue(profile))
		assetID := fixtureID(t, res)
		execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, deleted_at)
VALUES (?, 'tokyo', ?, 0, ?, CASE WHEN ? = 'deleted' THEN CURRENT_TIMESTAMP(6) ELSE NULL END)`,
			assetID, relPath, state, state)
		return assetID
	}
	f.CopyID = func(t *testing.T, assetID int64) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRow(`SELECT id FROM media_asset_copies WHERE media_asset_id = ? AND store = 'tokyo' ORDER BY id LIMIT 1`, assetID).Scan(&id); err != nil {
			t.Fatalf("reading copy of asset %d: %v", assetID, err)
		}
		return id
	}
	f.AddCopy = func(t *testing.T, assetID int64, storeName, relPath string) int64 {
		t.Helper()
		res := execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
SELECT id, ?, ?, derived_generation, 'active' FROM media_assets WHERE id = ?`, storeName, relPath, assetID)
		return fixtureID(t, res)
	}
	// ServingIn の "" は置き場で絞らない読み手（NewMediaUnboundWithPolicy）で、Postgres の testMedia(pool, "", "") と同じ構成。
	// servingStores は nil のままなので、派生物の配信はすべての置き場を読める。
	f.ServingIn = func(storeName string) store.MediaServing {
		if storeName == "" {
			return NewMediaUnboundWithPolicy(db, store.StoragePolicy{})
		}
		return NewMediaInStore(db, storeName, store.StoragePolicy{})
	}
	f.SetCopyState = func(t *testing.T, copyID int64, state string) {
		t.Helper()
		execFixture(t, db, `UPDATE media_asset_copies SET state = ?, deleted_at = NULL WHERE id = ?`, state, copyID)
	}
	f.SetCopyCreatedAt = func(t *testing.T, copyID int64, createdAt time.Time) {
		t.Helper()
		execFixture(t, db, `UPDATE media_asset_copies SET created_at = ? WHERE id = ?`, createdAt, copyID)
	}
	f.AddTombstoneCopy = func(t *testing.T, assetID int64, storeName, relPath string, generation int64) int64 {
		t.Helper()
		return fixtureID(t, execFixture(t, db, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, deleted_at)
VALUES (?, ?, ?, ?, 'deleted', CURRENT_TIMESTAMP(6))`, assetID, storeName, relPath, generation))
	}
	f.SetCopyMissing = func(t *testing.T, copyID int64, missing bool) {
		t.Helper()
		if missing {
			execFixture(t, db, `
INSERT INTO missing_media_assets (media_asset_copy_id) VALUES (?) AS incoming
ON DUPLICATE KEY UPDATE media_asset_copy_id = incoming.media_asset_copy_id`, copyID)
			return
		}
		execFixture(t, db, `DELETE FROM missing_media_assets WHERE media_asset_copy_id = ?`, copyID)
	}
	f.SeedOwnedTransferPath = func(t *testing.T, copyID int64, relPath string) {
		t.Helper()
		execFixture(t, db, `INSERT INTO media_asset_transfer_objects (copy_id, rel_path) VALUES (?, ?)`, copyID, relPath)
	}
	f.ExpireIngestLease = func(t *testing.T, recordingID int64) {
		t.Helper()
		execFixture(t, db, `UPDATE recording_ingest_checkpoints SET lease_expires_at = CURRENT_TIMESTAMP(6) - INTERVAL 1 SECOND WHERE recording_id = ?`, recordingID)
	}
	configureChapterFixtureMySQL(db, f)
}

// configureChapterFixtureMySQL はチャプターの区間・版の削除・作り直し要求の状態作りを SQL で実装する。
func configureChapterFixtureMySQL(db *sql.DB, f *contracttest.MediaFixture) {
	f.SetChapterSpans = func(t *testing.T, recordingID int64, spans []chapters.Span) {
		t.Helper()
		ctx := context.Background()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("beginning chapter fixture tx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		writeChapterSpansTx(t, tx, recordingID, spans)
		if err := tx.Commit(); err != nil {
			t.Fatalf("committing chapter fixture: %v", err)
		}
	}
	f.BeginChapterEdit = func(t *testing.T, recordingID int64, spans []chapters.Span) func() {
		t.Helper()
		ctx := context.Background()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("beginning chapter edit tx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		if _, err := tx.ExecContext(ctx, `SELECT id FROM recordings WHERE id = ? FOR UPDATE`, recordingID); err != nil {
			t.Fatalf("locking recording: %v", err)
		}
		writeChapterSpansTx(t, tx, recordingID, spans)
		return func() {
			if err := tx.Commit(); err != nil {
				t.Errorf("committing chapter edit tx: %v", err)
			}
		}
	}
	f.BeginEncodedRemoval = func(t *testing.T, recordingID int64, profile string) func() {
		t.Helper()
		ctx := context.Background()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("beginning encoded removal tx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		if _, err := tx.ExecContext(ctx, `SELECT id FROM recordings WHERE id = ? FOR UPDATE`, recordingID); err != nil {
			t.Fatalf("locking recording: %v", err)
		}
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT encode_profiles FROM recording_processing_policy WHERE recording_id = ? FOR UPDATE`, recordingID).Scan(&raw); err != nil {
			t.Fatalf("locking encode policy: %v", err)
		}
		var profiles []string
		if err := json.Unmarshal(raw, &profiles); err != nil {
			t.Fatalf("decoding encode profiles: %v", err)
		}
		kept := []string{}
		for _, p := range profiles {
			if p != profile {
				kept = append(kept, p)
			}
		}
		next, _ := json.Marshal(kept)
		if _, err := tx.ExecContext(ctx, `UPDATE recording_processing_policy SET encode_profiles = CAST(? AS JSON) WHERE recording_id = ?`, string(next), recordingID); err != nil {
			t.Fatalf("removing encode profile: %v", err)
		}
		return func() {
			if err := tx.Commit(); err != nil {
				t.Errorf("committing encoded removal tx: %v", err)
			}
		}
	}
	f.LoseTransactionDuringPublish = func(t *testing.T) {
		t.Helper()
		killOpenTransactions(t, db)
	}
	f.WaitForPublishBlocked = func(t *testing.T) {
		t.Helper()
		waitForLockWaiter(t, db, "publication")
	}
	f.SetEncodedAssetRebuildRequest = func(t *testing.T, recordingID int64, profile string, keep []chapters.Range) {
		t.Helper()
		raw, err := json.Marshal(keep)
		if err != nil {
			t.Fatalf("encoding rebuild keep: %v", err)
		}
		execFixture(t, db, `
INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep) VALUES (?, ?, CAST(? AS JSON)) AS incoming
ON DUPLICATE KEY UPDATE requested_keep = incoming.requested_keep`, recordingID, profile, string(raw))
	}
	f.SetCMRanges = func(t *testing.T, recordingID int64, ranges []chapters.Range) {
		t.Helper()
		raw, err := json.Marshal(nonNilRanges(ranges))
		if err != nil {
			t.Fatalf("encoding CM ranges: %v", err)
		}
		execFixture(t, db, `
INSERT INTO recording_cm_detections (recording_id, cm_ranges) VALUES (?, CAST(? AS JSON)) AS incoming
ON DUPLICATE KEY UPDATE cm_ranges = incoming.cm_ranges`, recordingID, string(raw))
	}
	f.SetMediaAssetKeepRanges = func(t *testing.T, assetID int64, keep []chapters.Range) {
		t.Helper()
		raw, err := json.Marshal(keep)
		if err != nil {
			t.Fatalf("encoding keep ranges: %v", err)
		}
		execFixture(t, db, `
INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES (?, CAST(? AS JSON)) AS incoming
ON DUPLICATE KEY UPDATE keep_ranges = incoming.keep_ranges`, assetID, string(raw))
	}
}

// writeChapterSpansTx は録画のチャプターを所有にして区間を書き直す。呼び出し側の tx の中で行う。
func writeChapterSpansTx(t *testing.T, tx *sql.Tx, recordingID int64, spans []chapters.Span) {
	t.Helper()
	ctx := context.Background()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_chapter_ownership (recording_id)
SELECT ? FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM recording_chapter_ownership WHERE recording_id = ?)`, recordingID, recordingID); err != nil {
		t.Fatalf("adopting chapters: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM recording_chapter_spans WHERE recording_id = ?`, recordingID); err != nil {
		t.Fatalf("clearing chapter spans: %v", err)
	}
	for _, sp := range spans {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_chapter_spans (recording_id, start_ms, end_ms, label, cut) VALUES (?, ?, ?, ?, ?)`,
			recordingID, sp.StartMs, sp.EndMs, sp.Label, sp.Cut); err != nil {
			t.Fatalf("writing chapter span: %v", err)
		}
	}
}

// killOpenTransactions は、このテストの DB の接続のうち、このテストの接続以外で tx を開いているものを落とす（pgstore の TerminateOpenTransactions と同じく DB で絞る）。
// 公開の tx の接続を落とすと、公開の後の commit が失敗する。
func killOpenTransactions(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("getting connection to terminate transactions: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var self uint64
	if err := conn.QueryRowContext(ctx, `SELECT CONNECTION_ID()`).Scan(&self); err != nil {
		t.Fatalf("reading connection ID: %v", err)
	}
	rows, err := conn.QueryContext(ctx, `SELECT t.trx_mysql_thread_id FROM information_schema.innodb_trx t JOIN information_schema.processlist p ON p.ID = t.trx_mysql_thread_id WHERE p.DB = DATABASE()`)
	if err != nil {
		t.Fatalf("listing open transactions: %v", err)
	}
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			t.Fatalf("scanning open transaction: %v", err)
		}
		if id != self {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("KILL CONNECTION %d", id)); err != nil {
			t.Fatalf("killing connection %d: %v", id, err)
		}
	}
}

// waitForLockWaiter は、この DB のどこかの行ロックで待つ tx が 1 つ以上できるまで待つ。
// information_schema.innodb_trx は trx ID を持たない locking read の待機を見せないので、
// performance_schema.data_lock_waits を読む（reservations_contract_test.go の waitLockWaiters と同じ）。
func waitForLockWaiter(t *testing.T, db *sql.DB, operation string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		var n int
		err := db.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
FROM performance_schema.data_lock_waits w
JOIN performance_schema.data_locks l
  ON l.ENGINE = w.ENGINE AND l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
WHERE l.OBJECT_SCHEMA = DATABASE()`).Scan(&n)
		if ctx.Err() != nil {
			t.Fatalf("%s did not wait for a row lock", operation)
		}
		if err != nil {
			t.Fatalf("observing row lock waiters: %v", err)
		}
		if n > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s did not wait for a row lock", operation)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
