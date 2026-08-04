package db

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

func setupTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateReset(ctx, dbURL); err != nil {
		t.Fatalf("migrate reset: %v", err)
	}
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() {
		if err := MigrateReset(ctx, dbURL); err != nil {
			t.Errorf("cleanup migrate reset: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func assertPgError(t *testing.T, err error, sqlstate string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != sqlstate {
		t.Errorf("SQLSTATE = %s, want %s", pgErr.Code, sqlstate)
	}
}

func TestSchemaV1_Tables(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	tables := []string{
		"reservations", "schedule_sync", "recordings", "record_sync", "media_assets", "drop_stats",
		// M2-1
		"rules", "rule_text_matches", "rule_services", "rule_channel_types",
		"rule_genres", "rule_times", "rule_sites", "reservation_rule_matches",
	}
	for _, table := range tables {
		var exists bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = $1)", table).Scan(&exists)
		if err != nil {
			t.Fatalf("checking table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s does not exist", table)
		}
	}
}

// insertTestProgramSnapshot は reservations / program_intents / program_overrides
// が参照する program_snapshots 行を作る（#27 で追加された FK の前提）。
//
// チャンネル・イベント識別 6 列は issue #101（00026）で NOT NULL 化された。
// このパッケージのテストは reservations/program_intents/program_overrides の
// FK・GC を見るだけでチャンネル識別自体は検証しないので、固定のダミー値で足りる。
func insertTestProgramSnapshot(t *testing.T, pool *pgxpool.Pool, site string, programID int64, startAt time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO program_snapshots (
			site, program_id, title, start_at, duration_ms,
			network_id, service_id, channel_type, channel, event_id, service_name
		 )
		 VALUES ($1, $2, '', $3, 1800000, 32736, 1024, 'GR', '27', 1, 'テスト局')`,
		site, programID, startAt)
	if err != nil {
		t.Fatalf("insert program_snapshot: %v", err)
	}
}

func TestSchemaV1_ReservationCRUD(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Microsecond)
	insertTestProgramSnapshot(t, pool, "home", 327360102415397, now)

	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO reservations (site, program_id)
		 VALUES ($1, $2)
		 RETURNING id`,
		"home", int64(327360102415397),
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive id, got %d", id)
	}

	var r Reservation
	err = pool.QueryRow(ctx,
		`SELECT id, site, program_id FROM reservations WHERE id = $1`, id,
	).Scan(&r.ID, &r.Site, &r.ProgramID)
	if err != nil {
		t.Fatalf("select reservation: %v", err)
	}
	if r.Site != "home" {
		t.Errorf("site = %q, want %q", r.Site, "home")
	}
	if r.ProgramID != 327360102415397 {
		t.Errorf("program_id = %d, want 327360102415397", r.ProgramID)
	}
}

func TestSchemaV1_ReservationUniqueSiteProgramID(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	now := time.Now()

	insertTestProgramSnapshot(t, pool, "home", 100, now)
	insertTestProgramSnapshot(t, pool, "office", 100, now)

	_, err := pool.Exec(ctx,
		`INSERT INTO reservations (site, program_id) VALUES ($1, $2)`,
		"home", int64(100))
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// 同一 site + program_id は一意制約違反
	_, err = pool.Exec(ctx,
		`INSERT INTO reservations (site, program_id) VALUES ($1, $2)`,
		"home", int64(100))
	assertPgError(t, err, "23505")

	// 別サイトなら同一 program_id で OK
	_, err = pool.Exec(ctx,
		`INSERT INTO reservations (site, program_id) VALUES ($1, $2)`,
		"office", int64(100))
	if err != nil {
		t.Fatalf("different site should allow same program_id: %v", err)
	}
}

