package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/store"
)

// lockLeasedCheckpoint は owner と世代が現行で、lease が生きている checkpoint の行を FOR UPDATE で取る。
// 取れなければ store.ErrIngestCheckpointLeaseLost。判定は行を読んだ結果で行い、影響行数は使わない。
func lockLeasedCheckpoint(ctx context.Context, tx *sql.Tx, recordingID int64, owner uuid.UUID, writerGeneration int64) error {
	var locked int64
	err := tx.QueryRowContext(ctx, `
SELECT recording_id FROM recording_ingest_checkpoints
WHERE recording_id = ? AND lease_owner = ? AND writer_generation = ? AND lease_expires_at > CURRENT_TIMESTAMP(6)
FOR UPDATE`, recordingID, owner[:], writerGeneration).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrIngestCheckpointLeaseLost
	}
	if err != nil {
		return fmt.Errorf("locking ingest checkpoint for recording %d: %w", recordingID, err)
	}
	return nil
}

// AcquireIngestCheckpoint は recorder ごとの writer lease を取り、clean handoff だけを再開する。
// 行が無ければ作り、生きた lease があれば acquired=false を返す。
func (s *Media) AcquireIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, ttl time.Duration) (store.IngestCheckpoint, bool, error) {
	var out store.IngestCheckpoint
	acquired := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		// 行の存在そのものを判定に使う。PRIMARY KEY だけが衝突するので、影響行数 1 は新しく作ったことを表す。
		res, err := tx.ExecContext(ctx, `
INSERT INTO recording_ingest_checkpoints (recording_id, lease_owner, lease_expires_at, writer_generation, handed_off, offset_bytes, hash_state)
VALUES (?, ?, CURRENT_TIMESTAMP(6) + INTERVAL ? MICROSECOND, 1, FALSE, 0, NULL)
ON DUPLICATE KEY UPDATE recording_id = recording_id`, recordingID, owner[:], ttl.Microseconds())
		if err != nil {
			return fmt.Errorf("inserting ingest checkpoint for recording %d: %w", recordingID, err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			out = store.IngestCheckpoint{WriterGeneration: 1}
			acquired = true
			return nil
		}
		var cur struct {
			gen       int64
			canonical sql.NullString
			handedOff bool
			offset    int64
			hash      []byte
			live      bool
		}
		err = tx.QueryRowContext(ctx, `
SELECT writer_generation, canonical_rel_path, handed_off, offset_bytes, hash_state, lease_expires_at > CURRENT_TIMESTAMP(6)
FROM recording_ingest_checkpoints WHERE recording_id = ? FOR UPDATE`, recordingID).
			Scan(&cur.gen, &cur.canonical, &cur.handedOff, &cur.offset, &cur.hash, &cur.live)
		if err != nil {
			return fmt.Errorf("locking ingest checkpoint for recording %d: %w", recordingID, err)
		}
		if cur.live {
			return nil
		}
		canonical := cur.canonical.String
		out = store.IngestCheckpoint{WriterGeneration: cur.gen + 1, CanonicalRelPath: canonical}
		if cur.handedOff {
			out = store.IngestCheckpoint{
				WriterGeneration: cur.gen, CanonicalRelPath: canonical,
				Resumed: true, Offset: cur.offset, HashState: cur.hash,
			}
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE recording_ingest_checkpoints
SET lease_owner = ?, lease_expires_at = CURRENT_TIMESTAMP(6) + INTERVAL ? MICROSECOND,
    writer_generation = ?, handed_off = FALSE, offset_bytes = ?, hash_state = ?
WHERE recording_id = ?`, owner[:], ttl.Microseconds(), out.WriterGeneration, out.Offset, nullBytes(out.HashState), recordingID); err != nil {
			return fmt.Errorf("claiming ingest checkpoint for recording %d: %w", recordingID, err)
		}
		acquired = true
		return nil
	})
	if err != nil {
		return store.IngestCheckpoint{}, false, err
	}
	if !acquired {
		return store.IngestCheckpoint{}, false, nil
	}
	return out, true, nil
}

// nullBytes は空の値を NULL の引数にする（hash_state の NULL）。
func nullBytes(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

// ReserveIngestRelPath は canonical path を初回だけチェックポイントへ保存し、保存されている値を返す。
func (s *Media) ReserveIngestRelPath(ctx context.Context, recordingID int64, owner uuid.UUID, relPath string) (string, error) {
	var reserved string
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var canonical sql.NullString
		err := tx.QueryRowContext(ctx, `
SELECT canonical_rel_path FROM recording_ingest_checkpoints
WHERE recording_id = ? AND lease_owner = ? AND lease_expires_at > CURRENT_TIMESTAMP(6)
FOR UPDATE`, recordingID, owner[:]).Scan(&canonical)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrIngestCheckpointLeaseLost
		}
		if err != nil {
			return fmt.Errorf("reserving ingest rel_path for recording %d: %w", recordingID, err)
		}
		if canonical.Valid {
			reserved = canonical.String
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recording_ingest_checkpoints SET canonical_rel_path = ? WHERE recording_id = ?`, relPath, recordingID); err != nil {
			return fmt.Errorf("reserving ingest rel_path for recording %d: %w", recordingID, err)
		}
		reserved = relPath
		return nil
	})
	return reserved, err
}

