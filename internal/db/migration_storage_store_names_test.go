package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
)

func TestStorageStoreNames(t *testing.T) {
	ctx := context.Background()
	dbURL := createScratchDatabase(t, testDatabaseURL(t), "store_names", "")
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	recordingID := dbtest.Recording(t, pool)
	for _, fixture := range []struct{ table, column, fields, values string }{
		{"orphan_files", "store", "rel_path", "'orphan.ts'"},
		{"storage_sync", "store", "root, path, total_bytes, used_bytes, available_bytes, observed_at", "'media', '/media', 100, 25, 75, now()"},
		{"recording_ingest_progress", "temp_store", "recording_id, written_bytes, temp_rel_path, temp_generation", fmt.Sprintf("%d, 0, '.ingest', 1", recordingID)},
	} {
		t.Run(fixture.table, func(t *testing.T) {
			insert := fmt.Sprintf("INSERT INTO %s (%s, %s) VALUES (%s, $1)", fixture.table, fixture.fields, fixture.column, fixture.values)
			for _, value := range []string{"", "Main", "a/b", " main", "main ", "main_1", "main\n"} {
				_, err := pool.Exec(ctx, insert, value)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != fixture.table+"_"+fixture.column+"_check" {
					t.Fatalf("store %q: got %v, want store CHECK violation", value, err)
				}
			}
			_, err := pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", fixture.table, fixture.fields, fixture.values))
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.ColumnName != fixture.column {
				t.Fatalf("omitted store: got %v, want NOT NULL violation", err)
			}
			for _, value := range []string{"main", "store-01", "0", "-"} {
				if _, err := pool.Exec(ctx, insert, value); err != nil {
					t.Fatalf("valid store %q: %v", value, err)
				}
				if _, err := pool.Exec(ctx, "DELETE FROM "+fixture.table); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestStorageStoreNamesMigrationRefusesUnassignedRows(t *testing.T) {
	for _, table := range []string{"orphan_files", "storage_sync", "recording_ingest_progress"} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			dbURL := createScratchDatabase(t, testDatabaseURL(t), "store_upgrade", "")
			migrateUpTo(t, dbURL, 64)
			pool, err := pgxpool.New(ctx, dbURL)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			recordingID := dbtest.Recording(t, pool)
			column := "store"
			var insert string
			switch table {
			case "orphan_files":
				insert = "INSERT INTO orphan_files (rel_path) VALUES ('legacy.ts')"
			case "storage_sync":
				insert = "INSERT INTO storage_sync (root, path, total_bytes, used_bytes, available_bytes, observed_at) VALUES ('media', '/media', 100, 25, 75, now())"
			case "recording_ingest_progress":
				column = "temp_store"
				insert = fmt.Sprintf("INSERT INTO recording_ingest_progress (recording_id, written_bytes, temp_rel_path, temp_generation) VALUES (%d, 7, '.legacy', 1)", recordingID)
			}
			if _, err := pool.Exec(ctx, insert); err != nil {
				t.Fatal(err)
			}
			err = MigrateUp(ctx, dbURL)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != table+"_"+column+"_check" {
				t.Fatalf("migration with legacy row: got %v, want store CHECK violation", err)
			}
			var count int
			if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s = ''", table, column)).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("legacy row count = %d, want 1 (no silent assignment/deletion)", count)
			}
			var version int64
			if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != 64 {
				t.Fatalf("version = %d after refusal, want 64", version)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND (table_name, column_name) IN (('orphan_files','store'), ('storage_sync','store'), ('recording_ingest_progress','temp_store')) AND column_default IS NOT NULL`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 3 {
				t.Fatalf("defaults after refused migration = %d, want 3 (transaction rollback)", count)
			}
			// Explicit operator assignment stands in for the preceding release's adopt.
			if _, err := pool.Exec(ctx, fmt.Sprintf("UPDATE %s SET %s = 'main'", table, column)); err != nil {
				t.Fatal(err)
			}
			if err := MigrateUp(ctx, dbURL); err != nil {
				t.Fatalf("migration after assignment: %v", err)
			}
			if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s = 'main'", table, column)).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("assigned rows = %d, want 1", count)
			}
			migrateDownTo(t, dbURL, 64)
			if err := MigrateUp(ctx, dbURL); err != nil {
				t.Fatalf("reapply after down: %v", err)
			}
		})
	}
}
