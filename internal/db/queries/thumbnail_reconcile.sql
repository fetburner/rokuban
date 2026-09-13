-- thumbnail の desired（active original）− observed（active thumbnail）を
-- 定期的に埋めるための候補。手動の EnqueueMissingThumbnails とは別のクエリに
-- して、明示的な復旧投入が missing_media_assets のマーカーを迂回できるようにする。
--
-- missing_media_assets に載っている原本は delete_reconcile が実体無しを確認した
-- ものなので、ファイルが戻るまで定期パスからは除外する。マーカーが stale に
-- なった場合は delete_reconcile が消し、次のパスで再び候補になる。
-- name: ListMissingThumbnailRecordings :many
SELECT o.recording_id
FROM media_assets o
JOIN recordings r ON r.id = o.recording_id
WHERE o.recording_id > sqlc.arg('after_recording_id')::bigint
  AND o.kind = 'original'
  AND o.state = 'active'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM media_assets t
    WHERE t.recording_id = o.recording_id
      AND t.kind = 'thumbnail'
      AND t.state = 'active'
  )
  AND NOT EXISTS (
    SELECT 1 FROM missing_media_assets m
    WHERE m.media_asset_id = o.id
  )
ORDER BY o.recording_id
LIMIT sqlc.arg('row_limit');
