package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

func TestCMDetectionContract(t *testing.T) {
	contracttest.RunCMDetection(t, func(t *testing.T) contracttest.CMDetectionFixture {
		db := mysqltest.SetupDB(t)
		analysis := NewAnalysis(db, cmStationContractStoreName)
		var eventSequence atomic.Int32
		// newCMDetectionRecording は CM 検出を有効にした録画を作り、生きた原本のコピーを state の形で付ける。
		newCMDetectionRecording := func(t *testing.T, originalCopyState string) int64 {
			t.Helper()
			genres, genreLv1, err := recordingQueryGenres(nil)
			if err != nil {
				t.Fatalf("encoding fixture genres: %v", err)
			}
			id := insertFixtureRow(t, db, `
INSERT INTO recordings (site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, genres, is_free, program_start_at, program_duration_ms, status, source, genre_lv1)
VALUES ('contract-site', ?, ?, ?, 'CM 検出契約 fixture', 'GR', 'contract channel',
    'CM detection contract fixture', ?, TRUE, ?, 1800000, 'finished', 'manual', ?)`,
				cmDetectionContractStation.NetworkID, cmDetectionContractStation.ServiceID, eventSequence.Add(1), genres,
				time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), genreLv1)
			execFixture(t, db, `INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES (?, 'always', JSON_ARRAY(), TRUE)`, id)
			if originalCopyState != "" {
				original := insertFixtureRow(t, db, `INSERT INTO media_assets (recording_id, kind, size_bytes) VALUES (?, 'original', 1000)`, id)
				execFixture(t, db, `INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, generation) VALUES (?, ?, ?, ?, 0)`,
					original, cmStationContractStoreName, fmt.Sprintf("test/%d.ts", id), originalCopyState)
			}
			return id
		}
		return contracttest.CMDetectionFixture{
			Store:          analysis,
			ChapterStore:   NewChapterEdits(db),
			CMStationStore: analysis,
			TrashStore:     NewRecordings(db),
			NewRecording: func(t *testing.T) int64 {
				return newCMDetectionRecording(t, "active")
			},
			SeedLogo: func(t *testing.T, station store.CMStationRef, recordingID int64) {
				t.Helper()
				// 同じ局への 2 回目の書き込みは版（learned_at）を進める。契約は版の変化で結果を拒否させる。
				execFixture(t, db, `
INSERT INTO cm_logos (network_id, service_id, lgd, learned_from, coded_width, coded_height)
VALUES (?, ?, ?, ?, 1440, 1080) AS incoming
ON DUPLICATE KEY UPDATE lgd = incoming.lgd, learned_from = incoming.learned_from, learned_at = CURRENT_TIMESTAMP(6)`,
					station.NetworkID, station.ServiceID, []byte("contract logo"), recordingID)
			},
			BeginChapterWrite: func(t *testing.T, recordingID int64) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("beginning held chapter write: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback() })
				if err := lockRecordingState(ctx, tx, recordingID); err != nil {
					t.Fatalf("locking recording for chapter write: %v", err)
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES (?)`, recordingID); err != nil {
					t.Fatalf("writing chapter ownership: %v", err)
				}
				return func() {
					if err := tx.Commit(); err != nil {
						t.Errorf("committing held chapter write: %v", err)
					}
				}
			},
			BeginLogoAreaWrite: func(t *testing.T, station store.CMStationRef, input store.CMLogoAreaInput) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("beginning held logo area write: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback() })
				if err := lockCMStation(ctx, tx, station); err != nil {
					t.Fatalf("locking station for logo area write: %v", err)
				}
				if _, err := tx.ExecContext(ctx, `
UPDATE cm_logo_areas
SET x = ?, y = ?, w = ?, h = ?, coded_width = ?, coded_height = ?,
    updated_at = DATE_ADD(updated_at, INTERVAL 1 SECOND)
WHERE network_id = ? AND service_id = ?`,
					input.X, input.Y, input.W, input.H, input.CodedWidth, input.CodedHeight,
					station.NetworkID, station.ServiceID); err != nil {
					t.Fatalf("updating logo area: %v", err)
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM cm_logo_candidates WHERE network_id = ? AND service_id = ?`,
					station.NetworkID, station.ServiceID); err != nil {
					t.Fatalf("clearing old logo candidate: %v", err)
				}
				return func() {
					if err := tx.Commit(); err != nil {
						t.Errorf("committing held logo area write: %v", err)
					}
				}
			},
			// 録画の行ロックは録画の行の FOR UPDATE で、局のロックは named_locks の 1 行への待ちとして見える。
			WaitForRecordingWriteWait: func(t *testing.T, recordingID int64, name string) {
				t.Helper()
				waitLockWaiters(t, db, "recordings", 1)
			},
			WaitForStationWriteWait: func(t *testing.T, station store.CMStationRef, name string) {
				t.Helper()
				waitLockWaiters(t, db, "named_locks", 1)
			},
			MarkOriginalMissing: func(t *testing.T, recordingID int64) {
				t.Helper()
				execFixture(t, db, `
INSERT INTO missing_media_assets (media_asset_copy_id)
SELECT copy.id FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE asset.recording_id = ? AND asset.kind = 'original' AND copy.store = ?`, recordingID, cmStationContractStoreName)
			},
			NewRecordingWithoutActiveOriginal: func(t *testing.T, absence contracttest.CMOriginalAbsence) int64 {
				t.Helper()
				if absence == contracttest.CMOriginalNoRow {
					return newCMDetectionRecording(t, "")
				}
				return newCMDetectionRecording(t, "deleted")
			},
			AdvancePolicyGeneration: func(t *testing.T, recordingID int64) {
				t.Helper()
				execFixture(t, db, `UPDATE recording_processing_policy SET cm_detect_generation = cm_detect_generation + 1 WHERE recording_id = ?`, recordingID)
			},
			SetRecordingTimes: func(t *testing.T, recordingID int64, startedAt, endedAt time.Time) {
				t.Helper()
				execFixture(t, db, `UPDATE recordings SET started_at = ?, ended_at = ? WHERE id = ?`, startedAt, endedAt, recordingID)
			},
			HasAttemptRow: func(t *testing.T, recordingID int64) bool {
				t.Helper()
				var exists bool
				if err := db.QueryRowContext(context.Background(),
					`SELECT EXISTS (SELECT 1 FROM recording_cm_attempts WHERE recording_id = ?)`, recordingID).Scan(&exists); err != nil {
					t.Fatalf("checking CM detection attempt row for recording %d: %v", recordingID, err)
				}
				return exists
			},
		}
	})
}

// cmDetectionContractStation は CM 検出契約の録画が属する局。
var cmDetectionContractStation = store.CMStationRef{NetworkID: 32678, ServiceID: 5168}

// insertFixtureRow は fixture の行を 1 つ挿入し、その ID を返す。
func insertFixtureRow(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	id, err := execFixture(t, db, query, args...).LastInsertId()
	if err != nil {
		t.Fatalf("reading fixture row ID: %v", err)
	}
	return id
}
