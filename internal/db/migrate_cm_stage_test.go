package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

// TestMigrateDown_CMStageResolutionMatch は、新しい工程 resolution / match を持つ試行が
// ある状態でも 00019 の Down が通り、その工程だけが未記録に戻ることを確かめる。
// 旧 CHECK は 2 値を許さず、行を残したままでは再追加が落ちる。
func TestMigrateDown_CMStageResolutionMatch(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() { _ = MigrateReset(ctx, dbURL) })

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// recordings の必須列に依存しないよう、この接続だけ FK 検査を外して試行行を直接作る。
	if _, err := conn.Exec(ctx, "SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable fk checks: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO recording_cm_attempts (recording_id, state, stage)
		VALUES (1, 'failed', 'resolution'), (2, 'failed', 'match'), (3, 'failed', 'logo')`); err != nil {
		t.Fatalf("insert attempts: %v", err)
	}

	// 後続のマイグレーションが増えても 00019 を確実に戻すため、版 18 まで戻す。
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.DownTo(ctx, 18)
		return err
	}); err != nil {
		t.Fatalf("migrate down with resolution/match attempts: %v", err)
	}

	var nulls, logos int
	if err := conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE stage IS NULL),
		count(*) FILTER (WHERE stage = 'logo') FROM recording_cm_attempts`).Scan(&nulls, &logos); err != nil {
		t.Fatalf("query: %v", err)
	}
	if nulls != 2 || logos != 1 {
		t.Errorf("after down: null stages = %d, logo stages = %d, want 2 and 1", nulls, logos)
	}
}
