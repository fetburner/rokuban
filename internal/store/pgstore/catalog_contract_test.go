package pgstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestCatalogContract(t *testing.T) {
	contracttest.RunCatalog(t, func(t *testing.T) contracttest.CatalogFixture {
		pool := testutil.SetupDB(t)
		siteNames := []string{"site-b", "default", "site-a", "site-b"}
		catalogStore := NewCatalog(pool, siteNames)
		revisionStore := NewReservations(pool)
		return contracttest.CatalogFixture{
			Store:                  catalogStore,
			RulerInputSites:        []string{"default", "site-a", "site-b"},
			BumpRulerInputRevision: revisionStore.BumpRulerInputRevision,
			GetRulerInputRevision:  revisionStore.GetRulerInputRevision,
			InsertRecordingAndAsset: func(ctx context.Context, eventID int32) error {
				recordingID := dbtest.Recording(t, pool, func(fixtureParams *sqlcgen.CreateRecordingParams) {
					fixtureParams.EventID = eventID
					fixtureParams.Title = "catalog contract"
					fixtureParams.ProgramStartAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
				})

				dbtest.OriginalAsset(t, pool, recordingID, func(fixtureParams *sqlcgen.CreateMediaAssetParams) {
					fixtureParams.RelPath = "catalog-contract/" + time.Unix(int64(eventID), 0).UTC().Format("150405") + ".m2ts"
					fixtureParams.SizeBytes = 100
				})
				return nil
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
			FailAtRulerInputRevisionWrite: func(t *testing.T) {
				t.Helper()
				ctx := context.Background()
				if _, err := pool.Exec(ctx, `CREATE SEQUENCE public.catalog_contract_ruler_revision_write_count`); err != nil {
					t.Fatalf("creating ruler input revision failure sequence: %v", err)
				}
				t.Cleanup(func() {
					if _, err := pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS catalog_contract_fail_ruler_revision ON ruler_input_revisions`); err != nil {
						t.Errorf("dropping ruler input revision failure trigger: %v", err)
					}
					if _, err := pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS public.catalog_contract_fail_ruler_revision()`); err != nil {
						t.Errorf("dropping ruler input revision failure trigger function: %v", err)
					}
					if _, err := pool.Exec(context.Background(), `DROP SEQUENCE IF EXISTS public.catalog_contract_ruler_revision_write_count`); err != nil {
						t.Errorf("dropping ruler input revision failure sequence: %v", err)
					}
				})
				if _, err := pool.Exec(ctx, `
CREATE FUNCTION public.catalog_contract_fail_ruler_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF nextval('public.catalog_contract_ruler_revision_write_count') = 3 THEN
        RAISE EXCEPTION 'injected ruler input revision failure';
    END IF;
    RETURN NEW;
END
$$`); err != nil {
					t.Fatalf("creating ruler input revision failure trigger function: %v", err)
				}
				if _, err := pool.Exec(ctx, `
CREATE TRIGGER catalog_contract_fail_ruler_revision
BEFORE UPDATE ON ruler_input_revisions
FOR EACH ROW EXECUTE FUNCTION public.catalog_contract_fail_ruler_revision()`); err != nil {
					t.Fatalf("creating ruler input revision failure trigger: %v", err)
				}
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

func TestNewCatalogRequiresSiteNames(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewCatalog did not panic without site names")
		}
	}()
	NewCatalog(nil, nil)
}
