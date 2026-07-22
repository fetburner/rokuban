package db

import (
	"context"
	"os"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ROKUBAN_TEST_DATABASE_URL not set")
	}
	return url
}

func TestMigrateUpDown(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	if err := MigrateDown(ctx, dbURL); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
}

func TestNewPool(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() {
		_ = MigrateDown(ctx, dbURL)
	})

	cfg := config.DBConfig{
		Host:     "localhost",
		Port:     5432,
		User:     "rokuban",
		Password: "rokuban",
		Database: "rokuban_test",
		SSLMode:  "disable",
	}

	pool, err := NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var value string
	err = pool.QueryRow(ctx, "SELECT value FROM schema_info WHERE key = 'version'").Scan(&value)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if value != "1" {
		t.Errorf("schema_info version = %q, want %q", value, "1")
	}
}

func TestNewPool_ConnectionFailure(t *testing.T) {
	cfg := config.DBConfig{
		Host:     "localhost",
		Port:     59999,
		User:     "nonexistent",
		Password: "nonexistent",
		Database: "nonexistent",
		SSLMode:  "disable",
	}

	ctx := context.Background()
	_, err := NewPool(ctx, cfg)
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
}
