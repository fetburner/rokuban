package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

// catalogContractStoreName は契約テストの物理コピーの置き場。roundTripCatalogDocument の Store と揃える。
const catalogContractStoreName = "test-store"

func TestCatalogContract(t *testing.T) {
	t.Parallel()
	contracttest.RunCatalog(t, func(t *testing.T) contracttest.CatalogFixture {
		db := mysqltest.SetupDB(t)
		reservations := NewReservations(db)
		return contracttest.CatalogFixture{
			Store:                  NewCatalog(db, []string{"site-b", "default", "site-a", "site-b"}, catalogContractStoreName),
			Pass:                   reservations,
			RulerInputSites:        []string{"default", "site-a", "site-b"},
			BumpRulerInputRevision: reservations.BumpRulerInputRevision,
			InsertRecordingAndAsset: func(ctx context.Context, eventID int32) error {
				return insertCatalogContractRecordingAndAsset(ctx, db, eventID)
			},
			Reset: func(t *testing.T) {
				t.Helper()
				truncateCatalogTables(t, db)
			},
			FailAtMediaAssetWrite: func(t *testing.T) {
				t.Helper()
				execFixture(t, db, `CREATE TRIGGER catalog_contract_fail_media_asset BEFORE INSERT ON media_assets FOR EACH ROW
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected catalog restore failure'`)
				t.Cleanup(func() {
					execFixture(t, db, `DROP TRIGGER IF EXISTS catalog_contract_fail_media_asset`)
				})
			},
			FailAtRulerInputRevisionWrite: func(t *testing.T) {
				t.Helper()
				// MySQL のトリガーはシーケンスを使えないので、更新の回数を AUTO_INCREMENT の表で数える。
				execFixture(t, db, `CREATE TABLE catalog_contract_revision_writes (
    seq BIGINT NOT NULL AUTO_INCREMENT,
    PRIMARY KEY (seq)
) ENGINE=InnoDB`)
				execFixture(t, db, `CREATE TRIGGER catalog_contract_fail_ruler_revision BEFORE UPDATE ON ruler_input_revisions FOR EACH ROW
BEGIN
    INSERT INTO catalog_contract_revision_writes () VALUES ();
    IF LAST_INSERT_ID() = 3 THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected ruler input revision failure';
    END IF;
END`)
				t.Cleanup(func() {
					execFixture(t, db, `DROP TRIGGER IF EXISTS catalog_contract_fail_ruler_revision`)
					execFixture(t, db, `DROP TABLE IF EXISTS catalog_contract_revision_writes`)
				})
			},
			BeginLabelRuleReevaluationLock: func(t *testing.T) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("beginning re-evaluation lock tx: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback() })
				if err := lockLabelRuleReconcile(ctx, tx); err != nil {
					t.Fatalf("acquiring re-evaluation lock: %v", err)
				}
				return func() {
					if err := tx.Commit(); err != nil {
						t.Errorf("releasing re-evaluation lock: %v", err)
					}
				}
			},
			WaitForRestoreLock: func(t *testing.T) {
				t.Helper()
				// Restore は最初の書き込みより前に名前付きロックで待つ。待ちは名前付きロックの行にだけ出る。
				waitLockWaiters(t, db, "named_locks", 1)
			},
		}
	})
}

