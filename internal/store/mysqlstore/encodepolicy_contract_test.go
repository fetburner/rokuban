package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/mysqltest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

// TestEncodePolicyContract は MySQL のエンコード方針と試行を共通契約で検査する。
func TestEncodePolicyContract(t *testing.T) {
	t.Parallel()
	contracttest.RunEncodePolicy(t, newEncodePolicyContractFixture)
}

func newEncodePolicyContractFixture(t *testing.T) contracttest.EncodePolicyFixture {
	t.Helper()
	db := mysqltest.SetupDB(t)
	media := NewMediaInStore(db, "tokyo", store.StoragePolicy{})
	beginChapterEdit := func(t *testing.T, recordingID int64, durationMs int64, spans []chapters.Span) func() {
		t.Helper()
		ctx := t.Context()
		return beginFixtureTx(t, db, func(tx *sql.Tx) error {
			if err := lockRecordingState(ctx, tx, recordingID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE recordings SET program_duration_ms = ? WHERE id = ?", durationMs, recordingID); err != nil {
				return fmt.Errorf("setting program duration in chapter fixture: %w", err)
			}
			writeChapterSpansTx(t, tx, recordingID, spans)
			return nil
		})
	}
	return contracttest.EncodePolicyFixture{
		Policy: media, Attempts: media, Reconcile: media, Removals: media,
		Recordings: NewRecordingQueries(db, "tokyo"), CMDetection: NewAnalysis(db, "tokyo"),
		NewRecording: func(t *testing.T) int64 {
			return tsScanContractRecording(t, db, "store encode policy contract fixture")
		},
		NewAsset: func(t *testing.T, recordingID int64, kind string, profile *string) int64 {
			return mediaDeleteAsset(t, db, recordingID, kind, profile, "tokyo")
		},
		MarkOriginalMissing: func(t *testing.T, assetID int64) {
			t.Helper()
			execFixture(t, db, `
INSERT INTO missing_media_assets (media_asset_copy_id)
SELECT id FROM media_asset_copies WHERE media_asset_id = ? AND store = 'tokyo'`, assetID)
		},
		SetRecordingTrashed: func(t *testing.T, recordingID int64) {
			t.Helper()
			execFixture(t, db, "UPDATE recordings SET deleted_at = CURRENT_TIMESTAMP(6) WHERE id = ?", recordingID)
		},
		SetDesired: func(t *testing.T, recordingID int64, profiles []string) {
			t.Helper()
			raw := encodeFixtureProfiles(t, profiles)
			execFixture(t, db, `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles)
VALUES (?, 'always', ?) AS incoming
ON DUPLICATE KEY UPDATE keep_original = incoming.keep_original, encode_profiles = incoming.encode_profiles`, recordingID, raw)
		},
		SetRemovalRequest: func(t *testing.T, recordingID int64, profile string) {
			t.Helper()
			execFixture(t, db, "INSERT INTO encoded_asset_removal_requests (recording_id, profile) VALUES (?, ?)", recordingID, profile)
		},
		HasRemovalRequest: func(t *testing.T, recordingID int64, profile string) bool {
			t.Helper()
			var exists bool
			if err := db.QueryRowContext(t.Context(), `
SELECT EXISTS (SELECT 1 FROM encoded_asset_removal_requests WHERE recording_id = ? AND profile = ?)`,
				recordingID, profile).Scan(&exists); err != nil {
				t.Fatalf("reading removal request: %v", err)
			}
			return exists
		},
		HasPolicyRow: func(t *testing.T, recordingID int64) bool {
			t.Helper()
			var exists bool
			if err := db.QueryRowContext(t.Context(), `
SELECT EXISTS (SELECT 1 FROM recording_processing_policy WHERE recording_id = ?)`, recordingID).Scan(&exists); err != nil {
				t.Fatalf("checking recording policy row: %v", err)
			}
			return exists
		},
		SetAttempt: func(t *testing.T, recordingID int64, profile, state string, count int32) {
			t.Helper()
			execFixture(t, db, `
INSERT INTO recording_encode_attempts (recording_id, profile, state, attempt_count)
VALUES (?, ?, ?, ?)`, recordingID, profile, state, count)
		},
		SetChapterSpans: func(t *testing.T, recordingID int64, durationMs int64, spans []chapters.Span) {
			t.Helper()
			beginChapterEdit(t, recordingID, durationMs, spans)()
		},
		BeginChapterEdit: beginChapterEdit,
		BeginPolicyUpdate: func(t *testing.T, recordingID int64, profiles []string) func() {
			t.Helper()
			raw := encodeFixtureProfiles(t, profiles)
			return beginFixtureTx(t, db, func(tx *sql.Tx) error {
				// UPDATE が方針の行を排他で保持する。
				if _, err := tx.ExecContext(t.Context(), `
UPDATE recording_processing_policy SET encode_profiles = ? WHERE recording_id = ?`, raw, recordingID); err != nil {
					return fmt.Errorf("updating desired profiles in fixture: %w", err)
				}
				return nil
			})
		},
		BeginRemoval: func(t *testing.T, recordingID int64, profile string) func() {
			t.Helper()
			ctx := t.Context()
			return beginFixtureTx(t, db, func(tx *sql.Tx) error {
				if err := lockRecordingState(ctx, tx, recordingID); err != nil {
					return err
				}
				var raw []byte
				if err := tx.QueryRowContext(ctx, `
SELECT encode_profiles FROM recording_processing_policy WHERE recording_id = ? FOR UPDATE`, recordingID).Scan(&raw); err != nil {
					return fmt.Errorf("locking encode policy in removal fixture: %w", err)
				}
				var desired []string
				if err := json.Unmarshal(raw, &desired); err != nil {
					return fmt.Errorf("decoding desired profiles in removal fixture: %w", err)
				}
				remaining := slices.DeleteFunc(desired, func(name string) bool { return name == profile })
				if _, err := tx.ExecContext(ctx, `
UPDATE recording_processing_policy SET encode_profiles = ? WHERE recording_id = ?`,
					encodeFixtureProfiles(t, remaining), recordingID); err != nil {
					return fmt.Errorf("removing desired profile in fixture: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `
DELETE FROM encoded_asset_rebuild_requests WHERE recording_id = ? AND profile = ?`, recordingID, profile); err != nil {
					return fmt.Errorf("clearing rebuild request in removal fixture: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `
INSERT INTO encoded_asset_removal_requests (recording_id, profile) VALUES (?, ?)`, recordingID, profile); err != nil {
					return fmt.Errorf("inserting removal request in fixture: %w", err)
				}
				return nil
			})
		},
		BeginRecordingLock: func(t *testing.T, recordingID int64) func() {
			t.Helper()
			return beginFixtureTx(t, db, func(tx *sql.Tx) error {
				return lockRecordingState(t.Context(), tx, recordingID)
			})
		},
		TryPolicyLock: func(t *testing.T, recordingID int64) {
			t.Helper()
			beginFixtureTx(t, db, func(tx *sql.Tx) error {
				var id int64
				if err := tx.QueryRowContext(t.Context(), `
SELECT recording_id FROM recording_processing_policy WHERE recording_id = ? FOR UPDATE NOWAIT`, recordingID).Scan(&id); err != nil {
					return fmt.Errorf("policy row must be lockable while removal waits for the recording row: %w", err)
				}
				return nil
			})()
		},
		WaitRecordingLock: func(t *testing.T) { waitForRecordingLockStatement(t, db) },
		WaitPolicyLock:    func(t *testing.T) { waitForEncodePolicyLock(t, db) },
	}
}

func encodeFixtureProfiles(t *testing.T, profiles []string) string {
	t.Helper()
	if profiles == nil {
		profiles = []string{}
	}
	raw, err := json.Marshal(profiles)
	if err != nil {
		t.Fatalf("encoding fixture profiles: %v", err)
	}
	return string(raw)
}

// waitForEncodePolicyLock は待機先が方針の行であることを確かめる。
// schema だけで絞る waitForLockWaiter では、録画など他の行の待ちと区別できない。
func waitForEncodePolicyLock(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM performance_schema.data_lock_waits w
    JOIN performance_schema.data_locks l
      ON l.ENGINE = w.ENGINE AND l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
    WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = 'recording_processing_policy'
      AND l.LOCK_TYPE = 'RECORD'
)`).Scan(&waiting); err != nil {
			t.Fatalf("checking the encode policy row lock wait: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no statement waits for an encode policy row: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
