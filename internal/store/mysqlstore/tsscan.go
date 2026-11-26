package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/fetburner/rokuban/internal/store"
)

var _ store.TSScan = (*Media)(nil)

// tsScanBatchSize は drop_stats・drop_positions の 1 回の INSERT の行数。
const tsScanBatchSize = 500

// GetActiveOriginalForTSScan は計測が必要な active 原本を返す。対象が無ければ store.ErrNotFound を返す。
func (s *Media) GetActiveOriginalForTSScan(ctx context.Context, recordingID int64) (store.TSScanSource, error) {
	var (
		assetID, sizeBytes, revision int64
		relPath                      string
	)
	err := s.db.QueryRowContext(ctx, `
SELECT a.id, copy.rel_path, a.size_bytes, a.content_revision
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN active_present_media_asset_copies pc ON pc.media_asset_id = a.id AND pc.store = ?
JOIN media_asset_copies copy ON copy.id = pc.id
WHERE a.recording_id = ? AND a.kind = 'original' AND r.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM current_ts_scanned_originals s WHERE s.media_asset_id = a.id)
ORDER BY a.id, copy.id
LIMIT 1`, s.storeName, recordingID).Scan(&assetID, &relPath, &sizeBytes, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return store.TSScanSource{}, fmt.Errorf("active original of recording %d for TS scan: %w", recordingID, store.ErrNotFound)
	}
	if err != nil {
		return store.TSScanSource{}, fmt.Errorf("loading active original for TS scan on recording %d: %w", recordingID, err)
	}
	return store.TSScanSource{
		MediaAsset:      store.MediaAsset{ID: assetID, Store: s.storeName, RelPath: relPath, SizeBytes: sizeBytes},
		ContentRevision: revision,
	}, nil
}

// ListMissingTSScanRecordings は計測記録の無い、またはサイズが古い原本の録画 ID を、recording_id の昇順に limit 件まで返す。
// 束縛された置き場が無ければ、置き場で絞らない。
func (s *Media) ListMissingTSScanRecordings(ctx context.Context, afterRecordingID int64, limit int32) ([]int64, error) {
	var storeArg any
	if s.storeName != "" {
		storeArg = s.storeName
	}
	ids, err := queryRows(ctx, s.db, "missing TS scan recordings", `
SELECT DISTINCT a.recording_id
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN active_present_media_asset_copies pc ON pc.media_asset_id = a.id
WHERE a.recording_id > ? AND a.kind = 'original' AND r.deleted_at IS NULL
  AND (? IS NULL OR pc.store = ?)
  AND NOT EXISTS (SELECT 1 FROM current_ts_scanned_originals s WHERE s.media_asset_id = a.id)
ORDER BY a.recording_id
LIMIT ?`, func(rows *sql.Rows) (int64, error) {
		var id int64
		err := rows.Scan(&id)
		return id, err
	}, afterRecordingID, storeArg, storeArg, limit)
	if err != nil {
		return nil, fmt.Errorf("listing missing TS scan recordings: %w", err)
	}
	return ids, nil
}

// ReplaceScan は読んだ内容のリビジョン・サイズが現在も一致する原本の計測値を 1 tx で置き換える。
// 資産の行を取った後に、active・内容のリビジョン・サイズ・計測の有無を判定し直す。
// 判定が外れたら何も書かずに false, nil を返す。
func (s *Media) ReplaceScan(ctx context.Context, source store.TSScanSource, scannedSizeBytes int64, stats []store.RecordingDropStat, positions []store.RecordingDropPosition) (bool, error) {
	assetID := source.ID
	applied := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		// 判定は FOR UPDATE の行ロックの下で行う。ロックを待った後の状態で判定することは
		// TestTSScanStoreContract/rechecks_active_state_after_waiting_for_the_asset_lock が見る。
		var currentSize int64
		err := tx.QueryRowContext(ctx, `
SELECT a.size_bytes
FROM media_assets a
JOIN media_asset_copies c ON c.media_asset_id = a.id AND c.store = ? AND c.state = 'active'
JOIN recordings r ON r.id = a.recording_id
WHERE a.id = ? AND a.content_revision = ? AND a.kind = 'original' AND r.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM current_ts_scanned_originals s WHERE s.media_asset_id = a.id)
  AND EXISTS (SELECT 1 FROM active_present_media_asset_copies p WHERE p.id = c.id)
FOR UPDATE OF a, c`, s.storeName, assetID, source.ContentRevision).Scan(&currentSize)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking media asset %d before replacing TS scan: %w", assetID, err)
		}
		if currentSize != scannedSizeBytes {
			// 読んでいる間に in-place 登録がファイルを差し替えうる。書かずに成功扱いにする。
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM drop_stats WHERE media_asset_id = ?`, assetID); err != nil {
			return fmt.Errorf("deleting drop_stats for media asset %d: %w", assetID, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM drop_positions WHERE media_asset_id = ?`, assetID); err != nil {
			return fmt.Errorf("deleting drop_positions for media asset %d: %w", assetID, err)
		}
		statRows := make([][]any, 0, len(stats))
		for _, stat := range stats {
			statRows = append(statRows, []any{assetID, stat.Pid, stat.Packets, stat.Drops, stat.Errors, stat.Scrambled, optionalValue(stat.PidType)})
		}
		if err := insertTSScanRows(ctx, tx, "drop_stats", "media_asset_id, pid, packets, drops, errors, scrambled, pid_type", statRows); err != nil {
			return err
		}
		positionRows := make([][]any, 0, len(positions))
		for _, position := range positions {
			positionRows = append(positionRows, []any{assetID, position.ByteOffset, position.Pid, optionalValue(position.ElapsedMs)})
		}
		if err := insertTSScanRows(ctx, tx, "drop_positions", "media_asset_id, byte_offset, pid, elapsed_ms", positionRows); err != nil {
			return err
		}
		// 印のリビジョンは source.ContentRevision を書く。上の判定で a.content_revision = source.ContentRevision を
		// 確かめており、その行は tx の終わりまでロックしているので、書く値は a.content_revision と同じである。
		if _, err := tx.ExecContext(ctx, `
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes, content_revision) VALUES (?, ?, ?) AS new
ON DUPLICATE KEY UPDATE scanned_size_bytes = new.scanned_size_bytes, content_revision = new.content_revision`,
			assetID, scannedSizeBytes, source.ContentRevision); err != nil {
			return fmt.Errorf("upserting TS scan marker for media asset %d: %w", assetID, err)
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("replacing TS scan for media asset %d: %w", assetID, err)
	}
	return applied, nil
}

// insertTSScanRows は行を tsScanBatchSize ごとの複数行 INSERT で書く。行が無ければ何もしない。
func insertTSScanRows(ctx context.Context, tx *sql.Tx, table, columns string, rows [][]any) error {
	for start := 0; start < len(rows); start += tsScanBatchSize {
		batch := rows[start:min(start+tsScanBatchSize, len(rows))]
		tuples := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*len(batch[0]))
		for _, row := range batch {
			tuples = append(tuples, "("+qmarks(len(row))+")")
			args = append(args, row...)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+" ("+columns+") VALUES "+strings.Join(tuples, ", "), args...); err != nil {
			return fmt.Errorf("inserting %s batch: %w", table, err)
		}
	}
	return nil
}
