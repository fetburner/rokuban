package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

const tsScanBatchSize = 500

// GetActiveOriginalForTSScan は計測が必要な active 原本を返す。
func (s *Media) GetActiveOriginalForTSScan(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetActiveOriginalForTSScan(ctx, sqlcgen.GetActiveOriginalForTSScanParams{
		RecordingID: recordingID, StoreName: s.storeName,
	})
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading active original for TS scan on recording %d", recordingID))
	}
	return store.MediaAsset{ID: row.ID, Store: s.storeName, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// ListMissingTSScanRecordings は計測記録の無い、またはサイズが古い原本をページ単位で返す。
func (s *Media) ListMissingTSScanRecordings(ctx context.Context, afterRecordingID int64, limit int32) ([]int64, error) {
	rows, err := sqlcgen.New(s.pool).ListMissingTSScanRecordings(ctx, sqlcgen.ListMissingTSScanRecordingsParams{
		AfterRecordingID: afterRecordingID,
		RowLimit:         limit,
		StoreName:        s.storeName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing missing TS scan recordings: %w", err)
	}
	return rows, nil
}

// ReplaceScan は現在も有効でサイズが一致する原本の計測値と計測サイズを 1 tx で置き換える。
func (s *Media) ReplaceScan(ctx context.Context, assetID, scannedSizeBytes int64, stats []store.RecordingDropStat, positions []store.RecordingDropPosition) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning TS scan replacement for media asset %d: %w", assetID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	currentSize, err := q.LockActiveOriginalForTSScan(ctx, sqlcgen.LockActiveOriginalForTSScanParams{
		MediaAssetID: assetID, StoreName: s.storeName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking media asset %d before replacing TS scan: %w", assetID, err)
	}
	if currentSize != scannedSizeBytes {
		// 読んでいる間に in-place 登録がファイルを差し替えうる。書かずに成功扱いにする。
		// 古い観測を残しても marker のサイズが現在のサイズとずれたままなので、
		// 次の reconcile が拾い直す。
		return false, nil
	}

	if err := q.DeleteDropStatsForTSScan(ctx, assetID); err != nil {
		return false, fmt.Errorf("deleting drop_stats for media asset %d: %w", assetID, err)
	}
	if err := q.DeleteDropPositionsForTSScan(ctx, assetID); err != nil {
		return false, fmt.Errorf("deleting drop_positions for media asset %d: %w", assetID, err)
	}
	if err := replaceTSScanRows(ctx, q, assetID, stats, positions); err != nil {
		return false, fmt.Errorf("replacing TS scan rows for media asset %d: %w", assetID, mapDatabaseError(err))
	}
	if err := q.UpsertMediaAssetTSScan(ctx, sqlcgen.UpsertMediaAssetTSScanParams{
		MediaAssetID: assetID, ScannedSizeBytes: scannedSizeBytes,
	}); err != nil {
		return false, fmt.Errorf("upserting TS scan marker for media asset %d: %w", assetID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing TS scan replacement for media asset %d: %w", assetID, mapDatabaseError(err))
	}
	return true, nil
}

func replaceTSScanRows(ctx context.Context, q *sqlcgen.Queries, assetID int64, stats []store.RecordingDropStat, positions []store.RecordingDropPosition) error {
	statRows := make([]sqlcgen.InsertDropStatParams, 0, len(stats))
	for _, stat := range stats {
		statRows = append(statRows, sqlcgen.InsertDropStatParams{
			MediaAssetID: assetID,
			Pid:          stat.Pid,
			Packets:      stat.Packets,
			Drops:        stat.Drops,
			Errors:       stat.Errors,
			Scrambled:    stat.Scrambled,
			PidType:      stat.PidType,
		})
	}
	for start := 0; start < len(statRows); start += tsScanBatchSize {
		if err := execTSScanBatch(q.InsertDropStat(ctx, statRows[start:min(start+tsScanBatchSize, len(statRows))])); err != nil {
			return fmt.Errorf("inserting drop_stats batch: %w", err)
		}
	}

	positionRows := make([]sqlcgen.InsertDropPositionParams, 0, len(positions))
	for _, position := range positions {
		positionRows = append(positionRows, sqlcgen.InsertDropPositionParams{
			MediaAssetID: assetID,
			ByteOffset:   position.ByteOffset,
			Pid:          position.Pid,
			ElapsedMs:    position.ElapsedMs,
		})
	}
	for start := 0; start < len(positionRows); start += tsScanBatchSize {
		if err := execTSScanBatch(q.InsertDropPosition(ctx, positionRows[start:min(start+tsScanBatchSize, len(positionRows))])); err != nil {
			return fmt.Errorf("inserting drop_positions batch: %w", err)
		}
	}
	return nil
}

type tsScanBatchResults interface {
	Exec(func(int, error))
	Close() error
}

func execTSScanBatch(batch tsScanBatchResults) error {
	var firstErr error
	batch.Exec(func(i int, err error) {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("batch item %d: %w", i, err)
		}
	})
	if err := batch.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing batch: %w", err)
	}
	return firstErr
}
