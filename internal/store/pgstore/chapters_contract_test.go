package pgstore

import (
	"context"
	"encoding/json"
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
				if options.ProgramDurationMs != 0 {
					fixtureParams.ProgramDurationMs = options.ProgramDurationMs
				}
			})

			if options.CMDetectionEnabled {
				if _, err := pool.Exec(ctx, `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
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
				if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = COALESCE(deleted_at, now()), purged_at = now() WHERE id = $1`, id); err != nil {
					t.Fatalf("purging recording: %v", err)
				}
			}
			return id
		}
		return contracttest.Fixture{
			Store:        NewAnalysis(pool, dbtest.StoreName),
			NewRecording: newRecording,
			SaveCMDetection: func(t *testing.T, id int64, ranges []chapters.Range, material *store.CMDetectionMaterial) {
				t.Helper()
				ctx := context.Background()
				logo, elements := "{}", []byte("[]")
				if material != nil {
					logo = multirange(material.LogoRanges)
					// nil のスライスは JSON の null になるので、空配列に揃える。
					encoded, err := json.Marshal(append([]chapters.CMElement{}, material.Elements...))
					if err != nil {
						t.Fatalf("encoding structure elements: %v", err)
					}
					elements = encoded
				}
				if err := sqlcgen.New(pool).SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
					RecordingID:       id,
					CmRanges:          multirange(ranges),
					LogoRanges:        logo,
					StructureElements: elements,
				}); err != nil {
					t.Fatalf("saving CM detection: %v", err)
				}
				if material == nil {
					// 材料を持たない検出は、両方を NULL にして書く（SaveCMDetection のクエリは logo を NULL にできない）。
					if _, err := pool.Exec(ctx, `UPDATE recording_cm_detections SET logo_ranges = NULL, structure_elements = NULL WHERE recording_id = $1`, id); err != nil {
						t.Fatalf("clearing review materials: %v", err)
					}
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
				if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{RecordingID: id, CmRanges: multirange(ranges), LogoRanges: "{}", StructureElements: []byte("[]")}); err != nil {
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