func TestSchemaV1_RecordingInsert(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)

	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO recordings (
			site, source, network_id, service_id, event_id,
			service_name, channel_type, channel, title,
			program_start_at, program_duration_ms, status, started_at
		 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		"home", "manual", 32736, 1024, 15397,
		"NHK総合", "GR", "27", "テスト番組",
		now, int64(1800000), "finished", now,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert recording: %v", err)
	}

	var rec Recording
	err = pool.QueryRow(ctx,
		`SELECT id, site, source, channel_type, status, title FROM recordings WHERE id = $1`, id,
	).Scan(&rec.ID, &rec.Site, &rec.Source, &rec.ChannelType, &rec.Status, &rec.Title)
	if err != nil {
		t.Fatalf("select recording: %v", err)
	}
	if rec.ChannelType != "GR" {
		t.Errorf("channel_type = %q, want %q", rec.ChannelType, "GR")
	}
	if rec.Status != "finished" {
		t.Errorf("status = %q, want %q", rec.Status, "finished")
	}
}

func TestSchemaV1_RecordingChannelTypeCheck(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO recordings (
			site, source, network_id, service_id, event_id,
			service_name, channel_type, channel, title,
			program_start_at, program_duration_ms, status
		 ) VALUES ('home','manual',1,1,1,'NHK','CATV','1','test',now(),0,'finished')`)
	assertPgError(t, err, "23514")
}

// TestSchemaV1_ProgramSnapshotChannelIdentityNotNull は issue #101（00026）の
// 回帰テスト。program_snapshots のチャンネル・イベント識別 6 列
// （network_id/service_id/channel_type/channel/event_id/service_name）は
// 「00009 以前の残骸を救えず nullable のままの行がありうる」という理由で
// nullable だったが、行の寿命（放送 + retention_grace）と書き込み経路が
// どちらも INNER JOIN であることから、この理由は失効している（issue 本文）。
// 00026 でこの 6 列を NOT NULL 化した後、各列を個別に NULL にした INSERT が
// すべて 23502 で落ち、6 列すべて揃えた INSERT は成功することを確認する。
func TestSchemaV1_ProgramSnapshotChannelIdentityNotNull(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	base := map[string]any{
		"site": "home", "title": "テスト番組", "start_at": time.Now(), "duration_ms": int64(1800000),
		"network_id": int32(32736), "service_id": int32(1024), "channel_type": "GR", "channel": "27",
		"event_id": int32(1), "service_name": "テスト局",
	}
	columns := []string{"network_id", "service_id", "channel_type", "channel", "event_id", "service_name"}

	for i, col := range columns {
		t.Run("NULL "+col, func(t *testing.T) {
			row := map[string]any{}
			for k, v := range base {
				row[k] = v
			}
			row[col] = nil
			row["program_id"] = int64(700000000000000) + int64(i)*1000

			_, err := pool.Exec(ctx, `
				INSERT INTO program_snapshots (
					site, program_id, title, start_at, duration_ms,
					network_id, service_id, channel_type, channel, event_id, service_name
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
				row["site"], row["program_id"], row["title"], row["start_at"], row["duration_ms"],
				row["network_id"], row["service_id"], row["channel_type"], row["channel"],
				row["event_id"], row["service_name"])
			assertPgError(t, err, "23502")
		})
	}

	t.Run("all 6 columns present succeeds", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO program_snapshots (
				site, program_id, title, start_at, duration_ms,
				network_id, service_id, channel_type, channel, event_id, service_name
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			base["site"], int64(700000000009999), base["title"], base["start_at"], base["duration_ms"],
			base["network_id"], base["service_id"], base["channel_type"], base["channel"],
			base["event_id"], base["service_name"])
		if err != nil {
			t.Fatalf("insert with all 6 columns should succeed: %v", err)
		}
	})
}

