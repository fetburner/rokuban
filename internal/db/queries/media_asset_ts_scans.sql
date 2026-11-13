-- 原本 TS の計測記録（計測したサイズ）を書く。計測済みの定義は view current_ts_scanned_originals にある。
-- name: UpsertMediaAssetTSScan :exec
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
VALUES (sqlc.arg('media_asset_id'), sqlc.arg('scanned_size_bytes'))
ON CONFLICT (media_asset_id) DO UPDATE
SET scanned_size_bytes = EXCLUDED.scanned_size_bytes;

-- 読み出し前にこの置き場の active な原本コピーのパスと期待サイズを取る。
-- name: GetActiveOriginalForTSScan :one
SELECT a.id, c.rel_path, a.size_bytes
FROM media_assets a
JOIN LATERAL active_present_media_asset_copies(a.id, sqlc.arg('store_name')::text) c ON true
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id = sqlc.arg('recording_id')::bigint
  AND a.kind = 'original'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1
    FROM current_ts_scanned_originals s
    WHERE s.media_asset_id = a.id
  );

-- 読み出し後に原本がまだ有効で同じサイズか、stats の置き換え前に検査する。
-- 長い TS 読み出しの間に in-place 更新や削除があった結果を記録しない。
-- name: LockActiveOriginalForTSScan :one
SELECT a.size_bytes
FROM media_assets a
JOIN media_asset_copies c
  ON c.media_asset_id = a.id
 AND c.store = sqlc.arg('store_name')
 AND c.state = 'active'
JOIN recordings r ON r.id = a.recording_id
WHERE a.id = sqlc.arg('media_asset_id')::bigint
  AND a.kind = 'original'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1
    FROM current_ts_scanned_originals s
    WHERE s.media_asset_id = a.id
  )
  -- 行ロックを c に掛けるため LATERAL 関数にせず、同じ述語の関数を EXISTS で引く。
  AND EXISTS (
    SELECT 1 FROM active_present_media_asset_copies(a.id, sqlc.arg('store_name')::text) p
    WHERE p.id = c.id
  )
FOR UPDATE OF a, c;

-- active な original のうち、計測記録がないか保存サイズが古いものを、
-- reconciler が recording_id の keyset pagination で拾う。
-- name: ListMissingTSScanRecordings :many
SELECT DISTINCT a.recording_id
FROM media_assets a
JOIN LATERAL active_present_media_asset_copies(a.id, sqlc.narg('store_name')::text) c ON true
JOIN recordings r ON r.id = a.recording_id
WHERE a.recording_id > sqlc.arg('after_recording_id')::bigint
  AND a.kind = 'original'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1
    FROM current_ts_scanned_originals s
    WHERE s.media_asset_id = a.id
  )
ORDER BY a.recording_id
LIMIT sqlc.arg('row_limit');

-- 同じ asset の再計測は置き換える。古い結果を追記で残さない。
-- name: DeleteDropStatsForTSScan :exec
DELETE FROM drop_stats WHERE media_asset_id = sqlc.arg('media_asset_id');

-- name: DeleteDropPositionsForTSScan :exec
DELETE FROM drop_positions WHERE media_asset_id = sqlc.arg('media_asset_id');
