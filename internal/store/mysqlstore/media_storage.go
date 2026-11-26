package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/serverevent"
	"github.com/fetburner/rokuban/internal/store"
)

var (
	_ store.StorageObservation     = (*Media)(nil)
	_ store.StorageSyncReader      = (*Media)(nil)
	_ store.EncodeProgressReporter = (*Media)(nil)
)

// DeleteStorageSyncExcept は今回の config が要求しない root の観測行を、束縛した置き場の中で削除する。
// roots が空なら、その置き場の全行を消す（NOT IN の空リストは書けないので notInSQL が TRUE に落とす）。
func (s *Media) DeleteStorageSyncExcept(ctx context.Context, roots []string) error {
	cond, args := notInSQL("root", roots)
	args = append([]any{s.storeName}, args...)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM storage_sync WHERE store = ? AND `+cond, args...); err != nil {
		return fmt.Errorf("deleting stale storage roots: %w", err)
	}
	return nil
}

// UpsertStorageSync は 1 root の最新観測値を保存する。
func (s *Media) UpsertStorageSync(ctx context.Context, observation store.StorageObservationInput) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO storage_sync (store, root, path, total_bytes, used_bytes, available_bytes, observed_at)
VALUES (?, ?, ?, ?, ?, ?, NOW(6)) AS incoming
ON DUPLICATE KEY UPDATE path = incoming.path, total_bytes = incoming.total_bytes,
    used_bytes = incoming.used_bytes, available_bytes = incoming.available_bytes, observed_at = NOW(6)`,
		s.storeName, observation.Root, observation.Path, observation.TotalBytes, observation.UsedBytes, observation.AvailableBytes); err != nil {
		return fmt.Errorf("upserting storage root %q: %w", observation.Root, err)
	}
	return nil
}

// ListStorageSync は現在の root 別観測値を返す。置き場が空の読み取りは全置き場の行を返す（監視の読み取り）。
func (s *Media) ListStorageSync(ctx context.Context) ([]store.StorageSync, error) {
	query := `SELECT store, root, path, total_bytes, used_bytes, available_bytes, observed_at FROM storage_sync`
	var args []any
	if s.storeName != "" {
		query += ` WHERE store = ?`
		args = append(args, s.storeName)
	}
	query += ` ORDER BY store, root`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing storage sync: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.StorageSync{}
	for rows.Next() {
		var row store.StorageSync
		if err := rows.Scan(&row.Store, &row.Root, &row.Path, &row.TotalBytes, &row.UsedBytes, &row.AvailableBytes, &row.ObservedAt); err != nil {
			return nil, fmt.Errorf("scanning storage sync: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading storage sync: %w", err)
	}
	return out, nil
}

// ListAllMediaAssetRelPaths は孤児との差集合に使う、この置き場の全コピーのパスを状態に関係なく返す。
func (s *Media) ListAllMediaAssetRelPaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rel_path FROM media_asset_copies WHERE store = ?`, s.storeName)
	if err != nil {
		return nil, fmt.Errorf("listing known media asset rel paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var relPath string
		if err := rows.Scan(&relPath); err != nil {
			return nil, fmt.Errorf("scanning known media asset rel paths: %w", err)
		}
		out = append(out, relPath)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading known media asset rel paths: %w", err)
	}
	return out, nil
}

// UpsertOrphanFile は孤児候補を記録し、既存候補の first_seen を保持する。
func (s *Media) UpsertOrphanFile(ctx context.Context, relPath string) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO orphan_files (store, rel_path) VALUES (?, ?)
ON DUPLICATE KEY UPDATE first_seen = first_seen`, s.storeName, relPath); err != nil {
		return fmt.Errorf("recording orphan path %q: %w", relPath, err)
	}
	return nil
}

