package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/fetburner/rokuban/internal/mysqldb"
	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
)

// frozenMySQLTimestamp は 2001-01-01 00:00:00 UTC の UNIX 時刻。SET timestamp にこの値を入れると NOW(6) がこの時刻で止まる。
const frozenMySQLTimestamp = "978307200"

// TestMediaAgeingUsesDatabaseClock は経過時間の比較が DB の時計で行われることを検査する。
// 契約テスト（RunMedia の ageing）は、テストと DB が同じホストの時計を指すため、ホストの時計で比べる実装を見分けられない。
// そこで DB の時計だけを 2001 年に固定した接続で書き、ホストの時計（今日）と取り違えた比較を検出する。
func TestMediaAgeingUsesDatabaseClock(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	frozen := NewMediaInStore(openFrozenClockPool(t, db), "tokyo", store.StoragePolicy{})
	f := newMediaContractFixtureOn(db)
	ctx := context.Background()

	t.Run("orphan", func(t *testing.T) {
		const fresh, old = "sites/home/clock-fresh.tmp", "sites/home/clock-old.tmp"
		for _, p := range []string{fresh, old} {
			if err := frozen.UpsertOrphanFile(ctx, p); err != nil {
				t.Fatalf("UpsertOrphanFile(%q): %v", p, err)
			}
		}
		// 2 時間前は DB の時計（2001 年）で数える。ホストの時計で比べる実装ならここで取り違える。
		shiftFirstSeenFrozen(t, frozen.db, `UPDATE orphan_files SET first_seen = NOW(6) - INTERVAL ? MICROSECOND
WHERE rel_path_key = UNHEX(SHA2(?, 256))`, (2 * time.Hour).Microseconds(), old)
		got, err := frozen.ListAgedOrphanFiles(ctx, time.Hour)
		if err != nil {
			t.Fatalf("ListAgedOrphanFiles(1h): %v", err)
		}
		if len(got) != 1 || got[0] != old {
			t.Fatalf("ListAgedOrphanFiles(1h) = %q, want only %q (a candidate recorded just now must not be aged)", got, old)
		}
	})

	t.Run("missing", func(t *testing.T) {
		id := f.NewRecording(t)
		assetID := f.NewAsset(t, id, "original", nil, fmt.Sprintf("sites/home/%d/clock-original.m2ts", id), "active")
		copyID := f.CopyID(t, assetID)
		if err := frozen.UpsertMissingMediaAsset(ctx, copyID); err != nil {
			t.Fatalf("UpsertMissingMediaAsset(copy %d): %v", copyID, err)
		}
		got, err := frozen.ListAgedMissingMediaAssets(ctx, time.Hour)
		if err != nil || len(got) != 0 {
			t.Fatalf("ListAgedMissingMediaAssets(1h) just after recording = (%+v, %v), want none", got, err)
		}
		shiftFirstSeenFrozen(t, frozen.db, `UPDATE missing_media_assets SET first_seen = NOW(6) - INTERVAL ? MICROSECOND
WHERE media_asset_copy_id = ?`, (2 * time.Hour).Microseconds(), copyID)
		got, err = frozen.ListAgedMissingMediaAssets(ctx, time.Hour)
		if err != nil {
			t.Fatalf("ListAgedMissingMediaAssets(1h): %v", err)
		}
		if len(got) != 1 || got[0].ID != copyID {
			t.Fatalf("ListAgedMissingMediaAssets(1h) = %+v, want only copy %d", got, copyID)
		}
	})
}

// openFrozenClockPool は db と同じ DB に、NOW(6) を 2001 年に固定した接続プールを開く。
// 固定できていることを確かめてから返す。固定できていなければ、このテストは何も検査していない。
func openFrozenClockPool(t *testing.T, db *sql.DB) *sql.DB {
	t.Helper()
	ctx := context.Background()
	var name string
	if err := db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&name); err != nil {
		t.Fatalf("reading test database name: %v", err)
	}
	cfg, err := mysql.ParseDSN(mysqltest.TestDSN(t))
	if err != nil {
		t.Fatalf("parsing MySQL test DSN: %v", err)
	}
	cfg.DBName = name
	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	cfg.Params["timestamp"] = frozenMySQLTimestamp
	frozen, err := mysqldb.OpenPool(ctx, cfg.FormatDSN())
	if err != nil {
		t.Fatalf("opening frozen-clock MySQL pool: %v", err)
	}
	t.Cleanup(func() {
		if err := frozen.Close(); err != nil {
			t.Errorf("closing frozen-clock MySQL pool: %v", err)
		}
	})
	var now string
	if err := frozen.QueryRowContext(ctx, `SELECT DATE_FORMAT(NOW(6), '%Y-%m-%d %H:%i:%s')`).Scan(&now); err != nil {
		t.Fatalf("reading frozen clock: %v", err)
	}
	if now != "2001-01-01 00:00:00" {
		t.Fatalf("frozen clock reads %q, want 2001-01-01 00:00:00", now)
	}
	return frozen
}

// shiftFirstSeenFrozen は固定時計の接続で first_seen を過去へずらす。
func shiftFirstSeenFrozen(t *testing.T, frozen *sql.DB, query string, args ...any) {
	t.Helper()
	res, err := frozen.ExecContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("shifting first_seen: %v", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("shifting first_seen changed %d rows (%v); want 1", n, err)
	}
}
