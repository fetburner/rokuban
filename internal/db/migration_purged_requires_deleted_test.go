package db

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

func TestPurgedRequiresDeletedMigrationsPreserveUnvalidatedConstraint(t *testing.T) {
	ctx := t.Context()
	dbURL := createScratchDatabase(t, testDatabaseURL(t), "purged_requires_deleted", "TEMPLATE template0")
	migrateUpTo(t, dbURL, 68)
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	insert := `INSERT INTO recordings
 (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
 program_start_at, program_duration_ms, status, purged_at)
 VALUES ('manual', 'default', 1, 1, $1, 'test', 'GR', '1', now(), 1000, 'finished', now())`
	if _, err := pool.Exec(ctx, insert, 1); err != nil {
		t.Fatalf("seeding existing violation: %v", err)
	}
	migrateUpTo(t, dbURL, 69)
	assertConstraintViolation := func(err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "recordings_purged_requires_deleted" {
			t.Fatalf("want recordings_purged_requires_deleted check violation (23514), got %v", err)
		}
	}
	_, err = pool.Exec(ctx, insert, 2)
	assertConstraintViolation(err)
	assertConstraintViolation(MigrateUp(ctx, dbURL))
	var current int64
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		var err error
		current, _, err = p.GetVersions(ctx)
		return err
	}); err != nil {
		t.Fatalf("reading migration version: %v", err)
	}
	if current != 69 {
		t.Fatalf("version after failed validation = %d, want 69", current)
	}
	// 検証失敗後も、追加済みの制約による書き込みの検査は続く。
	_, err = pool.Exec(ctx, insert, 3)
	assertConstraintViolation(err)
}
