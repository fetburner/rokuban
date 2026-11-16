package pgstore

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestReconcileMarkersContract(t *testing.T) {
	contracttest.RunReconcileMarkers(t, func(t *testing.T) contracttest.ReconcileMarkerFixture {
		pool := testutil.SetupDB(t)
		return contracttest.ReconcileMarkerFixture{
			Store: NewReservations(pool),
			FailPassMarkerFor: func(t *testing.T, pass string) {
				rejectReconcileMarkerWrite(t, pool, "reconcile_pass_snapshots", "pass", pass)
			},
			FailDeleteMarkerFor: func(t *testing.T, store string) {
				rejectReconcileMarkerWrite(t, pool, "delete_reconcile_snapshots", "store", store)
			},
		}
	})
}

// rejectReconcileMarkerWrite は table の key 列が value の行への書き込みを拒むトリガーを置く。
// 失敗の形を書き込みの側に作るので、読み取りには影響しない。
func rejectReconcileMarkerWrite(t *testing.T, pool *pgxpool.Pool, table, column, value string) {
	t.Helper()
	ctx := context.Background()
	fn := table + "_reject_write"
	if _, err := pool.Exec(ctx, `
CREATE OR REPLACE FUNCTION `+fn+`() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.`+column+` = `+sqlLiteral(value)+` THEN
    RAISE EXCEPTION 'reconcile marker write rejected for %', NEW.`+column+`;
  END IF;
  RETURN NEW;
END;
$$`); err != nil {
		t.Fatalf("creating reconcile marker trigger function for %s: %v", table, err)
	}
	if _, err := pool.Exec(ctx, `
CREATE TRIGGER `+table+`_reject
BEFORE INSERT OR UPDATE ON `+table+`
FOR EACH ROW EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatalf("creating reconcile marker trigger for %s: %v", table, err)
	}
}
