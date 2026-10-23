package pgstoretest_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCatalogDefaultsToDefaultSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pgstoretest.Catalog(pool).Restore(ctx, &catalogdoc.Document{Version: catalogdoc.Version}, store.RestoreOptions{}); err != nil {
		t.Fatalf("restoring empty catalog: %v", err)
	}

	assertRulerInputRevision(t, pool, "default", 1)
	assertRulerInputRevision(t, pool, "tokyo", 0)
}

func TestCatalogUsesProvidedSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pgstoretest.Catalog(pool, "tokyo").Restore(ctx, &catalogdoc.Document{Version: catalogdoc.Version}, store.RestoreOptions{}); err != nil {
		t.Fatalf("restoring empty catalog: %v", err)
	}

	assertRulerInputRevision(t, pool, "default", 0)
	assertRulerInputRevision(t, pool, "tokyo", 1)
}

func TestRulesDefaultsToDefaultSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pgstoretest.Rules(pool).Create(ctx, store.RuleInput{Name: "pgstoretest default"}); err != nil {
		t.Fatalf("creating rule: %v", err)
	}

	assertRulerInputRevision(t, pool, "default", 1)
	assertRulerInputRevision(t, pool, "tokyo", 0)
}

func TestRulesUsesProvidedSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pgstoretest.Rules(pool, "tokyo").Create(ctx, store.RuleInput{Name: "pgstoretest site"}); err != nil {
		t.Fatalf("creating rule: %v", err)
	}

	assertRulerInputRevision(t, pool, "default", 0)
	assertRulerInputRevision(t, pool, "tokyo", 1)
}

func assertRulerInputRevision(t *testing.T, pool *pgxpool.Pool, site string, want int64) {
	t.Helper()

	got, err := pgstoretest.Reservations(pool).GetRulerInputRevision(context.Background(), site)
	if err != nil {
		t.Fatalf("getting ruler input revision for %q: %v", site, err)
	}
	if got != want {
		t.Errorf("ruler input revision for %q = %d, want %d", site, got, want)
	}
}
