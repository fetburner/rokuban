package catalog

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRescueFileRejectsPurgedWithoutDeleted(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	dbtest.Recording(t, pool)
	dbtest.Recording(t, pool)
	catalogStore := pgstoretest.Catalog(pool, "default")
	doc, err := catalogStore.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	doc.Recordings[1].PurgedAt = &now
	doc.Recordings[1].DeletedAt = nil
	mediaDir := newTestMediaDir(t)
	dir, err := Write(mediaDir, doc, 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE recordings RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	result, err := RescueFile(ctx, catalogStore, mediaDir, "test-store", filepath.Join(dir, DocumentFilename))
	var pgErr *pgconn.PgError
	if result != nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "recordings_purged_requires_deleted" || !strings.Contains(err.Error(), "restoring catalog: upserting recording") {
		t.Fatalf("want wrapped check violation and nil result, got %v, %v", result, err)
	}
	t.Logf("RescueFile error: %v", err)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recordings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("recordings after failed rescue = %d, want 0 (whole restore rolled back)", count)
	}
}
