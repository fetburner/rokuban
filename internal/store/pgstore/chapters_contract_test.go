package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

var contractEventID atomic.Int32

func TestChapterEditsContract(t *testing.T) {
	contracttest.Run(t, func(t *testing.T) contracttest.Fixture {
		pool := testutil.SetupDB(t)
		newRecording := func(t *testing.T, options contracttest.RecordingOptions) int64 {
			t.Helper()
			ctx := context.Background()
			eventID := contractEventID.Add(1)
			id := dbtest.Recording(t, pool, func(fixtureParams *dbtest.RecordingParams) {
				fixtureParams.EventID = eventID
				fixtureParams.Title = "store contract fixture"
			})

			if options.CMDetectionEnabled {
				if _, err := pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', '{}', true)`, id); err != nil {
					t.Fatalf("enabling CM detection: %v", err)
				}
			}
			if options.Trashed {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, id); err != nil {
					t.Fatalf("trashing recording: %v", err)
				}
			}
			if options.Purged {
				if _, err := pool.Exec(ctx, `UPDATE recordings SET purged_at = now() WHERE id = $1`, id); err != nil {
					t.Fatalf("purging recording: %v", err)
				}
			}
			return id
		}
		return contracttest.Fixture{
			Store:        NewAnalysis(pool),
			NewRecording: newRecording,
			SaveCMDetection: func(t *testing.T, id int64, ranges []chapters.Range) {
				t.Helper()
				if err := sqlcgen.New(pool).SaveCMDetection(context.Background(), sqlcgen.SaveCMDetectionParams{
					RecordingID: id,
					CmRanges:    multirange(ranges),
				}); err != nil {
					t.Fatalf("saving CM detection: %v", err)
				}
			},
			BeginCMDetection: func(t *testing.T, id int64, ranges []chapters.Range) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("beginning detection tx: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback(ctx) })
				// 検出 worker の保存 tx と同じ順序（行ロック、結果の書き込み）。
				q := sqlcgen.New(tx)
				if _, err := q.LockRecording(ctx, id); err != nil {
					t.Fatalf("locking recording: %v", err)
				}
				if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: id, CmRanges: multirange(ranges)}); err != nil {
					t.Fatalf("writing CM result: %v", err)
				}
				return func() {
					if err := tx.Commit(ctx); err != nil {
						t.Errorf("committing detection tx: %v", err)
					}
				}
			},
		}
	})
}

// multirange は区間列を int8multirange のテキスト表現にする。
func multirange(ranges []chapters.Range) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = fmt.Sprintf("[%d,%d)", r.StartMs, r.EndMs)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestMapDatabaseErrorMapsUniqueAndRangeConflicts(t *testing.T) {
	for _, code := range []string{"23505", "23P01"} {
		t.Run(code, func(t *testing.T) {
			got := mapDatabaseError(&pgconn.PgError{Code: code})
			if !errors.Is(got, store.ErrConflict) {
				t.Fatalf("mapped error = %v, want ErrConflict", got)
			}
		})
	}
}
