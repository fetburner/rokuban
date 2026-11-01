package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// AcquireIngestCheckpoint は recorder ごとの writer lease を取り、clean handoff だけを再開する。
func (s *Media) AcquireIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, ttl time.Duration) (store.IngestCheckpoint, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.IngestCheckpoint{}, false, fmt.Errorf("beginning ingest checkpoint acquisition: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	ownerID := pgtype.UUID{Bytes: owner, Valid: true}
	inserted, err := q.InsertRecordingIngestCheckpointIfAbsent(ctx, sqlcgen.InsertRecordingIngestCheckpointIfAbsentParams{
		RecordingID: recordingID, Owner: ownerID, TtlSeconds: ttl.Seconds(),
	})
	if err != nil {
		return store.IngestCheckpoint{}, false, fmt.Errorf("inserting ingest checkpoint for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	if inserted == 1 {
		if err := tx.Commit(ctx); err != nil {
			return store.IngestCheckpoint{}, false, fmt.Errorf("committing ingest checkpoint acquisition: %w", mapDatabaseError(err))
		}
		return store.IngestCheckpoint{WriterGeneration: 1}, true, nil
	}
	cur, err := q.LockRecordingIngestCheckpoint(ctx, recordingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.IngestCheckpoint{}, false, nil
	}
	if err != nil {
		return store.IngestCheckpoint{}, false, fmt.Errorf("locking ingest checkpoint for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	if cur.LeaseLive {
		return store.IngestCheckpoint{}, false, nil
	}
	checkpoint := store.IngestCheckpoint{WriterGeneration: cur.WriterGeneration + 1}
	if cur.HandedOff {
		checkpoint = store.IngestCheckpoint{
			WriterGeneration: cur.WriterGeneration,
			Resumed:          true,
			Offset:           cur.OffsetBytes,
			HashState:        cur.HashState,
		}
	}
	if _, err := q.ClaimRecordingIngestCheckpoint(ctx, sqlcgen.ClaimRecordingIngestCheckpointParams{
		RecordingID: recordingID, Owner: ownerID, TtlSeconds: ttl.Seconds(),
		WriterGeneration: checkpoint.WriterGeneration, OffsetBytes: checkpoint.Offset, HashState: checkpoint.HashState,
	}); err != nil {
		return store.IngestCheckpoint{}, false, fmt.Errorf("claiming ingest checkpoint for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return store.IngestCheckpoint{}, false, fmt.Errorf("committing ingest checkpoint claim: %w", mapDatabaseError(err))
	}
	return checkpoint, true, nil
}

// ResetIngestCheckpoint は無効な handoff を捨て、新しい writer 世代を Range 0 から始める。
func (s *Media) ResetIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64) (int64, error) {
	generation, err := sqlcgen.New(s.pool).ResetRecordingIngestCheckpoint(ctx, sqlcgen.ResetRecordingIngestCheckpointParams{
		RecordingID: recordingID, Owner: pgtype.UUID{Bytes: owner, Valid: true}, WriterGeneration: writerGeneration,
	})
	if err != nil {
		return 0, fmt.Errorf("resetting ingest checkpoint for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return generation, nil
}

// SaveIngestCheckpoint は同期・close 済み temp の状態を clean handoff として保存する。
func (s *Media) SaveIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64, offset int64, hashState []byte) error {
	rows, err := sqlcgen.New(s.pool).SaveRecordingIngestCheckpoint(ctx, sqlcgen.SaveRecordingIngestCheckpointParams{
		RecordingID: recordingID, Owner: pgtype.UUID{Bytes: owner, Valid: true},
		WriterGeneration: writerGeneration, OffsetBytes: offset, HashState: hashState,
	})
	if err != nil {
		return fmt.Errorf("saving ingest checkpoint for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	if rows == 0 {
		return store.ErrIngestCheckpointLeaseLost
	}
	return nil
}

// SaveIngestFinalHandoff は全量を書き終えた temp を、lease を残したまま clean handoff として保存する。
// 公開（link）より前に呼ぶ。link 後の commit が失敗しても、次の試行はここから hash を復元できる。
func (s *Media) SaveIngestFinalHandoff(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64, offset int64, hashState []byte) error {
	rows, err := sqlcgen.New(s.pool).SaveRecordingIngestFinalHandoff(ctx, sqlcgen.SaveRecordingIngestFinalHandoffParams{
		RecordingID: recordingID, Owner: pgtype.UUID{Bytes: owner, Valid: true},
		WriterGeneration: writerGeneration, OffsetBytes: offset, HashState: hashState,
	})
	if err != nil {
		return fmt.Errorf("saving final ingest handoff for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	if rows == 0 {
		return store.ErrIngestCheckpointLeaseLost
	}
	return nil
}

// ReleaseIngestCheckpoint は失敗で終わった writer の lease を切り、次の試行を直ぐ始められるようにする。
// handed_off には触らない。公開後の commit 失敗で最終 handoff を残すため。
func (s *Media) ReleaseIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID) error {
	if err := sqlcgen.New(s.pool).ReleaseRecordingIngestCheckpoint(ctx, sqlcgen.ReleaseRecordingIngestCheckpointParams{
		RecordingID: recordingID, Owner: pgtype.UUID{Bytes: owner, Valid: true},
	}); err != nil {
		return fmt.Errorf("releasing ingest checkpoint for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return nil
}

// DiscardIngestCheckpoint は lease を持つ writer の進捗行を消し、checkpoint は lease を切って世代を進める。
// checkpoint 行を消さないのは、次の writer が破棄済みの世代の temp 名を再利用しないようにするため。
func (s *Media) DiscardIngestCheckpoint(ctx context.Context, recordingID int64, owner uuid.UUID, writerGeneration int64) (bool, error) {
	rows, err := sqlcgen.New(s.pool).DiscardRecordingIngestCheckpoint(ctx, sqlcgen.DiscardRecordingIngestCheckpointParams{
		RecordingID: recordingID, Owner: pgtype.UUID{Bytes: owner, Valid: true}, WriterGeneration: writerGeneration,
	})
	if err != nil {
		return false, fmt.Errorf("discarding ingest checkpoint for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return rows > 0, nil
}

// UpdateIngestProgress は writer lease を確認し、見えている temp を新 writer が追い越してから切り替える。
func (s *Media) UpdateIngestProgress(ctx context.Context, progress store.IngestProgress, owner uuid.UUID, writerGeneration int64, ttl time.Duration) error {
	rows, err := sqlcgen.New(s.pool).UpdateRecordingIngestProgress(ctx, sqlcgen.UpdateRecordingIngestProgressParams{
		RecordingID: progress.RecordingID, WrittenBytes: progress.WrittenBytes, ExpectedBytes: progress.ExpectedBytes,
		TempRelPath: progress.TempRelPath, TempStore: s.storeName, TempGeneration: progress.TempGeneration,
		TtlSeconds: ttl.Seconds(), Owner: pgtype.UUID{Bytes: owner, Valid: true}, WriterGeneration: writerGeneration,
	})
	if err != nil {
		return fmt.Errorf("updating ingest progress for recording %d: %w", progress.RecordingID, mapDatabaseError(err))
	}
	if rows == 0 {
		return store.ErrIngestCheckpointLeaseLost
	}
	return nil
}

// GetIngestWriterState は mirakc record に対応する checkpoint と streamer 表示先を返す。
func (s *Media) GetIngestWriterState(ctx context.Context, site, recordID string) (store.IngestWriterState, bool, error) {
	row, err := sqlcgen.New(s.pool).GetIngestWriterStateByMirakcRecord(ctx, sqlcgen.GetIngestWriterStateByMirakcRecordParams{
		Site: site, RecordID: recordID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.IngestWriterState{}, false, nil
	}
	if err != nil {
		return store.IngestWriterState{}, false, fmt.Errorf("getting ingest writer state for %s/%s: %w", site, recordID, mapDatabaseError(err))
	}
	return store.IngestWriterState{
		RecordingID: row.RecordingID, WriterGeneration: row.WriterGeneration,
		TempRelPath: row.TempRelPath, HasProgress: row.HasProgress,
	}, true, nil
}