// ResetIngestCheckpoint は無効な handoff を捨て、新しい writer 世代を Range 0 から始める。新しい世代を返す。
func (s *Media) ResetIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64) (int64, error) {
	next := writerGeneration + 1
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockLeasedCheckpoint(ctx, tx, recordingID, owner, writerGeneration); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
UPDATE recording_ingest_checkpoints
SET writer_generation = ?, handed_off = FALSE, offset_bytes = 0, hash_state = NULL
WHERE recording_id = ?`, next, recordingID)
		if err != nil {
			return fmt.Errorf("resetting ingest checkpoint for recording %d: %w", recordingID, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}

// SaveIngestCheckpoint は同期・close 済み temp の状態を clean handoff として保存し、lease を切る。
func (s *Media) SaveIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64, offset int64, hashState []byte) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockLeasedCheckpoint(ctx, tx, recordingID, owner, writerGeneration); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
UPDATE recording_ingest_checkpoints
SET handed_off = TRUE, offset_bytes = ?, hash_state = ?, lease_expires_at = CURRENT_TIMESTAMP(6)
WHERE recording_id = ?`, offset, hashState, recordingID)
		if err != nil {
			return fmt.Errorf("saving ingest checkpoint for recording %d: %w", recordingID, err)
		}
		return nil
	})
}

// SaveIngestFinalHandoff は全量を書き終えた temp を、lease を残したまま clean handoff として保存する。
// 同じ値の再保存は成功する（判定は lease の行の読み取りで行い、影響行数は使わない）。
func (s *Media) SaveIngestFinalHandoff(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64, offset int64, hashState []byte) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockLeasedCheckpoint(ctx, tx, recordingID, owner, writerGeneration); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
UPDATE recording_ingest_checkpoints
SET handed_off = TRUE, offset_bytes = ?, hash_state = ?
WHERE recording_id = ?`, offset, hashState, recordingID)
		if err != nil {
			return fmt.Errorf("saving final ingest handoff for recording %d: %w", recordingID, err)
		}
		return nil
	})
}

// ReleaseIngestCheckpoint は失敗で終わった writer の lease を切る。handed_off には触らない。
func (s *Media) ReleaseIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx, `
UPDATE recording_ingest_checkpoints SET lease_expires_at = CURRENT_TIMESTAMP(6)
WHERE recording_id = ? AND lease_owner = ?`, recordingID, owner[:]); err != nil {
		return fmt.Errorf("releasing ingest checkpoint for recording %d: %w", recordingID, err)
	}
	return nil
}

// DiscardIngestCheckpoint は lease を持つ writer の進捗行を消し、checkpoint は lease を切って世代を進める。
// 世代を進めるのは、破棄した世代の temp 名を次の writer が再利用しないため。
func (s *Media) DiscardIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64) (bool, error) {
	discarded := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var locked int64
		err := tx.QueryRowContext(ctx, `
