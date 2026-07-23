package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
)

// DatabaseURL は ROKUBAN_TEST_DATABASE_URL を返す。未設定なら Skip する。
func DatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ROKUBAN_TEST_DATABASE_URL not set")
	}
	return url
}

// SetupDB はマイグレーションを適用してプールを返す。テスト終了時に Reset + Close する。
func SetupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := DatabaseURL(t)
	ctx := context.Background()

	if err := db.MigrateReset(ctx, dbURL); err != nil {
		t.Fatalf("migrate reset: %v", err)
	}
	if err := db.MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() {
		if err := db.MigrateReset(ctx, dbURL); err != nil {
			t.Errorf("cleanup migrate reset: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
