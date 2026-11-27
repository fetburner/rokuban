package mysqldb_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/fetburner/rokuban/internal/mysqldb"
	"github.com/fetburner/rokuban/internal/mysqltest"
)

// seedAlignRecording は録画を 1 件入れて ID を返す。テストごとに DB が別なので event_id は固定でよい。
func seedAlignRecording(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	res, err := db.ExecContext(context.Background(), `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name, channel_type, channel, title, program_start_at, program_duration_ms, status)
VALUES ('manual', 'test', 1, 1, 1, 'svc', 'GR', 'ch', 't', '2030-01-01 00:00:00.000000', 1000, 'finished')`)
	if err != nil {
		t.Fatalf("seeding recording: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("reading recording ID: %v", err)
	}
	return id
}

// seedAlignTransferCopy は pending の転送コピーを 1 件入れて copy_id を返す。
func seedAlignTransferCopy(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	ctx := context.Background()
	recordingID := seedAlignRecording(t, db)
	res, err := db.ExecContext(ctx, `INSERT INTO media_assets (recording_id, kind, size_bytes) VALUES (?, 'original', 0)`, recordingID)
	if err != nil {
		t.Fatalf("seeding media asset: %v", err)
	}
	assetID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("reading media asset ID: %v", err)
	}
	res, err = db.ExecContext(ctx, `INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, generation)
VALUES (?, 'test-store', 'orig/source.ts', 'pending', 0)`, assetID)
	if err != nil {
		t.Fatalf("seeding media asset copy: %v", err)
	}
	copyID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("reading media asset copy ID: %v", err)
	}
	return copyID
}

// insertAlignTransferObject は同じ rel_path の行を入れる。2 回目の呼び出しが主キーの検査になる。
func insertAlignTransferObject(db *sql.DB, copyID int64) error {
	_, err := db.ExecContext(context.Background(), `INSERT INTO media_asset_transfer_objects (copy_id, rel_path) VALUES (?, 'transfer/object.mp4')`, copyID)
	return err
}

// TestTransferObjectsRejectDuplicateKey は同じ (copy_id, rel_path) の 2 回目の INSERT が一意キー違反になることを固定する。
// 00020 までの表には主キーが無く、この検査は落ちる。
func TestTransferObjectsRejectDuplicateKey(t *testing.T) {
	db := mysqltest.SetupDB(t)
	copyID := seedAlignTransferCopy(t, db)
	if err := insertAlignTransferObject(db, copyID); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := insertAlignTransferObject(db, copyID)
	if !mysqldb.IsUniqueViolation(err) {
		t.Fatalf("second insert of the same (copy_id, rel_path) error = %v, want unique violation", err)
	}
}

// TestChapterSpansRejectSameStart は同じ (recording_id, start_ms) の 2 行が一意キー違反になることを固定する。
// 00020 までは索引が一意でないので、この検査は落ちる。
func TestChapterSpansRejectSameStart(t *testing.T) {
	db := mysqltest.SetupDB(t)
	recordingID := seedAlignRecording(t, db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES (?)`, recordingID); err != nil {
		t.Fatalf("seeding chapter ownership: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO recording_chapter_spans (recording_id, start_ms, end_ms, label, cut) VALUES (?, 0, 10000, 'a', TRUE)`, recordingID); err != nil {
		t.Fatalf("first span insert: %v", err)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO recording_chapter_spans (recording_id, start_ms, end_ms, label, cut) VALUES (?, 0, 20000, 'b', TRUE)`, recordingID)
	if !mysqldb.IsUniqueViolation(err) {
		t.Fatalf("second span with the same start_ms error = %v, want unique violation", err)
	}
}

// TestCircuitBreakerTrippedAtAfter2038 は 2038-01-19 を過ぎた tripped_at が書けて同じ値で読めることを固定する。
// TIMESTAMP のままだと書き込みが範囲外で失敗する。
func TestCircuitBreakerTrippedAtAfter2038(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	want := time.Date(2038, 1, 20, 3, 14, 7, 123456000, time.UTC)
	if _, err := db.ExecContext(ctx, `INSERT INTO circuit_breakers (scope, name, tripped_at, pending, threshold, detail)
VALUES ('test', 'breaker', ?, 1, 1, JSON_OBJECT())`, want); err != nil {
		t.Fatalf("inserting breaker tripped after 2038: %v", err)
	}
	var got time.Time
	if err := db.QueryRowContext(ctx, `SELECT tripped_at FROM circuit_breakers WHERE scope = 'test' AND name = 'breaker'`).Scan(&got); err != nil {
		t.Fatalf("reading tripped_at: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("tripped_at = %s, want %s", got.UTC().Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

// TestMigration00021RoundTrip は 00021 を版 20 まで戻して重複 INSERT が通り、Up で主キーが戻ることを固定する。
// 1 段だけ戻すと、後続の migration が増えたときに 00021 ではない版を戻すので、版を名指しする。
// 00020 までの表には主キーが無く、Up 後の重複検査は落ちる。
func TestMigration00021RoundTrip(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	copyID := seedAlignTransferCopy(t, db)
	if err := insertAlignTransferObject(db, copyID); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// 埋め込みと同じ migrations を、テストのパッケージディレクトリ（go test の作業dir）から読んで版 20 まで戻す。
	provider, err := goose.NewProvider(goose.DialectMySQL, db, os.DirFS("migrations"))
	if err != nil {
		t.Fatalf("creating goose provider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 20); err != nil {
		t.Fatalf("DownTo(20): %v", err)
	}
	if err := insertAlignTransferObject(db, copyID); err != nil {
		t.Fatalf("duplicate insert at version 20 should succeed (no primary key): %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM media_asset_transfer_objects WHERE copy_id = ?`, copyID); err != nil {
		t.Fatalf("clearing duplicates before Up: %v", err)
	}
	if err := mysqldb.MigrateUp(ctx, db); err != nil {
		t.Fatalf("MigrateUp after DownTo(20): %v", err)
	}
	if err := mysqldb.CheckMigrationsCurrent(ctx, db); err != nil {
		t.Fatalf("CheckMigrationsCurrent: %v", err)
	}
	if err := insertAlignTransferObject(db, copyID); err != nil {
		t.Fatalf("first insert after Up: %v", err)
	}
	if err := insertAlignTransferObject(db, copyID); !mysqldb.IsUniqueViolation(err) {
		t.Fatalf("duplicate insert after Up error = %v, want unique violation", err)
	}
}
