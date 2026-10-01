package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// rescueWriteBarrier parks the production rescue path after its file check and
// before the first asset query, without replacing its filesystem or DB writes.
type rescueWriteBarrier struct {
	query    string
	observed chan struct{}
	resume   chan struct{}
	once     sync.Once
}

func (b *rescueWriteBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, b.query) {
		b.once.Do(func() {
			close(b.observed)
			select {
			case <-b.resume:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}
func (*rescueWriteBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRescueDeletionSerialization(t *testing.T) {
	for _, mode := range []string{"catalog", "scan"} {
		t.Run(mode, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			mediaDir := t.TempDir()
			const relPath = "sites/default/show.m2ts"
			seedRecordingWithOriginal(t, pool, mediaDir, relPath, nil, []byte("original"))
			var assetID int64
			if err := pool.QueryRow(ctx, "SELECT id FROM media_assets WHERE rel_path = $1", relPath).Scan(&assetID); err != nil {
				t.Fatal(err)
			}
			if mode == "catalog" {
				doc, err := catalog.Export(ctx, pool)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := catalog.Write(mediaDir, doc, 7); err != nil {
					t.Fatal(err)
				}
			}
			query := "-- name: CatalogUpsertMediaAsset"
			if mode == "scan" {
				query = "-- name: GetInPlaceAssetByRelPath"
			}
			barrier := &rescueWriteBarrier{query: query, observed: make(chan struct{}), resume: make(chan struct{})}
			cfg := pool.Config()
			cfg.ConnConfig.Tracer = barrier
			rescuePool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer rescuePool.Close()
			var resumeOnce sync.Once
			resume := func() { resumeOnce.Do(func() { close(barrier.resume) }) }
			defer resume()
			done := make(chan error, 1)
			go func() { _, err := catalog.RescueLatest(ctx, rescuePool, mediaDir, []string{"default"}); done <- err }()
			select {
			case <-barrier.observed:
			case err := <-done:
				t.Fatalf("rescue ended before file observation: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			w := &DeleteReconcileWorker{Pool: pool, MediaDir: mediaDir}
			target := deleteTarget{ID: assetID, RelPath: relPath, Kind: "original", SizeBytes: 8}
			deletionCtx, deletionCancel := context.WithTimeout(ctx, 150*time.Millisecond)
			w.deleteMediaAsset(deletionCtx, sqlcgen.New(pool), target, "trash")
			deletionCancel()
			path := filepath.Join(mediaDir, filepath.FromSlash(relPath))
			if _, err := os.Stat(path); err != nil {
				t.Errorf("deletion crossed rescue's file check/write window: %v", err)
			}
			resume()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var state string
			if err := pool.QueryRow(ctx, "SELECT state FROM media_assets WHERE id=$1", assetID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "active" {
				t.Fatalf("state after rescue = %q", state)
			}
			w.deleteMediaAsset(ctx, sqlcgen.New(pool), target, "trash")
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("file after deletion = %v", err)
			}
			if err := pool.QueryRow(ctx, "SELECT state FROM media_assets WHERE id=$1", assetID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != "deleted" {
				t.Fatalf("state after deletion = %q", state)
			}
			if mode == "catalog" {
				result, err := catalog.RescueLatest(ctx, pool, mediaDir, []string{"default"})
				if err != nil {
					t.Fatal(err)
				}
				if result.MissingMediaFiles != 1 {
					t.Fatalf("missing files = %d", result.MissingMediaFiles)
				}
				if err := pool.QueryRow(ctx, "SELECT state FROM media_assets WHERE id=$1", assetID).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state != "deleted" {
					t.Fatalf("stale catalog resurrected asset: %q", state)
				}
			}
		})
	}
}