// insertCatalogContractRecordingAndAsset は録画と原本を 1 件ずつ、別々の commit で入れる。
// 原本の物理コピーは、export が読む置き場に置く。
func insertCatalogContractRecordingAndAsset(ctx context.Context, db *sql.DB, eventID int32) error {
	res, err := db.ExecContext(ctx, `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, program_start_at, program_duration_ms, status)
VALUES ('manual', 'default', 32736, 1024, ?, 'test', 'GR', '27', 'catalog contract', '2026-01-02 03:04:05', 1000, 'recording')`,
		eventID)
	if err != nil {
		return err
	}
	recordingID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	res, err = db.ExecContext(ctx, `INSERT INTO media_assets (recording_id, kind, size_bytes) VALUES (?, 'original', 100)`, recordingID)
	if err != nil {
		return err
	}
	assetID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES (?, ?, ?, 0, 'active')`,
		assetID, catalogContractStoreName, "catalog-contract/"+time.Unix(int64(eventID), 0).UTC().Format("150405")+".m2ts")
	return err
}

// truncateCatalogTables は catalog が保護する表を空にする。外部キーの検査を止めた 1 つの接続で行う。
func truncateCatalogTables(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("checking out connection to empty catalog tables: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		t.Fatalf("disabling foreign key checks: %v", err)
	}
	for _, table := range []string{
		"label_rule_hits", "recording_chapter_spans", "recording_chapter_ownership", "recording_watched",
		"recording_purge_requests", "recording_processing_policy", "drop_positions", "drop_stats",
		"media_asset_ts_scans", "media_asset_copies", "media_assets", "recordings",
		"program_overrides", "program_intents", "program_snapshots", "label_rules", "rule_import_keys",
		"rule_text_matches", "rule_services", "rule_channel_types", "rule_genres", "rule_times", "rule_sites", "rules",
	} {
		if _, err := conn.ExecContext(ctx, "TRUNCATE TABLE "+table); err != nil {
			t.Fatalf("emptying %s: %v", table, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS = 1`); err != nil {
		t.Fatalf("enabling foreign key checks: %v", err)
	}
}