// DeleteOrphanFile は解消した孤児候補を消す。
func (s *Media) DeleteOrphanFile(ctx context.Context, relPath string) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM orphan_files WHERE store = ? AND rel_path_key = UNHEX(SHA2(?, 256))`, s.storeName, relPath); err != nil {
		return fmt.Errorf("deleting orphan path %q: %w", relPath, err)
	}
	return nil
}

// ListAllOrphanFiles は記録中の孤児候補を返す。
func (s *Media) ListAllOrphanFiles(ctx context.Context) ([]store.OrphanFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rel_path, first_seen FROM orphan_files WHERE store = ?`, s.storeName)
	if err != nil {
		return nil, fmt.Errorf("listing orphan candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.OrphanFile{}
	for rows.Next() {
		var f store.OrphanFile
		if err := rows.Scan(&f.RelPath, &f.FirstSeen); err != nil {
			return nil, fmt.Errorf("scanning orphan candidates: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading orphan candidates: %w", err)
	}
	return out, nil
}

// ListAgedOrphanFiles は指定期間より前から記録されている孤児候補を返す。経過時間は first_seen と同じ DB の時計で比べる。
func (s *Media) ListAgedOrphanFiles(ctx context.Context, age time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT rel_path FROM orphan_files
WHERE store = ? AND first_seen <= NOW(6) - INTERVAL ? MICROSECOND`, s.storeName, age.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("listing aged orphan candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var relPath string
		if err := rows.Scan(&relPath); err != nil {
			return nil, fmt.Errorf("scanning aged orphan candidates: %w", err)
		}
		out = append(out, relPath)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading aged orphan candidates: %w", err)
	}
	return out, nil
}

// ListActiveMediaAssets は実体無しの差集合に使う、この置き場の全 active コピーを返す。
func (s *Media) ListActiveMediaAssets(ctx context.Context) ([]store.ActiveMediaAsset, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, a.id, a.recording_id, c.rel_path, a.kind
FROM media_assets a
JOIN media_asset_copies c ON c.media_asset_id = a.id
WHERE c.store = ? AND c.state = 'active'`, s.storeName)
	if err != nil {
		return nil, fmt.Errorf("listing active media assets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.ActiveMediaAsset{}
	for rows.Next() {
		var m store.ActiveMediaAsset
		if err := rows.Scan(&m.ID, &m.MediaAssetID, &m.RecordingID, &m.RelPath, &m.Kind); err != nil {
			return nil, fmt.Errorf("scanning active media assets: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading active media assets: %w", err)
	}
	return out, nil
}

// UpsertMissingMediaAsset は実体無し候補を記録し、既存候補の first_seen を保持する。
func (s *Media) UpsertMissingMediaAsset(ctx context.Context, copyID int64) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO missing_media_assets (media_asset_copy_id) VALUES (?) AS incoming
ON DUPLICATE KEY UPDATE media_asset_copy_id = incoming.media_asset_copy_id`, copyID); err != nil {
		return fmt.Errorf("recording missing media asset copy %d: %w", copyID, err)
	}
	return nil
}

// DeleteMissingMediaAsset は実体が再び観測された候補を消す。
func (s *Media) DeleteMissingMediaAsset(ctx context.Context, copyID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM missing_media_assets WHERE media_asset_copy_id = ?`, copyID); err != nil {
		return fmt.Errorf("deleting missing media asset copy %d: %w", copyID, err)
	}
	return nil
}

// ListAllMissingMediaAssetIDs は記録中の実体無し候補 ID を、この置き場のコピーに限って返す。
func (s *Media) ListAllMissingMediaAssetIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.media_asset_copy_id
FROM missing_media_assets m
JOIN media_asset_copies c ON c.id = m.media_asset_copy_id
WHERE c.store = ?`, s.storeName)
	if err != nil {
		return nil, fmt.Errorf("listing missing media asset candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning missing media asset candidates: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading missing media asset candidates: %w", err)
	}
	return out, nil
}

// ListAgedMissingMediaAssets は指定期間より前から見つからない active コピーを返す。経過時間は first_seen と同じ DB の時計で比べる。
func (s *Media) ListAgedMissingMediaAssets(ctx context.Context, age time.Duration) ([]store.AgedMissingMediaAsset, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, a.id, a.recording_id, c.rel_path, a.kind, m.first_seen
FROM missing_media_assets m
JOIN media_asset_copies c ON c.id = m.media_asset_copy_id
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.store = ? AND c.state = 'active'
  AND m.first_seen <= NOW(6) - INTERVAL ? MICROSECOND
ORDER BY m.media_asset_copy_id`, s.storeName, age.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("listing aged missing media asset candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.AgedMissingMediaAsset{}
	for rows.Next() {
		var m store.AgedMissingMediaAsset
		if err := rows.Scan(&m.ID, &m.MediaAssetID, &m.RecordingID, &m.RelPath, &m.Kind, &m.FirstSeen); err != nil {
			return nil, fmt.Errorf("scanning aged missing media asset candidates: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading aged missing media asset candidates: %w", err)
	}
	return out, nil
}

// GetMediaAssetTransferProgressWriterGeneration は、コピーの転送進捗に記録された書き手の世代を返す。行が無ければ ok=false。
func (s *Media) GetMediaAssetTransferProgressWriterGeneration(ctx context.Context, copyID int64) (int64, bool, error) {
	var generation int64
	err := s.db.QueryRowContext(ctx, `SELECT writer_generation FROM media_asset_copy_transfer_progress WHERE copy_id = ?`, copyID).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading transfer progress generation for copy %d: %w", copyID, err)
	}
	return generation, true, nil
}

// ReportEncodeProgress はエンコード進捗を通知表へ 1 行書く。読み手は通知表を読み直す（notifier の mysqlsource と同じ表）。
func (s *Media) ReportEncodeProgress(ctx context.Context, progress store.EncodeProgress) error {
	payload, err := json.Marshal(serverevent.EncodeProgressEvent{
		Type:        serverevent.EncodeProgressEventType,
		RecordingID: progress.RecordingID,
		Profile:     progress.Profile,
		Progress:    progress.Progress,
	})
	if err != nil {
		return fmt.Errorf("encoding encode progress for recording %d profile %q: %w", progress.RecordingID, progress.Profile, err)
	}
	if err := NotifyTopic(ctx, s.db, string(payload)); err != nil {
		return fmt.Errorf("notifying encode progress for recording %d profile %q: %w", progress.RecordingID, progress.Profile, err)
	}
	return nil
}