// TestSchemaV1_ProgramSnapshotChannelTypeCheckSimplified は 00026 が
// CHECK (channel_type IS NULL OR channel_type IN (...)) を
// CHECK (channel_type IN (...)) に単純化したことの回帰テスト。NOT NULL 化で
// NULL 分岐はもう表現できないので、許可されない値（例: 'CATV'）は引き続き
// 23514 で弾かれることだけを確認する（NULL の弾かれ方は上の
// TestSchemaV1_ProgramSnapshotChannelIdentityNotNull が担当）。
func TestSchemaV1_ProgramSnapshotChannelTypeCheckSimplified(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO program_snapshots (
			site, program_id, title, start_at, duration_ms,
			network_id, service_id, channel_type, channel, event_id, service_name
		) VALUES ('home', 1, 't', now(), 1800000, 1, 1, 'CATV', '1', 1, 'x')`)
	assertPgError(t, err, "23514")
}

// TestMigration00026_DeletesNullChannelRowsAndCascades は issue #101 の
// 「注意」節が指摘する経路（program_snapshots への FK が ON DELETE CASCADE
// なので、NULL 行の DELETE は理論上 reservations を巻き添えにする）を
// マイグレーション自体で確認する。実運用ではこの経路の対象は 0 行のはずだが
// （書き込み 2 経路はどちらも INNER JOIN で NULL を書けない。issue 本文の
// 裏取り）、00025 までの状態で直接 SQL を使えば NULL 行は作れるので、
// 00026 がそれを DELETE し、CASCADE で参照している reservations 行も
// 一緒に消えることを固定する。
func TestMigration00026_DeletesNullChannelRowsAndCascades(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateReset(ctx, dbURL); err != nil {
		t.Fatalf("migrate reset: %v", err)
	}
	t.Cleanup(func() {
		if err := MigrateReset(ctx, dbURL); err != nil {
			t.Errorf("cleanup migrate reset: %v", err)
		}
	})

	subFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatalf("getting migrations sub-FS: %v", err)
	}
	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, subFS)
	if err != nil {
		t.Fatalf("creating goose provider: %v", err)
	}

	// 00025 まで適用（00026 適用前、チャンネル識別 6 列がまだ nullable な状態）。
	if _, err := provider.UpTo(ctx, 25); err != nil {
		t.Fatalf("migrating up to 00025: %v", err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting pool: %v", err)
	}
	defer pool.Close()

	// 00009/00025 以前の残骸を模す: channel identity が全部 NULL の
	// program_snapshots 行と、それを参照する reservations 行。
	const programID int64 = 918273645
	if _, err := pool.Exec(ctx, `
		INSERT INTO program_snapshots (site, program_id, title, start_at, duration_ms)
		VALUES ('home', $1, '残骸', now(), 1800000)`, programID); err != nil {
		t.Fatalf("inserting legacy null-channel snapshot: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO reservations (site, program_id) VALUES ('home', $1)`, programID); err != nil {
		t.Fatalf("inserting reservation referencing legacy snapshot: %v", err)
	}

	// 00026 を適用: NULL 行が DELETE され、FK CASCADE で reservations も
	// 巻き添えで消える。
	if _, err := provider.UpTo(ctx, 26); err != nil {
		t.Fatalf("migrating up to 00026: %v", err)
	}

	var snapshotExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM program_snapshots WHERE site = 'home' AND program_id = $1)`,
		programID).Scan(&snapshotExists); err != nil {
		t.Fatalf("checking program_snapshots: %v", err)
	}
	if snapshotExists {
		t.Error("legacy null-channel program_snapshots row should have been deleted by 00026")
	}

	var reservationExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM reservations WHERE site = 'home' AND program_id = $1)`,
		programID).Scan(&reservationExists); err != nil {
		t.Fatalf("checking reservations: %v", err)
	}
	if reservationExists {
		t.Error("reservation referencing the deleted snapshot should have been cascaded away (FK ON DELETE CASCADE)")
	}

	// NOT NULL が実際に効いていること: 以後は NULL のチャンネル識別で
	// program_snapshots を作れない。
	_, err = pool.Exec(ctx, `
		INSERT INTO program_snapshots (site, program_id, title, start_at, duration_ms)
		VALUES ('home', $1, '新規残骸', now(), 1800000)`, programID+1)
	assertPgError(t, err, "23502")
}

func TestSchemaV1_MediaAssetProfileKindCheck(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	recID := insertTestRecording(t, pool)

	// original + profile=NULL: OK
	_, err := pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes) VALUES ($1, 'original', 'test.m2ts', 1024)`, recID)
	if err != nil {
		t.Fatalf("original with NULL profile should succeed: %v", err)
	}

	// encoded + profile=NULL: NG
	_, err = pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes) VALUES ($1, 'encoded', 'test.mp4', 512)`, recID)
	assertPgError(t, err, "23514")

	// encoded + profile='h265': OK
	_, err = pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes)
		 VALUES ($1, 'encoded', 'h265', 'test_h265.mp4', 512)`, recID)
	if err != nil {
		t.Fatalf("encoded with profile should succeed: %v", err)
	}

	// original + profile='x': NG
	_, err = pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes)
		 VALUES ($1, 'original', 'x', 'test2.m2ts', 1024)`, recID)
	assertPgError(t, err, "23514")
}

