package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// trashDeletableRecordingSQL は Postgres の関数 trash_deletable_recordings の MySQL 版の述語である。
// r は recordings の別名で、? に猶予の cutoff を 1 つ渡す。
// MySQL には表を返す関数が無く、UPDATE recordings の中で recordings を読むサブクエリは 1093 になるので、
// 述語の形で持つ。将来メディア削除も同じ述語を引く。
const trashDeletableRecordingSQL = `r.deleted_at IS NOT NULL
  AND (
    EXISTS (SELECT 1 FROM recording_purge_requests p WHERE p.recording_id = r.id)
    OR r.deleted_at <= ?
  )`

// SoftDelete は録画を論理削除する。既にごみ箱にある場合も成功し、対象が無ければ ErrNotFound を返す。
func (s *Recordings) SoftDelete(ctx context.Context, recordingID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockRecordingState(ctx, tx, recordingID); err != nil {
			return err
		}
		return softDeleteRecording(ctx, tx, recordingID)
	})
}

// Restore は録画を復元し、同じ録画の即時削除要求を取り下げる。
// 同じ録画の復元と即時削除要求は、録画の行ロックで直列化される。
// 同一放送イベントの生きた録画と衝突する場合は ErrConflict、ごみ箱に無い・完全削除済みの場合は ErrNotFound を返す。
func (s *Recordings) Restore(ctx context.Context, recordingID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		trashed, purged, err := lockRecordingFlags(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		if !trashed || purged {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE recordings
SET deleted_at = NULL, updated_at = CURRENT_TIMESTAMP(6)
WHERE id = ?`, recordingID); err != nil {
			return fmt.Errorf("restoring recording %d: %w", recordingID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recording_purge_requests WHERE recording_id = ?`, recordingID); err != nil {
			return fmt.Errorf("withdrawing purge request for recording %d: %w", recordingID, err)
		}
		return nil
	})
}

// RequestPurge は即時削除を要求し、まだごみ箱に無ければ論理削除も行う。
// 対象が無ければ ErrNotFound を返す。同じ録画の復元と即時削除要求は、録画の行ロックで直列化される。
func (s *Recordings) RequestPurge(ctx context.Context, recordingID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockRecordingState(ctx, tx, recordingID); err != nil {
			return err
		}
		if err := softDeleteRecording(ctx, tx, recordingID); err != nil {
			return err
		}
		// 既に要求の行があれば何もしない。requested_at は最初の要求のまま据え置く。
		// この表の一意キーは主キーだけなので、ON DUPLICATE KEY が他の一意キーの衝突で発火することは無い。
		if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_purge_requests (recording_id)
VALUES (?)
ON DUPLICATE KEY UPDATE recording_id = recording_id`, recordingID); err != nil {
			return fmt.Errorf("requesting purge for recording %d: %w", recordingID, err)
		}
		return nil
	})
}

// MarkPurged は、猶予を過ぎたか即時削除を要求されたごみ箱の録画のうち、削除済みでないコピーが残っていないものを完全削除済みにし、今回確定した録画を返す。
// 候補を録画の行ロック（FOR UPDATE OF r）で読み、各行を purged_at IS NULL の条件つきで更新して、1 行変わった録画だけを返す。
// 並行に呼んだときの振る舞いは未検証（契約テストは逐次の 2 回呼びだけを検査する）。
func (s *Recordings) MarkPurged(ctx context.Context, graceCutoff time.Time) ([]store.PurgedRecording, error) {
	var purged []store.PurgedRecording
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		candidates, err := queryRows(ctx, tx, "purge candidates", `
SELECT r.id, r.site, r.title
FROM recordings r
WHERE r.purged_at IS NULL
  AND `+trashDeletableRecordingSQL+`
  AND NOT EXISTS (
    SELECT 1 FROM media_assets a
    WHERE a.recording_id = r.id
      AND EXISTS (
          SELECT 1 FROM media_asset_copies copy
          WHERE copy.media_asset_id = a.id AND copy.state <> 'deleted'
      )
  )
FOR UPDATE OF r`, scanPurgeCandidate, graceCutoff)
		if err != nil {
			return fmt.Errorf("marking purged recordings: %w", err)
		}
		confirmed := make([]store.PurgedRecording, 0, len(candidates))
		for _, c := range candidates {
			res, err := tx.ExecContext(ctx, `
UPDATE recordings
SET purged_at = ?, updated_at = ?
WHERE id = ? AND purged_at IS NULL`, now, now, c.ID)
			if err != nil {
				return fmt.Errorf("marking recording %d purged: %w", c.ID, err)
			}
			changed, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("reading purged row count for recording %d: %w", c.ID, err)
			}
			if changed == 1 {
				confirmed = append(confirmed, c)
			}
		}
		purged = confirmed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return purged, nil
}

func scanPurgeCandidate(rows *sql.Rows) (store.PurgedRecording, error) {
	var c store.PurgedRecording
	err := rows.Scan(&c.ID, &c.Site, &c.Title)
	return c, err
}

// softDeleteRecording は deleted_at が立っていなければ今の時刻を入れる。既にあれば据え置く。
// SoftDelete と RequestPurge の両方が呼ぶ。呼び出し側が録画の行ロックを先に取る。
func softDeleteRecording(ctx context.Context, tx *sql.Tx, recordingID int64) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE recordings
SET deleted_at = COALESCE(deleted_at, CURRENT_TIMESTAMP(6)), updated_at = CURRENT_TIMESTAMP(6)
WHERE id = ?`, recordingID); err != nil {
		return fmt.Errorf("soft-deleting recording %d: %w", recordingID, err)
	}
	return nil
}

var (
	_ store.RecordingTrash = (*Recordings)(nil)
	_ store.RecordingPurge = (*Recordings)(nil)
)
