package mysqlstore

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

// cmStationContractStoreName は契約テストの物理コピーの置き場。media_asset_copies.store の形（^[a-z0-9-]+$）に合わせる。
const cmStationContractStoreName = "test-store"

func TestCMStationContract(t *testing.T) {
	t.Parallel()
	contracttest.RunCMStation(t, func(t *testing.T) contracttest.CMStationFixture {
		db := mysqltest.SetupDB(t)
		var eventSequence atomic.Int32
		newStationRecording := func(t *testing.T, station store.CMStationRef) int64 {
			t.Helper()
			genres, genreLv1, err := recordingQueryGenres(nil)
			if err != nil {
				t.Fatalf("encoding fixture genres: %v", err)
			}
			id, err := execFixture(t, db, `
INSERT INTO recordings (site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, genres, is_free, program_start_at, program_duration_ms, status, source, genre_lv1)
VALUES ('contract-site', ?, ?, ?, 'CM 局契約 fixture', 'GR', 'contract channel',
    'CM station contract fixture', ?, TRUE, ?, 1800000, 'finished', 'manual', ?)`,
				station.NetworkID, station.ServiceID, eventSequence.Add(1), genres,
				time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), genreLv1).LastInsertId()
			if err != nil {
				t.Fatalf("reading fixture recording ID: %v", err)
			}
			original, err := execFixture(t, db, `INSERT INTO media_assets (recording_id, kind, size_bytes) VALUES (?, 'original', 1000)`, id).LastInsertId()
			if err != nil {
				t.Fatalf("reading fixture media asset ID: %v", err)
			}
			execFixture(t, db, `INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, generation) VALUES (?, ?, ?, 'active', 0)`,
				original, cmStationContractStoreName, fmt.Sprintf("test/%d.ts", id))
			return id
		}
		defaultStation := store.CMStationRef{NetworkID: 32678, ServiceID: 5168}
		return contracttest.CMStationFixture{
			Store:               NewAnalysis(db, cmStationContractStoreName),
			Detection:           NewAnalysis(db, cmStationContractStoreName),
			NewStationRecording: newStationRecording,
			NewRecording: func(t *testing.T) int64 {
				return newStationRecording(t, defaultStation)
			},
			MarkOriginalMissing: func(t *testing.T, recordingID int64) {
				t.Helper()
				execFixture(t, db, `
INSERT INTO missing_media_assets (media_asset_copy_id)
SELECT copy.id FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE asset.recording_id = ? AND asset.kind = 'original' AND copy.store = ?`, recordingID, cmStationContractStoreName)
			},
			SeedDetection: func(t *testing.T, recordingID int64) {
				t.Helper()
				execFixture(t, db, `INSERT INTO recording_cm_detections (recording_id, cm_ranges) VALUES (?, '[{"startMs": 0, "endMs": 1000}]')`, recordingID)
			},
			SetRecordingNames: func(t *testing.T, recordingID int64, serviceName, site string) {
				t.Helper()
				execFixture(t, db, `UPDATE recordings SET service_name = ?, site = ? WHERE id = ?`, serviceName, site, recordingID)
			},
			EnableCMDetection: func(t *testing.T, recordingID int64) {
				t.Helper()
				execFixture(t, db, `INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES (?, 'always', JSON_ARRAY(), TRUE)`, recordingID)
			},
			SeedFailedAttempt: func(t *testing.T, recordingID int64, stage string, attemptedAt time.Time) {
				t.Helper()
				execFixture(t, db, `
INSERT INTO recording_cm_attempts (recording_id, state, stage, error, attempted_at, attempt_count, policy_generation)
VALUES (?, 'failed', ?, 'injected failure', ?, 1, 0)`, recordingID, stage, attemptedAt)
			},
			BeginStationWrite: func(t *testing.T, station store.CMStationRef) func() {
				t.Helper()
				ctx := context.Background()
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("beginning held station write: %v", err)
				}
				t.Cleanup(func() { _ = tx.Rollback() })
				if err := lockCMStation(ctx, tx, station); err != nil {
					t.Fatalf("locking station: %v", err)
				}
				return func() {
					if err := tx.Commit(); err != nil {
						t.Errorf("committing held station write: %v", err)
					}
				}
			},
			WaitForStationWriteWait: func(t *testing.T, station store.CMStationRef, name string) {
				t.Helper()
				// 局のロック用の行は named_locks の 1 行で、待ちは同じ表への待ちとして見える。
				waitLockWaiters(t, db, "named_locks", 1)
			},
			SeedCandidate: func(t *testing.T, station store.CMStationRef, recordingID int64, areaUpdatedAt, attemptedAt time.Time, state string) {
				t.Helper()
				var lgd any
				if state == "ready" {
					lgd = []byte("candidate logo")
				}
				execFixture(t, db, `
INSERT INTO cm_logo_candidates (network_id, service_id, state, x, y, w, h, coded_width, coded_height,
    recording_id, observed_area_updated_at, attempted_at, lgd)
VALUES (?, ?, ?, 10, 10, 100, 50, 1920, 1080, ?, ?, ?, ?)`,
					station.NetworkID, station.ServiceID, state, recordingID, areaUpdatedAt, attemptedAt, lgd)
			},
			FailCandidateDelete: func(t *testing.T, station store.CMStationRef) func() {
				t.Helper()
				const triggerName = "cmstation_contract_reject_candidate_delete"
				execFixture(t, db, fmt.Sprintf(`CREATE TRIGGER %s BEFORE DELETE ON cm_logo_candidates FOR EACH ROW
BEGIN
    IF OLD.network_id = %d AND OLD.service_id = %d THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected candidate deletion failure';
    END IF;
END`, triggerName, station.NetworkID, station.ServiceID))
				return func() {
					execFixture(t, db, `DROP TRIGGER IF EXISTS cmstation_contract_reject_candidate_delete`)
				}
			},
		}
	})
}