func TestSchemaV1_MediaAssetUniqueRelPath(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	recID := insertTestRecording(t, pool)

	_, err := pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes) VALUES ($1, 'original', 'same/path.m2ts', 1024)`, recID)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	recID2 := insertTestRecording(t, pool)
	// active なアセットとパスが重複: NG
	_, err = pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes) VALUES ($1, 'original', 'same/path.m2ts', 2048)`, recID2)
	assertPgError(t, err, "23505")

	// deleted にすればパス再利用可
	_, err = pool.Exec(ctx,
		`UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE recording_id = $1`, recID)
	if err != nil {
		t.Fatalf("update to deleted: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes) VALUES ($1, 'original', 'same/path.m2ts', 2048)`, recID2)
	if err != nil {
		t.Fatalf("should allow path reuse after tombstone: %v", err)
	}
}

func TestSchemaV1_MediaAssetUniqueNullsNotDistinct(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	recID := insertTestRecording(t, pool)

	// 1 つ目の original (profile=NULL): OK
	_, err := pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
		 VALUES ($1, 'original', 'a.m2ts', 1024)`, recID)
	if err != nil {
		t.Fatalf("first original: %v", err)
	}

	// 同一 recording に 2 つ目の original (profile=NULL) は UNIQUE NULLS NOT DISTINCT で拒否
	_, err = pool.Exec(ctx,
		`INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
		 VALUES ($1, 'original', 'b.m2ts', 2048)`, recID)
	assertPgError(t, err, "23505")
}

func TestSchemaV1_DropStats(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	recID := insertTestRecording(t, pool)

	var assetID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO media_assets (recording_id, kind, rel_path, size_bytes)
		 VALUES ($1, 'original', 'drop_test.m2ts', 4096) RETURNING id`, recID,
	).Scan(&assetID)
	if err != nil {
		t.Fatalf("insert media_asset: %v", err)
	}

	_, err = pool.Exec(ctx,
		`INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		assetID, 256, int64(100000), int64(3), int64(0), int64(0))
	if err != nil {
		t.Fatalf("insert drop_stats: %v", err)
	}

	var ds DropStat
	err = pool.QueryRow(ctx,
		`SELECT media_asset_id, pid, packets, drops, errors, scrambled
		 FROM drop_stats WHERE media_asset_id = $1 AND pid = $2`, assetID, 256,
	).Scan(&ds.MediaAssetID, &ds.PID, &ds.Packets, &ds.Drops, &ds.Errors, &ds.Scrambled)
	if err != nil {
		t.Fatalf("select drop_stats: %v", err)
	}
	if ds.Drops != 3 {
		t.Errorf("drops = %d, want 3", ds.Drops)
	}
}

func TestSchemaV1_RecordSyncFK(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	recID := insertTestRecording(t, pool)

	_, err := pool.Exec(ctx,
		`INSERT INTO record_sync (site, record_id, recording_id, program_id, status)
		 VALUES ($1, $2, $3, $4, $5)`,
		"home", "abcdef1234567890abcdef1234567890", recID, int64(100), "finished")
	if err != nil {
		t.Fatalf("insert record_sync: %v", err)
	}

	// recording 行を物理削除すると record_sync.recording_id は NULL になる（ON DELETE SET NULL）
	_, err = pool.Exec(ctx, `DELETE FROM recordings WHERE id = $1`, recID)
	if err != nil {
		t.Fatalf("delete recording: %v", err)
	}

	var recordingID *int64
	err = pool.QueryRow(ctx,
		`SELECT recording_id FROM record_sync WHERE site = 'home' AND record_id = 'abcdef1234567890abcdef1234567890'`,
	).Scan(&recordingID)
	if err != nil {
		t.Fatalf("select record_sync: %v", err)
	}
	if recordingID != nil {
		t.Errorf("expected NULL recording_id after parent delete, got %d", *recordingID)
	}
}

// TestSchemaV1_ProgramSnapshotGCCascadesToReservationIntentAndOverrides は #27 の
// GC 集約の回帰テスト。旧実装は reservations / program_intents / program_overrides
// それぞれに DeleteEndedReservations / DeleteEndedProgramIntents /
// DeleteEndedProgramOverrides という 3 本の GC クエリがあり、しかも 3 表が
// それぞれ自分のスナップショット列（program_start_at 等）を持っていて既にドリフト
// していたため、表ごとに違う時刻で GC されうる状態だった。#27 でスナップショット
// 列を program_snapshots に一本化し、3 本の GC を DeleteEndedProgramSnapshots
// 1 本（program_snapshots への FK が ON DELETE CASCADE）に統合した。
//
// このテストは 1 回の DeleteEndedProgramSnapshots 呼び出しで、終了 + 猶予を
// 過ぎた番組の reservations / program_intents / program_overrides が同時に
// CASCADE で落ちることを固定する。反対方向として、別サイトの終わっていない
// 番組の 3 表が GC されずに残ることも確認する。
func TestSchemaV1_ProgramSnapshotGCCascadesToReservationIntentAndOverrides(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	const site = "home"
	const programID int64 = 555000555015551

	// 番組は既に終了している（開始 -2h、30 分番組なので終了はとっくに過ぎている）。
	endedStart := time.Now().Add(-2 * time.Hour)
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site: site, ProgramID: programID, Title: "終了番組",
		StartAt: endedStart, DurationMs: 1800000,
		NetworkID: 32736, ServiceID: 1024, ChannelType: "GR", Channel: "27",
		EventID: 1, ServiceName: "テスト局",
	}); err != nil {
		t.Fatalf("upserting program snapshot: %v", err)
	}
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: site, ProgramID: programID,
	}); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: site, ProgramID: programID, Action: "record",
	}); err != nil {
		t.Fatalf("upserting program intent: %v", err)
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: site, ProgramID: programID, Overrides: []byte(`{"priority":5}`),
	}); err != nil {
		t.Fatalf("upserting program overrides: %v", err)
	}

	// 反対方向の対照: 別サイトの、終わっていない番組は GC 対象にならない。
	const otherSite = "office"
	const futureProgramID int64 = 555000555025552
	futureStart := time.Now().Add(2 * time.Hour)
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site: otherSite, ProgramID: futureProgramID, Title: "未来番組",
		StartAt: futureStart, DurationMs: 1800000,
		NetworkID: 32736, ServiceID: 1025, ChannelType: "GR", Channel: "28",
		EventID: 2, ServiceName: "テスト局2",
	}); err != nil {
		t.Fatalf("upserting future program snapshot: %v", err)
	}
	if _, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site: otherSite, ProgramID: futureProgramID,
	}); err != nil {
		t.Fatalf("creating future reservation: %v", err)
	}

	// 事前確認: 終了番組側の 3 表すべてに行がある。
	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: site, ProgramID: programID}); err != nil {
		t.Fatalf("precondition: program_intents row missing: %v", err)
	}
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: site, ProgramID: programID}); err != nil {
		t.Fatalf("precondition: program_overrides row missing: %v", err)
	}
	if _, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{Site: site, ProgramID: programID}); err != nil {
		t.Fatalf("precondition: reservations row missing: %v", err)
	}

	// 核心: DeleteEndedProgramSnapshots を 1 回呼ぶだけで 3 表が CASCADE で
	// 一緒に落ちる（#27 の主要な利益）。
	deleted, err := q.DeleteEndedProgramSnapshots(ctx, time.Now())
	if err != nil {
		t.Fatalf("DeleteEndedProgramSnapshots: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted program_snapshots = %d, want 1 (only the ended program)", deleted)
	}

	if _, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{Site: site, ProgramID: programID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("reservations row should be gone via CASCADE from program_snapshots, err=%v", err)
	}
	if _, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: site, ProgramID: programID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("program_intents row should be gone via CASCADE from program_snapshots, err=%v", err)
	}
	if _, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: site, ProgramID: programID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("program_overrides row should be gone via CASCADE from program_snapshots, err=%v", err)
	}

	// 反対方向: 終わっていない番組の予約は GC されず残る。
	if _, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{Site: otherSite, ProgramID: futureProgramID}); err != nil {
		t.Errorf("future reservation should survive GC, err=%v", err)
	}
}

// TestMigration00027_DropsScheduleSyncReservationID は issue #148: 書き手は
// いるが読み手が本番コードに 1 つも無かった schedule_sync.reservation_id
// （と、その FK・索引）が 00027 で確実に落ちることを見る。
func TestMigration00027_DropsScheduleSyncReservationID(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateReset(ctx, dbURL); err != nil {
		t.Fatalf("migrate reset: %v", err)
	}
	t.Cleanup(func() {
		if err := MigrateReset(ctx, dbURL); err != nil {
			t.Errorf("cleanup migrate reset: %v", err)
		}
	})

	subFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatalf("getting migrations sub-FS: %v", err)
	}
	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, subFS)
	if err != nil {
		t.Fatalf("creating goose provider: %v", err)
	}

	// 00026 まで適用（00027 適用前、reservation_id 列がまだある状態）。
	if _, err := provider.UpTo(ctx, 26); err != nil {
		t.Fatalf("migrating up to 00026: %v", err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting pool: %v", err)
	}
	defer pool.Close()

	const site = "home"
	const programID int64 = 271828182
	if _, err := pool.Exec(ctx, `
		INSERT INTO program_snapshots (
			site, program_id, title, start_at, duration_ms,
			network_id, service_id, channel_type, channel, event_id, service_name
		)
		VALUES ($1, $2, '', now(), 1800000, 32736, 1024, 'GR', '27', 1, 'テスト局')`,
		site, programID); err != nil {
		t.Fatalf("inserting program_snapshot: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO reservations (site, program_id) VALUES ($1, $2)`, site, programID); err != nil {
		t.Fatalf("inserting reservation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO schedule_sync (site, program_id, reservation_id, state, options)
		SELECT $1, $2, id, 'scheduled', '{}'::jsonb FROM reservations WHERE site = $1 AND program_id = $2`,
		site, programID); err != nil {
		t.Fatalf("inserting schedule_sync row with reservation_id set: %v", err)
	}

	var columnExistsBefore bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = 'schedule_sync' AND column_name = 'reservation_id')`,
	).Scan(&columnExistsBefore); err != nil {
		t.Fatalf("checking column exists before 00027: %v", err)
	}
	if !columnExistsBefore {
		t.Fatal("precondition: schedule_sync.reservation_id should exist before 00027")
	}

	// 00027 を適用: 列（と FK・索引）が落ちる。行自体は他の列を保ったまま残る。
	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("migrating up to 00027: %v", err)
	}

	var columnExistsAfter bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = 'schedule_sync' AND column_name = 'reservation_id')`,
	).Scan(&columnExistsAfter); err != nil {
		t.Fatalf("checking column exists after 00027: %v", err)
	}
	if columnExistsAfter {
		t.Error("schedule_sync.reservation_id should have been dropped by 00027")
	}

	var stateAfter string
	if err := pool.QueryRow(ctx,
		`SELECT state FROM schedule_sync WHERE site = $1 AND program_id = $2`, site, programID,
	).Scan(&stateAfter); err != nil {
		t.Fatalf("schedule_sync row should survive the column drop: %v", err)
	}
	if stateAfter != "scheduled" {
		t.Errorf("schedule_sync.state = %q, want %q", stateAfter, "scheduled")
	}

	// 反対方向の確認: 削除した索引を DROP INDEX IF EXISTS で参照しているだけの
	// はずなので、列が無い状態で reservations 側を DELETE しても schedule_sync
	// は（かつての ON DELETE SET NULL の FK 経由ではなく）無関係に残る。
	if _, err := pool.Exec(ctx, `DELETE FROM reservations WHERE site = $1 AND program_id = $2`, site, programID); err != nil {
		t.Fatalf("deleting reservation: %v", err)
	}
	var scheduleSyncExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schedule_sync WHERE site = $1 AND program_id = $2)`, site, programID,
	).Scan(&scheduleSyncExists); err != nil {
		t.Fatalf("checking schedule_sync survives reservation deletion: %v", err)
	}
	if !scheduleSyncExists {
		t.Error("schedule_sync row should survive reservation deletion now that the FK is gone")
	}
}