// TestInsertableSnapshot は、MySQL が拒否する snapshot の行だけを落とすことを境界の値で確かめる。
// 拒否の実際は TestMySQLDatetimeRangeRejectsYearsAbove9999 が実 MySQL で確かめる。
func TestInsertableSnapshot(t *testing.T) {
	t.Parallel()
	inside := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	base := catalogdoc.ProgramSnapshot{Site: "default", ChannelType: "GR", StartAt: inside, UpdatedAt: inside}
	for _, tc := range []struct {
		name string
		edit func(*catalogdoc.ProgramSnapshot)
		want bool
	}{
		{"GR", func(*catalogdoc.ProgramSnapshot) {}, true},
		{"unknown channel type", func(s *catalogdoc.ProgramSnapshot) { s.ChannelType = "XX" }, false},
		{"site of 255 characters", func(s *catalogdoc.ProgramSnapshot) { s.Site = strings.Repeat("s", 255) }, true},
		{"site of 256 characters", func(s *catalogdoc.ProgramSnapshot) { s.Site = strings.Repeat("s", 256) }, false},
		{"start at year 0 (outside the driver's range)", func(s *catalogdoc.ProgramSnapshot) { s.StartAt = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) }, false},
		{"start at year 999", func(s *catalogdoc.ProgramSnapshot) { s.StartAt = time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC) }, true},
		{"start at 9999-12-31 23:59:59.999999", func(s *catalogdoc.ProgramSnapshot) {
			s.StartAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		}, true},
		{"start at year 10000", func(s *catalogdoc.ProgramSnapshot) { s.StartAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, false},
		{"updated at year 10000", func(s *catalogdoc.ProgramSnapshot) { s.UpdatedAt = time.Date(10000, 6, 1, 0, 0, 0, 0, time.UTC) }, false},
		{"updated at 0999", func(s *catalogdoc.ProgramSnapshot) { s.UpdatedAt = time.Date(999, 6, 1, 0, 0, 0, 0, time.UTC) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.edit(&s)
			if got := insertableSnapshot(s); got != tc.want {
				t.Fatalf("insertableSnapshot = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestMySQLDatetimeRangeRejectsYearsAbove9999 は、insertableSnapshot が写す DATETIME(6) の範囲が
// 実際の MySQL の拒否と一致することを確かめる。年 999 と 9999 は入り、10000 年は入らない。
func TestMySQLDatetimeRangeRejectsYearsAbove9999(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	for _, tc := range []struct {
		value time.Time
		want  bool
	}{
		{time.Date(999, 12, 31, 23, 59, 59, 999999000, time.UTC), true},
		{time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), true},
		{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), false},
	} {
		// 値は driver の時刻の書き方（UTC で整形）を通す。insertableSnapshot が見るのと同じ経路。
		_, err := db.ExecContext(ctx, `
INSERT INTO program_snapshots (site, program_id, title, start_at, duration_ms, network_id, service_id,
    channel_type, channel, event_id, service_name)
VALUES ('default', 1, 't', ?, 1000, 1, 1, 'GR', 'ch', 1, 'svc')`, tc.value)
		if got := err == nil; got != tc.want {
			t.Fatalf("inserting start_at %s: accepted = %t (err %v), want %t", tc.value, got, err, tc.want)
		}
		execFixture(t, db, `DELETE FROM program_snapshots`)
	}
}

// TestCatalogRestoreSkipsSnapshotsMySQLRejects は、MySQL が拒否する（年 10000 の）snapshot と、それに従属する意図を
// 落として、残りの復元を続けることを確かめる（落とす判定は insertableSnapshot）。
func TestCatalogRestoreSkipsSnapshotsMySQLRejects(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	catalog := NewCatalog(db, []string{"default"}, catalogContractStoreName)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	doc := &catalogdoc.Document{
		Version: catalogdoc.Version,
		ProgramSnapshots: []catalogdoc.ProgramSnapshot{{
			Site: "default", ProgramID: 7, Title: "old", StartAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
			DurationMs: 1000, NetworkID: 1, ServiceID: 1, ChannelType: "GR", Channel: "ch", EventID: 1,
			ServiceName: "svc", UpdatedAt: at,
		}},
		ProgramIntents: []catalogdoc.ProgramIntent{{Site: "default", ProgramID: 7, Action: "record", CreatedAt: at, UpdatedAt: at}},
	}
	res, err := catalog.Restore(ctx, doc, store.RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore with an out-of-range snapshot: %v", err)
	}
	if res.SkippedProgramSnapshots != 1 || res.SkippedProgramIntents != 1 || res.ProgramSnapshots != 0 {
		t.Fatalf("skipped snapshots/intents = %d/%d, restored snapshots = %d; want 1/1/0",
			res.SkippedProgramSnapshots, res.SkippedProgramIntents, res.ProgramSnapshots)
	}
}

// TestCatalogRestoreWritesDerivedKeys は、復元が MySQL で生成列でない導出値（録画の series_key と
// 分類ルールの value_key）をアプリとして書くことを確かめる。題名から鍵が取れれば書き、取れなければ NULL にする。
// 分類ルールの値は鍵が無ければ拒む。
func TestCatalogRestoreWritesDerivedKeys(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	catalog := NewCatalog(db, []string{"default"}, catalogContractStoreName)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	recording := func(id int64, title string) catalogdoc.Recording {
		return catalogdoc.Recording{
			ID: id, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: int32(id),
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: title, IsFree: true,
			ProgramStartAt: at, ProgramDurationMs: 1000, Status: "recording", CreatedAt: at, UpdatedAt: at,
		}
	}
	doc := &catalogdoc.Document{
		Version:    catalogdoc.Version,
		Recordings: []catalogdoc.Recording{recording(1, "Hello World 第2話"), recording(2, "▽★")},
		// キーワードは録画の題名に当たらないものにする。当たると recording_series は value_key を返し、series_key を見ない。
		LabelRules: []catalogdoc.LabelRule{{ID: 1, Key: "series", Value: "Hello World 第2話", Keyword: "nomatch", CreatedAt: at, UpdatedAt: at}},
	}
	if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// 題名から鍵が取れる録画は "Hello"、取れない録画は NULL（recording_series の値）になる。
	for id, want := range map[int64]sql.NullString{1: {String: "Hello", Valid: true}, 2: {}} {
		var got sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT value FROM recording_series WHERE recording_id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("reading recording_series of %d: %v", id, err)
		}
		if got != want {
			t.Fatalf("recording_series of %d = %v, want %v", id, got, want)
		}
	}
	var valueKey string
	if err := db.QueryRowContext(ctx, `SELECT value_key FROM label_rules WHERE id = 1`).Scan(&valueKey); err != nil {
		t.Fatalf("reading value_key: %v", err)
	}
	if valueKey != "Hello" {
		t.Fatalf("value_key = %q, want %q", valueKey, "Hello")
	}

	noKey := &catalogdoc.Document{
		Version:    catalogdoc.Version,
		LabelRules: []catalogdoc.LabelRule{{ID: 2, Key: "series", Value: "▽★", Keyword: "x", CreatedAt: at, UpdatedAt: at}},
	}
	if _, err := catalog.Restore(ctx, noKey, store.RestoreOptions{}); err == nil {
		t.Fatal("Restore accepted a label rule whose value has no series key")
	}
}

// TestCatalogRestoreAdvancesAutoIncrement は、明示 ID で復元した後の採番が復元した ID を避けることを確かめる。
// Restore の doc comment の「明示 ID の挿入で AUTO_INCREMENT が最大値 + 1 以上に進む」の検査。
func TestCatalogRestoreAdvancesAutoIncrement(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	catalog := NewCatalog(db, []string{"default"}, catalogContractStoreName)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	doc := &catalogdoc.Document{
		Version: catalogdoc.Version,
		Rules:   []catalogdoc.Rule{{ID: 100, Name: "restored", KeepOriginal: "always", EncodeProfiles: []string{}, Metadata: json.RawMessage(`{}`), CreatedAt: at, UpdatedAt: at}},
	}
	if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	res, err := db.ExecContext(ctx, `INSERT INTO rules (name) VALUES ('after restore')`)
	if err != nil {
		t.Fatalf("inserting a rule without an id: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if id < 101 {
		t.Fatalf("id after restoring id 100 = %d, want at least 101", id)
	}
}

// TestCatalogRestoreRederivesKeysOnUpdate は、既存の録画を再復元（UPDATE の経路）したときも、
// 題名から導く series_key と genres から導く genre_lv1 が新しい値に変わることを確かめる。
// 新規の INSERT の経路は TestCatalogRestoreWritesDerivedKeys と TestCatalogRestoreAdvancesAutoIncrement が見る。
func TestCatalogRestoreRederivesKeysOnUpdate(t *testing.T) {
	t.Parallel()
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	catalog := NewCatalog(db, []string{"default"}, catalogContractStoreName)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	restoreTitle := func(title string, genres string) {
		t.Helper()
		doc := &catalogdoc.Document{
			Version: catalogdoc.Version,
			Recordings: []catalogdoc.Recording{{
				ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1,
				ServiceName: "test", ChannelType: "GR", Channel: "27", Title: title, IsFree: true,
				Genres: json.RawMessage(genres), ProgramStartAt: at, ProgramDurationMs: 1000, Status: "recording",
				CreatedAt: at, UpdatedAt: at,
			}},
		}
		if _, err := catalog.Restore(ctx, doc, store.RestoreOptions{}); err != nil {
			t.Fatalf("Restore of title %q: %v", title, err)
		}
	}
	// 題名から鍵が取れなければ NULL、取れれば鍵。genre_lv1 は JSON の配列で、JSON_CONTAINS で読む。
	check := func(name string, wantKey sql.NullString, wantLevel5 bool) {
		t.Helper()
		var key sql.NullString
		var level5 bool
		if err := db.QueryRowContext(ctx, `SELECT series_key, JSON_CONTAINS(genre_lv1, '5') FROM recordings WHERE id = 1`).Scan(&key, &level5); err != nil {
			t.Fatalf("%s: reading recording: %v", name, err)
		}
		if key != wantKey {
			t.Fatalf("%s: series_key = %v, want %v", name, key, wantKey)
		}
		if level5 != wantLevel5 {
			t.Fatalf("%s: genre_lv1 contains 5 = %t, want %t", name, level5, wantLevel5)
		}
	}
	restoreTitle("Hello World 第2話", `[{"lv1":3}]`)
	check("insert", sql.NullString{String: "Hello", Valid: true}, false)
	restoreTitle("▽★", `[{"lv1":5}]`)
	check("update to a title without a key", sql.NullString{}, true)
	restoreTitle("Foo Bar", `[{"lv1":5}]`)
	check("update to a title with a key", sql.NullString{String: "Foo", Valid: true}, true)
}
