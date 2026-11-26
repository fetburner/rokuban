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

// cmDetectionContractStation は CM 検出契約の録画が属する局。
var cmDetectionContractStation = store.CMStationRef{NetworkID: 32678, ServiceID: 5168}

func TestCMDetectionContract(t *testing.T) {
	contracttest.RunCMDetection(t, func(t *testing.T) contracttest.CMDetectionFixture {
		db := mysqltest.SetupDB(t)
		analysis := NewAnalysis(db, cmStationContractStoreName)
		var eventSequence atomic.Int32
		newRecording := func(t *testing.T, originalCopyState string) int64 {
			t.Helper()
			return insertCMDetectionRecording(t, db, eventSequence.Add(1), originalCopyState)
		}
		return contracttest.CMDetectionFixture{
			Store:          analysis,
			ChapterStore:   NewChapterEdits(db),
			CMStationStore: analysis,
			TrashStore:     NewRecordings(db),
			NewRecording: func(t *testing.T) int64 {
				return newRecording(t, "active")
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
			// 録画の行の待ちは、録画の行を FOR UPDATE で取る文として確かめる（待ちの有無だけでは外部キーの検査と区別できない）。
			WaitForRecordingWriteWait: func(t *testing.T, recordingID int64, name string) {
				t.Helper()
				waitForRecordingLockStatement(t, db)
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
					return newRecording(t, "")
				}
				return newRecording(t, "deleted")
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

// TestCMDetectionBeginAttemptFirstRowRace は、行の無い録画への並行する初回の開始が、先に入った行の commit を待ってから
// 既存の行として扱われ、エラーにならないことを確かめる。先の開始の挿入は、開始と同じ文を保持した tx で再現する。
func TestCMDetectionBeginAttemptFirstRowRace(t *testing.T) {
	db := mysqltest.SetupDB(t)
	ctx := context.Background()
	id := insertCMDetectionRecording(t, db, 1, "active")
	held, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("beginning held first attempt: %v", err)
	}
	t.Cleanup(func() { _ = held.Rollback() })
	if _, err := held.ExecContext(ctx, `
INSERT INTO recording_cm_attempts (recording_id, state, stage, error, attempted_at, attempt_count, policy_generation)
VALUES (?, 'running', NULL, NULL, NOW(6), 1, 0)`, id); err != nil {
		t.Fatalf("inserting held first attempt: %v", err)
	}

	type begun struct {
		attempt   store.CMDetectionAttempt
		shouldRun bool
		err       error
	}
	result := make(chan begun, 1)
	go func() {
		attempt, shouldRun, err := NewAnalysis(db, cmStationContractStoreName).BeginAttempt(ctx, id, 3)
		result <- begun{attempt, shouldRun, err}
	}()
	waitLockWaiters(t, db, "recording_cm_attempts", 1)
	if err := held.Commit(); err != nil {
		t.Fatalf("committing held first attempt: %v", err)
	}
	got := <-result
	if got.err != nil {
		t.Fatalf("BeginAttempt() waiting for the first row = %+v, %v; want no error", got.attempt, got.err)
	}
	// 先の行は試行 1 の running なので、後の開始は既存の行として 2 回目になる。
	if !got.shouldRun || got.attempt.AttemptCount != 2 {
		t.Fatalf("BeginAttempt() after the first row = %+v, %v; want running attempt 2", got.attempt, got.shouldRun)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recording_cm_attempts WHERE recording_id = ?`, id).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("attempt rows for recording %d = %d, %v; want 1", id, rows, err)
	}
}

// insertCMDetectionRecording は CM 検出を有効にした録画を 1 件作る。originalCopyState が空なら原本を作らず、
// 空でなければ原本とその物理コピーを、その state で作る。
func insertCMDetectionRecording(t *testing.T, db *sql.DB, eventID int32, originalCopyState string) int64 {
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
		cmDetectionContractStation.NetworkID, cmDetectionContractStation.ServiceID, eventID, genres,
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

// insertFixtureRow は fixture の行を 1 つ挿入し、その ID を返す。
func insertFixtureRow(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	id, err := execFixture(t, db, query, args...).LastInsertId()
	if err != nil {
		t.Fatalf("reading fixture row ID: %v", err)
	}
	return id
}

// waitForRecordingLockStatement は録画の行で待っている文が、録画の行を FOR UPDATE で取る文であることを確かめる。
// 録画を親に持つ表への INSERT も外部キーの検査で録画の行を共有ロックして待つため、待ちの有無だけでは
// 録画の行を取る順序を区別できない。pgstore の waitForCMDetectionRecordingLock が文の内容で同じことを確かめる。
func waitForRecordingLockStatement(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM performance_schema.data_lock_waits w
    JOIN performance_schema.data_locks l
      ON l.ENGINE = w.ENGINE AND l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
    JOIN performance_schema.events_statements_current st
      ON st.THREAD_ID = w.REQUESTING_THREAD_ID
    WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = 'recordings'
      AND st.SQL_TEXT LIKE '%FROM recordings%' AND st.SQL_TEXT LIKE '%FOR UPDATE%'
)`).Scan(&waiting); err != nil {
			t.Fatalf("checking the recording row lock wait: %v", err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no statement waits on a FOR UPDATE of the recording row: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