func TestReservationOptions_Effective(t *testing.T) {
	priority1 := 1
	priority2 := 2
	skip := true
	path := "videos/test.m2ts"
	keepOrig := "untilEncoded"

	profiles := []string{"h265-1080p"}
	base := &ReservationOptions{
		Priority:       &priority1,
		ContentPath:    &path,
		EncodeProfiles: &profiles,
		KeepOriginal:   &keepOrig,
	}
	overrides := &ReservationOptions{
		Skip:     &skip,
		Priority: &priority2,
	}

	eff := overrides.Effective(base)

	if eff.Skip == nil || !*eff.Skip {
		t.Error("skip should be true from overrides")
	}
	if eff.Priority == nil || *eff.Priority != 2 {
		t.Errorf("priority = %v, want 2", eff.Priority)
	}
	if eff.ContentPath == nil || *eff.ContentPath != path {
		t.Error("contentPath should come from base")
	}
	if eff.EncodeProfiles == nil || len(*eff.EncodeProfiles) != 1 || (*eff.EncodeProfiles)[0] != "h265-1080p" {
		t.Error("encodeProfiles should come from base")
	}
	if eff.KeepOriginal == nil || *eff.KeepOriginal != keepOrig {
		t.Error("keepOriginal should come from base")
	}
}

