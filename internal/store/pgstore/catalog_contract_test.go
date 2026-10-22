package pgstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCatalogContract(t *testing.T) {
	contracttest.RunCatalog(t, func(t *testing.T) contracttest.CatalogFixture {
		pool := testutil.SetupDB(t)
		catalogStore := NewCatalog(pool)
		return contracttest.CatalogFixture{
			Store: catalogStore,
			InsertRecordingAndAsset: func(ctx context.Context, eventID int32) error {
				recordingID, err := sqlcgen.New(pool).CreateRecording(ctx, sqlcgen.CreateRecordingParams{
					Source: "manual", Site: store.DefaultSite, NetworkID: 32678, ServiceID: 5168,
					EventID: eventID, ServiceName: "ＯＨＫ", ChannelType: "GR", Channel: "27",
					Title: "catalog contract", ProgramStartAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
					ProgramDurationMs: 30 * time.Minute.Milliseconds(), Status: "finished",
				})
				if err != nil {
					return err
				}
				_, err = sqlcgen.New(pool).CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
					RecordingID: recordingID, Kind: store.AssetKindOriginal,
					RelPath:   "catalog-contract/" + time.Unix(int64(eventID), 0).UTC().Format("150405") + ".m2ts",
					SizeBytes: 100,
				})
				return err
			},
			Reset: func(t *testing.T) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), `TRUNCATE recordings, rules, label_rules, program_snapshots RESTART IDENTITY CASCADE`); err != nil {
					t.Fatalf("emptying catalog tables: %v", err)
				}
			},
			FailAtMediaAssetWrite: func(t *testing.T) {
				t.Helper()
				ctx := context.Background()
				if _, err := pool.Exec(ctx, `
CREATE FUNCTION public.catalog_contract_fail_media_asset() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'injected catalog restore failure';
END
$$`); err != nil {
					t.Fatalf("creating catalog failure trigger function: %v", err)
				}
				if _, err := pool.Exec(ctx, `
CREATE TRIGGER catalog_contract_fail_media_asset
BEFORE INSERT ON media_assets
FOR EACH ROW EXECUTE FUNCTION public.catalog_contract_fail_media_asset()`); err != nil {
					t.Fatalf("creating catalog failure trigger: %v", err)
				}
				t.Cleanup(func() {
					if _, err := pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS catalog_contract_fail_media_asset ON media_assets`); err != nil {
						t.Errorf("dropping catalog failure trigger: %v", err)
					}
					if _, err := pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS public.catalog_contract_fail_media_asset()`); err != nil {
						t.Errorf("dropping catalog failure trigger function: %v", err)
					}
				})
			},
			CatalogCounts: func(t *testing.T) (rules, recordings, assets int) {
				t.Helper()
				if err := pool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM rules),
       (SELECT count(*) FROM recordings),
       (SELECT count(*) FROM media_assets)`).Scan(&rules, &recordings, &assets); err != nil {
					t.Fatalf("counting catalog rows after restore: %v", err)
				}
				return rules, recordings, assets
			},
			BeginLabelRuleReevaluationLock: func(t *testing.T) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("beginning label rule re-evaluation lock tx: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback(ctx) })
				if err := sqlcgen.New(tx).LockLabelRuleReevaluation(ctx); err != nil {
					t.Fatalf("acquiring label rule re-evaluation lock: %v", err)
				}
				return func() {
					if err := tx.Commit(ctx); err != nil {
						t.Errorf("releasing label rule re-evaluation lock: %v", err)
					}
				}
			},
			WaitForRestoreLock: func(t *testing.T) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					var pid int
					err := pool.QueryRow(context.Background(), `
SELECT pid FROM pg_locks
WHERE locktype = 'advisory' AND NOT granted
  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
LIMIT 1`).Scan(&pid)
					if errors.Is(err, pgx.ErrNoRows) {
						time.Sleep(10 * time.Millisecond)
						continue
					}
					if err != nil {
						t.Fatalf("finding restore blocked on the re-evaluation lock: %v", err)
					}
					var relationLocks int
					if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_locks
WHERE pid = $1 AND locktype = 'relation' AND granted
  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`, pid).Scan(&relationLocks); err != nil {
						t.Fatalf("checking restore relation locks: %v", err)
					}
					if relationLocks != 0 {
						t.Fatalf("restore waiting for re-evaluation already holds %d protected relation locks", relationLocks)
					}
					return
				}
				t.Fatal("Restore did not wait for the held label rule re-evaluation lock")
			},
		}
	})
}
