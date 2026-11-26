package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/serverevent"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/contracttest"
)

// configureMediaObservationFixtureMySQL は読み取り・観測の契約（RunMedia）が使う状態作りのフックを SQL で実装する。
// 時刻の過去へのずらしは DB の時計（NOW(6)）で行い、store の判定と同じ時計に揃える。
func configureMediaObservationFixtureMySQL(db *sql.DB, f *contracttest.MediaFixture) {
	f.SetRecordingEncodeTimes = func(t *testing.T, recordingID int64, startedAt, endedAt *time.Time) {
		t.Helper()
		execFixture(t, db, `UPDATE recordings SET started_at = ?, ended_at = ? WHERE id = ?`,
			optionalValue(startedAt), optionalValue(endedAt), recordingID)
	}
	f.SetRecordingRemovalTimes = func(t *testing.T, recordingID int64, deletedAt, purgedAt, supersededAt *time.Time) {
		t.Helper()
		execFixture(t, db, `UPDATE recordings SET deleted_at = ?, purged_at = ?, superseded_at = ? WHERE id = ?`,
			optionalValue(deletedAt), optionalValue(purgedAt), optionalValue(supersededAt), recordingID)
	}
	f.SetAssetState = func(t *testing.T, assetID int64, state string) {
		t.Helper()
		execFixture(t, db, `
UPDATE media_asset_copies
SET state = ?, deleted_at = CASE WHEN ? = 'deleted' THEN COALESCE(deleted_at, CURRENT_TIMESTAMP(6)) ELSE NULL END
WHERE media_asset_id = ?`, state, state, assetID)
	}
	f.FailEncodeAttempt = func(t *testing.T, recordingID int64, profile string, attemptCount int32) {
		t.Helper()
		res := execFixture(t, db, `
UPDATE recording_encode_attempts SET state = 'failed', attempt_count = ?
WHERE recording_id = ? AND profile = ?`, attemptCount, recordingID, profile)
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("failing encode attempt for recording %d profile %q changed %d rows (%v); want 1", recordingID, profile, n, err)
		}
	}
	f.NewRecordSync = func(t *testing.T, recordingID int64, site, recordID, status string) {
		t.Helper()
		execFixture(t, db, `
INSERT INTO record_sync (site, record_id, recording_id, program_id, status, tags)
VALUES (?, ?, ?, ?, ?, JSON_ARRAY()) AS incoming
ON DUPLICATE KEY UPDATE recording_id = incoming.recording_id, program_id = incoming.program_id, status = incoming.status`,
			site, recordID, recordingID, recordingID, status)
	}
	f.SetRecordingDropStats = func(t *testing.T, assetID int64, stats []store.RecordingDropStat) {
		t.Helper()
		// 計測済みの判定（current_ts_scanned_originals）は、計測時のサイズと版が原本の現在のものに一致することを要求する。
		execFixture(t, db, `DELETE FROM media_asset_ts_scans WHERE media_asset_id = ?`, assetID)
		execFixture(t, db, `
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes, content_revision)
SELECT id, size_bytes, content_revision FROM media_assets WHERE id = ?`, assetID)
		for _, stat := range stats {
			execFixture(t, db, `
INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled, pid_type)
VALUES (?, ?, ?, ?, ?, ?, ?)`, assetID, stat.Pid, stat.Packets, stat.Drops, stat.Errors, stat.Scrambled, optionalValue(stat.PidType))
		}
	}
	f.SetRecordingDropPositions = func(t *testing.T, assetID int64, positions []store.RecordingDropPosition) {
		t.Helper()
		for _, position := range positions {
			execFixture(t, db, `
INSERT INTO drop_positions (media_asset_id, byte_offset, pid, elapsed_ms) VALUES (?, ?, ?, ?)`,
				assetID, position.ByteOffset, position.Pid, optionalValue(position.ElapsedMs))
		}
	}
	f.SetOrphanFirstSeenAgo = func(t *testing.T, relPath string, ago time.Duration) {
		t.Helper()
		execFixture(t, db, `
UPDATE orphan_files SET first_seen = NOW(6) - INTERVAL ? MICROSECOND
WHERE rel_path_key = UNHEX(SHA2(?, 256))`, ago.Microseconds(), relPath)
	}
	f.SetMissingFirstSeenAgo = func(t *testing.T, copyID int64, ago time.Duration) {
		t.Helper()
		execFixture(t, db, `
UPDATE missing_media_assets SET first_seen = NOW(6) - INTERVAL ? MICROSECOND
WHERE media_asset_copy_id = ?`, ago.Microseconds(), copyID)
	}
	f.StartEncodeProgressListener = func(t *testing.T) func() store.EncodeProgress {
		t.Helper()
		return startEncodeProgressListener(t, db)
	}
}

// startEncodeProgressListener は通知表の読み直しで encode-progress を待つ関数を返す（notifier の mysqlsource と同じ表を読む）。
// 開始時点の最後の id より後の行だけを見る。
func startEncodeProgressListener(t *testing.T, db *sql.DB) func() store.EncodeProgress {
	t.Helper()
	ctx := context.Background()
	var last int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM notifications`).Scan(&last); err != nil {
		t.Fatalf("reading notification baseline: %v", err)
	}
	return func() store.EncodeProgress {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			rows, err := db.QueryContext(ctx, `SELECT id, payload FROM notifications WHERE id > ? ORDER BY id`, last)
			if err != nil {
				t.Fatalf("reading notifications: %v", err)
			}
			for rows.Next() {
				var id int64
				var payload string
				if err := rows.Scan(&id, &payload); err != nil {
					_ = rows.Close()
					t.Fatalf("scanning notification: %v", err)
				}
				last = id
				var event serverevent.EncodeProgressEvent
				if err := json.Unmarshal([]byte(payload), &event); err != nil || event.Type != serverevent.EncodeProgressEventType {
					continue
				}
				_ = rows.Close()
				return store.EncodeProgress{RecordingID: event.RecordingID, Profile: event.Profile, Progress: event.Progress}
			}
			_ = rows.Close()
			if time.Now().After(deadline) {
				t.Fatalf("encode progress notification did not arrive")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