func TestReservationOptions_EffectiveNilBase(t *testing.T) {
	skip := true
	overrides := &ReservationOptions{Skip: &skip}
	eff := overrides.Effective(nil)
	if eff.Skip == nil || !*eff.Skip {
		t.Error("manual reservation: skip should be true from overrides alone")
	}
}

// docs/recording.md §4.2 が定める式は
// effective.skip = (action = 'skip') OR (意図がなく base.skip)
// であり、action が record なら base.skip の値に関わらず false になる。
// M2-6 の重複排除が base.skip を立てても、ユーザーの record 意図が勝つという主張
// （同 §4.2「M2-6 の dedup skip」）はこの分岐に依存している。
//
// intentAction == nil のケースを両方向で押さえているのが要点: 上書きを常に
// 適用する実装にすると base 由来の skip が効かなくなる（重複排除が機能しなくなる）。
func TestEffectiveOptions_IntentActionOverridesBaseSkip(t *testing.T) {
	record := IntentRecord
	skip := IntentSkip
	baseSkip := []byte(`{"skip":true,"priority":3}`)

	tests := []struct {
		name         string
		base         []byte
		intentAction *string
		wantSkip     bool
	}{
		{"record intent beats base.skip", baseSkip, &record, false},
		{"skip intent keeps skip", baseSkip, &skip, true},
		{"no intent lets base.skip through", baseSkip, nil, true},
		{"record intent without base", nil, &record, false},
		{"skip intent without base", nil, &skip, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eff, err := EffectiveOptions(tt.base, nil, tt.intentAction)
			if err != nil {
				t.Fatalf("EffectiveOptions: %v", err)
			}
			got := eff.Skip != nil && *eff.Skip
			if got != tt.wantSkip {
				t.Errorf("effective skip = %v (Skip=%v), want %v", got, eff.Skip, tt.wantSkip)
			}
		})
	}
}

// insertTestRecording は各テスト間で event_id を変えて一意な recording を作る。
var testEventCounter atomic.Int32

func insertTestRecording(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	eventID := testEventCounter.Add(1)
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO recordings (
			site, source, network_id, service_id, event_id,
			service_name, channel_type, channel, title,
			program_start_at, program_duration_ms, status
		 ) VALUES ('home','manual',32736,1024,$1,'NHK総合','GR','27','test',now(),1800000,'finished') RETURNING id`,
		eventID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertTestRecording: %v", err)
	}
	return id
}
