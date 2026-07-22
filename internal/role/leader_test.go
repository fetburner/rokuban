package role

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROKUBAN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ROKUBAN_TEST_DATABASE_URL not set")
	}
	return url
}

func TestTryAcquire_Exclusive(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	pool1, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool1: %v", err)
	}
	defer pool1.Close()

	pool2, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool2: %v", err)
	}
	defer pool2.Close()

	acquired1, release1, err := TryAcquire(ctx, pool1, "ruler")
	if err != nil {
		t.Fatalf("TryAcquire on pool1: %v", err)
	}
	if !acquired1 {
		t.Fatal("expected pool1 to acquire lock")
	}
	defer release1()

	acquired2, _, err := TryAcquire(ctx, pool2, "ruler")
	if err != nil {
		t.Fatalf("TryAcquire on pool2: %v", err)
	}
	if acquired2 {
		t.Fatal("expected pool2 to NOT acquire lock (already held by pool1)")
	}
}

func TestTryAcquire_ReleaseAndReacquire(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	pool1, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool1: %v", err)
	}
	defer pool1.Close()

	pool2, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool2: %v", err)
	}
	defer pool2.Close()

	acquired, release, err := TryAcquire(ctx, pool1, "reconciler")
	if err != nil {
		t.Fatalf("TryAcquire on pool1: %v", err)
	}
	if !acquired {
		t.Fatal("expected pool1 to acquire lock")
	}

	release()

	acquired2, release2, err := TryAcquire(ctx, pool2, "reconciler")
	if err != nil {
		t.Fatalf("TryAcquire on pool2 after release: %v", err)
	}
	if !acquired2 {
		t.Fatal("expected pool2 to acquire lock after pool1 released")
	}
	defer release2()
}

func TestTryAcquire_DifferentRoles(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}
	defer pool.Close()

	acquired1, release1, err := TryAcquire(ctx, pool, "ruler")
	if err != nil {
		t.Fatalf("TryAcquire ruler: %v", err)
	}
	if !acquired1 {
		t.Fatal("expected to acquire ruler lock")
	}
	defer release1()

	acquired2, release2, err := TryAcquire(ctx, pool, "reconciler")
	if err != nil {
		t.Fatalf("TryAcquire reconciler: %v", err)
	}
	if !acquired2 {
		t.Fatal("expected to acquire reconciler lock (different role)")
	}
	defer release2()
}