SELECT recording_id FROM recording_ingest_checkpoints
WHERE recording_id = ? AND lease_owner = ? AND writer_generation = ? FOR UPDATE`,
			recordingID, owner[:], writerGeneration).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking ingest checkpoint for discard of recording %d: %w", recordingID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recording_ingest_progress WHERE recording_id = ?`, recordingID); err != nil {
			return fmt.Errorf("deleting ingest progress for recording %d: %w", recordingID, err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE recording_ingest_checkpoints
SET writer_generation = writer_generation + 1, lease_expires_at = CURRENT_TIMESTAMP(6),
    handed_off = FALSE, offset_bytes = 0, hash_state = NULL
WHERE recording_id = ?`, recordingID); err != nil {
			return fmt.Errorf("discarding ingest checkpoint for recording %d: %w", recordingID, err)
		}
		discarded = true
		return nil
	})
	return discarded, err
}

// UpdateIngestProgress は writer lease を確認して lease を延ばし、見えている temp を新 writer が追い越してから切り替える。
// 新しい writer の書き込み量が見えている量より小さければ、見えている temp・量・世代は変えない。
// 判定は、lease の行を取った後に進捗の行を読んで 1 回だけ行う。
func (s *Media) UpdateIngestProgress(ctx context.Context, progress store.IngestProgress, owner uuid.UUID, writerGeneration int64, ttl time.Duration) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockLeasedCheckpoint(ctx, tx, progress.RecordingID, owner, writerGeneration); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE recording_ingest_checkpoints SET lease_expires_at = CURRENT_TIMESTAMP(6) + INTERVAL ? MICROSECOND
WHERE recording_id = ?`, ttl.Microseconds(), progress.RecordingID); err != nil {
			return fmt.Errorf("renewing ingest lease for recording %d: %w", progress.RecordingID, err)
		}
		var (
			visiblePath    string
			visibleGen     int64
			visibleWritten int64
		)
		err := tx.QueryRowContext(ctx, `
SELECT temp_rel_path, temp_generation, written_bytes FROM recording_ingest_progress
WHERE recording_id = ? FOR UPDATE`, progress.RecordingID).Scan(&visiblePath, &visibleGen, &visibleWritten)
		if errors.Is(err, sql.ErrNoRows) {
			_, err := tx.ExecContext(ctx, `
INSERT INTO recording_ingest_progress (recording_id, written_bytes, expected_bytes, temp_rel_path, temp_generation, temp_store, observed_at)
VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP(6))`,
				progress.RecordingID, progress.WrittenBytes, progress.ExpectedBytes, progress.TempRelPath, progress.TempGeneration, s.storeName)
			if err != nil {
				return fmt.Errorf("inserting ingest progress for recording %d: %w", progress.RecordingID, err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading ingest progress for recording %d: %w", progress.RecordingID, err)
		}
		if visiblePath == progress.TempRelPath || progress.WrittenBytes >= visibleWritten {
			visibleWritten, visiblePath, visibleGen = progress.WrittenBytes, progress.TempRelPath, progress.TempGeneration
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE recording_ingest_progress
SET written_bytes = ?, temp_rel_path = ?, temp_generation = ?, expected_bytes = ?, observed_at = CURRENT_TIMESTAMP(6)
WHERE recording_id = ?`, visibleWritten, visiblePath, visibleGen, progress.ExpectedBytes, progress.RecordingID); err != nil {
			return fmt.Errorf("updating ingest progress for recording %d: %w", progress.RecordingID, err)
		}
		return nil
	})
}

// UpsertIngestProgress は転送中の進捗を lease なしで記録する。行の存在そのものが「転送中」の主張である。
func (s *Media) UpsertIngestProgress(ctx context.Context, progress store.IngestProgress) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO recording_ingest_progress (recording_id, written_bytes, expected_bytes, temp_rel_path, temp_generation, temp_store, observed_at)
VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP(6)) AS incoming
ON DUPLICATE KEY UPDATE written_bytes = incoming.written_bytes, expected_bytes = incoming.expected_bytes,
    temp_rel_path = incoming.temp_rel_path, temp_generation = incoming.temp_generation,
    temp_store = incoming.temp_store, observed_at = CURRENT_TIMESTAMP(6)`,
		progress.RecordingID, progress.WrittenBytes, progress.ExpectedBytes, progress.TempRelPath, progress.TempGeneration, s.storeName); err != nil {
		return fmt.Errorf("upserting ingest progress for recording %d: %w", progress.RecordingID, err)
	}
	return nil
}

// ClearIngestProgress は録画の ingest 進捗行を削除する。行が無くても成功する。
func (s *Media) ClearIngestProgress(ctx context.Context, recordingID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM recording_ingest_progress WHERE recording_id = ?`, recordingID); err != nil {
		return fmt.Errorf("clearing ingest progress for recording %d: %w", recordingID, err)
	}
	return nil
}

// GetIngestTempState は進捗行の temp パスと世代を返す。行が無ければ nil, nil を返す。
func (s *Media) GetIngestTempState(ctx context.Context, recordingID int64) (*store.IngestTempState, error) {
	var state store.IngestTempState
	err := s.db.QueryRowContext(ctx, `
SELECT temp_rel_path, temp_generation FROM recording_ingest_progress
WHERE recording_id = ? AND temp_store = ?`, recordingID, s.storeName).Scan(&state.TempRelPath, &state.TempGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting ingest temp state for recording %d: %w", recordingID, err)
	}
	return &state, nil
}

// NextIngestTempGeneration は新しい temp ファイルに割り当てる世代を採番する。
// 1 行のカウンタを UPDATE し、同じ接続で LAST_INSERT_ID() を読む。読みは同じ tx の中で行う。
func (s *Media) NextIngestTempGeneration(ctx context.Context) (int64, error) {
	var generation int64
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE ingest_temp_generation_counter SET value = LAST_INSERT_ID(value + 1) WHERE id = 1`); err != nil {
			return fmt.Errorf("allocating ingest temp generation: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT LAST_INSERT_ID()`).Scan(&generation); err != nil {
			return fmt.Errorf("reading ingest temp generation: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return generation, nil
}
