package db

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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

func TestSchemaV1_ReservationCRUD(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Microsecond)
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO reservations (site, program_id, source, state, overrides, title, program_start_at, program_duration_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id`,
		"home", int64(327360102415397), "manual", "active", `{"priority":1}`, "テスト番組", now, int64(1800000),
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive id, got %d", id)
	}

	var r Reservation
	err = pool.QueryRow(ctx,
		`SELECT id, site, program_id, source, state, title, program_start_at, program_duration_ms
		 FROM reservations WHERE id = $1`, id,
	).Scan(&r.ID, &r.Site, &r.ProgramID, &r.Source, &r.State, &r.Title, &r.ProgramStartAt, &r.ProgramDurationMs)
	if err != nil {
		t.Fatalf("select reservation: %v", err)
	}
	if r.Site != "home" {
		t.Errorf("site = %q, want %q", r.Site, "home")
	}
	if r.ProgramID != 327360102415397 {
		t.Errorf("program_id = %d, want 327360102415397", r.ProgramID)
	}
	if r.Source != "manual" {
		t.Errorf("source = %q, want %q", r.Source, "manual")
	}
}

func TestSchemaV1_ReservationUniqueSiteProgramID(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	now := time.Now()

	_, err := pool.Exec(ctx,
		`INSERT INTO reservations (site, program_id, source, title, program_start_at, program_duration_ms)
		 VALUES ($1, $2, 'manual', '', $3, 0)`,
		"home", int64(100), now)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// 同一 site + program_id は一意制約違反
	_, err = pool.Exec(ctx,
		`INSERT INTO reservations (site, program_id, source, title, program_start_at, program_duration_ms)
		 VALUES ($1, $2, 'manual', '', $3, 0)`,
		"home", int64(100), now)
	assertPgError(t, err, "23505")

	// 別サイトなら同一 program_id で OK
	_, err = pool.Exec(ctx,
		`INSERT INTO reservations (site, program_id, source, title, program_start_at, program_duration_ms)
		 VALUES ($1, $2, 'manual', '', $3, 0)`,
		"office", int64(100), now)
	if err != nil {
		t.Fatalf("different site should allow same program_id: %v", err)
	}
}

func TestSchemaV1_ReservationSourceCheck(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO reservations (site, program_id, source, title, program_start_at, program_duration_ms)
		 VALUES ('home', 1, 'invalid', '', now(), 0)`)
	assertPgError(t, err, "23514")
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
