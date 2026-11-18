package pgstore

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestLoopMarkersContract(t *testing.T) {
	contracttest.RunLoopMarkers(t, func(t *testing.T) contracttest.LoopMarkerFixture {
		pool := testutil.SetupDB(t)
		return contracttest.LoopMarkerFixture{
			Store: NewReservations(pool),
			FailMarkFor: func(t *testing.T, loop, scope string) {
				// 失敗の形は書き込みの側に作る。読み取りには影響しない。
				ctx := context.Background()
				if _, err := pool.Exec(ctx, `
CREATE OR REPLACE FUNCTION reject_loop_marker_write() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.loop = TG_ARGV[0] AND NEW.scope = TG_ARGV[1] THEN
    RAISE EXCEPTION 'loop marker write rejected for % %', NEW.loop, NEW.scope;
  END IF;
  RETURN NEW;
END;
$$`); err != nil {
					t.Fatalf("creating loop marker trigger function: %v", err)
				}
				if _, err := pool.Exec(ctx, `
CREATE TRIGGER reject_loop_marker
BEFORE INSERT OR UPDATE ON loop_success_markers
FOR EACH ROW EXECUTE FUNCTION reject_loop_marker_write(`+sqlLiteral(loop)+`, `+sqlLiteral(scope)+`)`); err != nil {
					t.Fatalf("creating loop marker trigger: %v", err)
				}
			},
		}
	})
}
