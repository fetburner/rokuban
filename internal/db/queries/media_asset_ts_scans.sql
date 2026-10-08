-- 原本 TS の計測記録。行があり scanned_size_bytes が現在値と一致すれば計測済み。
-- name: UpsertMediaAssetTSScan :exec
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
VALUES (sqlc.arg('media_asset_id'), sqlc.arg('scanned_size_bytes'))
ON CONFLICT (media_asset_id) DO UPDATE
SET scanned_size_bytes = EXCLUDED.scanned_size_bytes;

-- 読み出し前に active な原本のパスと期待サイズを取る。
-- name: GetActiveOriginalForTSScan :one
SELECT a.id, a.rel_path, a.size_bytes
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id = sqlc.arg('recording_id')::bigint
  AND a.kind = 'original'
  AND a.state = 'active'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1
    FROM current_ts_scanned_originals s
    WHERE s.media_asset_id = a.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = a.id
  );

-- 読み出し後に原本がまだ有効で同じサイズか、stats の置き換え前に検査する。
-- 長い TS 読み出しの間に in-place 更新や削除があった結果を記録しない。
-- name: LockActiveOriginalForTSScan :one
SELECT a.size_bytes
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.id = sqlc.arg('media_asset_id')::bigint
  AND a.kind = 'original'
  AND a.state = 'active'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1
    FROM current_ts_scanned_originals s
    WHERE s.media_asset_id = a.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = a.id
  )
FOR UPDATE OF a;

-- active な original のうち、計測記録がないか保存サイズが古いものを、
-- reconciler が recording_id の keyset pagination で拾う。
-- name: ListMissingTSScanRecordings :many
SELECT a.recording_id
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id > sqlc.arg('after_recording_id')::bigint
  AND a.kind = 'original'
  AND a.state = 'active'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1
    FROM current_ts_scanned_originals s
    WHERE s.media_asset_id = a.id
  )
  AND NOT EXISTS (
    SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = a.id
  )
ORDER BY a.recording_id
LIMIT sqlc.arg('row_limit');

-- 同じ asset の再計測は置き換える。古い結果を追記で残さない。
-- name: DeleteDropStatsForTSScan :exec
DELETE FROM drop_stats WHERE media_asset_id = sqlc.arg('media_asset_id');

-- name: DeleteDropPositionsForTSScan :exec
DELETE FROM drop_positions WHERE media_asset_id = sqlc.arg('media_asset_id');
