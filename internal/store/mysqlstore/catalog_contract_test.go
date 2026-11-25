package mysqlstore

import (
	"context"
	"database/sql"
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

// TestGenreLv1Of は genres から導く genre_lv1 が Postgres の genre_lv1_of と同じ規則に従うことを確かめる。
// 数字の文字列か整数の lv1 だけを数え、0〜15 の範囲外・小数・欠けた要素は無視する。
func TestGenreLv1Of(t *testing.T) {
	for _, tc := range []struct {
		name, genres, want string
	}{
		{"empty", ``, `[]`},
		{"not array", `{"lv1":3}`, `[]`},
		{"sorted and distinct", `[{"lv1":5},{"lv1":1},{"lv1":5}]`, `[1,5]`},
		{"string level", `[{"lv1":"7"}]`, `[7]`},
		{"out of range", `[{"lv1":16},{"lv1":-1},{"lv1":15}]`, `[15]`},
		{"fraction is not a level", `[{"lv1":3.0}]`, `[]`},
		{"missing key", `[{"lv2":3}]`, `[]`},
		{"not an object", `[3]`, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := genreLv1Of([]byte(tc.genres))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("genreLv1Of(%s) = %s, want %s", tc.genres, got, tc.want)
			}
		})
	}
}

// TestInsertableSnapshot は、MySQL が拒否する snapshot の行だけを落とすことを確かめる。
func TestInsertableSnapshot(t *testing.T) {
	base := catalogdoc.ProgramSnapshot{Site: "default", ChannelType: "GR"}
	if !insertableSnapshot(base) {
		t.Fatal("a GR snapshot is rejected")
	}
	bad := base
	bad.ChannelType = "XX"
	if insertableSnapshot(bad) {
		t.Fatal("an unknown channel type is accepted")
	}
	long := base
	long.Site = strings.Repeat("s", 255)
	if !insertableSnapshot(long) {
		t.Fatal("a 255-character site is rejected")
	}
	long.Site = strings.Repeat("s", 256)
	if insertableSnapshot(long) {
		t.Fatal("a 256-character site is accepted, but site is VARCHAR(255)")
	}
}

// TestCatalogRestoreWritesDerivedKeys は、復元が MySQL で生成列でない導出値（録画の series_key と
// 分類ルールの value_key）をアプリとして書くことを確かめる。題名から鍵が取れれば書き、取れなければ NULL にする。
// 分類ルールの値は鍵が無ければ拒む。
func TestCatalogRestoreWritesDerivedKeys(t *testing.T) {
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
