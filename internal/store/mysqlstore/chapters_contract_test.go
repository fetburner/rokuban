package mysqlstore

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestChapterEditsContract(t *testing.T) {
	t.Parallel()
	contracttest.Run(t, func(t *testing.T) contracttest.Fixture {
		db := mysqltest.SetupDB(t)
		var eventSequence atomic.Int32
		newRecording := func(t *testing.T, o contracttest.RecordingOptions) int64 {
			return newContractRecording(t, db, o, &eventSequence)
		}
		return contracttest.Fixture{
			Store:        NewChapterEdits(db),
			NewRecording: newRecording,
			SaveCMDetection: func(t *testing.T, id int64, ranges []chapters.Range, material *store.CMDetectionMaterial) {
				t.Helper()
				logo, elements := any(nil), any(nil)
				if material != nil {
					logo = mustJSON(t, nonNilRanges(material.LogoRanges))
					elements = mustJSON(t, append([]chapters.CMElement{}, material.Elements...))
				}
				execFixture(t, db, `
INSERT INTO recording_cm_detections (recording_id, cm_ranges, logo_ranges, structure_elements)
VALUES (?, ?, ?, ?) AS incoming
ON DUPLICATE KEY UPDATE cm_ranges = incoming.cm_ranges, logo_ranges = incoming.logo_ranges,
    structure_elements = incoming.structure_elements, detected_at = CURRENT_TIMESTAMP(6)`,
					id, mustJSON(t, nonNilRanges(ranges)), logo, elements)
			},
			BeginCMDetection: func(t *testing.T, id int64, ranges []chapters.Range) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("beginning detection tx: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback() })
				// 検出 worker の保存 tx と同じ順序（行ロック、結果の書き込み）。
				var locked int64
				if err := tx.QueryRowContext(ctx, `SELECT id FROM recordings WHERE id = ? FOR UPDATE`, id).Scan(&locked); err != nil {
					t.Fatalf("locking recording: %v", err)
				}
				if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_cm_detections (recording_id, cm_ranges, logo_ranges, structure_elements)
VALUES (?, ?, ?, ?)`, id, mustJSON(t, nonNilRanges(ranges)), mustJSON(t, []chapters.Range{}), mustJSON(t, []chapters.CMElement{})); err != nil {
					t.Fatalf("writing CM result: %v", err)
				}
				return func() {
					if err := tx.Commit(); err != nil {
						t.Errorf("committing detection tx: %v", err)
					}
				}
			},
		}
	})
}

// nonNilRanges は nil を空配列として JSON にするために使う（JSON の null は CHECK で禁止）。
func nonNilRanges(ranges []chapters.Range) []chapters.Range {
	return append([]chapters.Range{}, ranges...)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding fixture JSON: %v", err)
	}
	return string(raw)
}
