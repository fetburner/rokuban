-- 削除 reconcile（M3-8、docs/storage.md §7）。物理 unlink に至る 3 ソース
-- （ごみ箱 / until_encoded / 孤児）を 1 本のループに統一するためのクエリ群。
--
-- 削除プロトコルは冪等: active → deleting → deleted。deleting のまま
-- プロセスが落ちても ListMediaAssetsPendingDelete が次パスで拾い直す。

-- 前パスで deleting にマークしたまま unlink できずに終わった行を拾い直す。
-- 「既に決めた削除」の再実行であり、新規の判断ではないのでブレーカーの対象外。
-- name: ListMediaAssetsPendingDelete :many
SELECT id, recording_id, rel_path, size_bytes, kind
FROM media_assets
WHERE state = 'deleting'
ORDER BY id
LIMIT sqlc.arg('row_limit');

-- ごみ箱の猶予超過、または「今すぐ完全削除」（purge_after）の対象。
-- name: ListTrashMediaAssetsToDelete :many
SELECT a.id, a.recording_id, a.rel_path, a.size_bytes, a.kind
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.state = 'active'
  AND r.deleted_at IS NOT NULL
  AND (
    (r.purge_after IS NOT NULL AND r.purge_after <= now())
    OR r.deleted_at <= sqlc.arg('grace_cutoff')::timestamptz
  )
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- keep_original='until_encoded' で、desired な派生物（全 encode_profiles +
-- thumbnail）がすべて active でコミット済みの原本。ごみ箱経由の録画は
-- ListTrashMediaAssetsToDelete 側で扱うのでここでは除外する。
-- name: ListUntilEncodedOriginalsToDelete :many
SELECT a.id, a.recording_id, a.rel_path, a.size_bytes, a.kind
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE a.state = 'active'
  AND a.kind = 'original'
  AND r.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM unnest(r.encode_profiles) AS want(profile)
    WHERE NOT EXISTS (
      SELECT 1 FROM media_assets e
      WHERE e.recording_id = r.id
        AND e.kind = 'encoded'
        AND e.state = 'active'
        AND e.profile = want.profile
    )
  )
  AND EXISTS (
    SELECT 1 FROM media_assets t
    WHERE t.recording_id = r.id AND t.kind = 'thumbnail' AND t.state = 'active'
  )
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- name: MarkMediaAssetDeleting :execrows
UPDATE media_assets SET state = 'deleting', updated_at = now()
WHERE id = $1 AND state = 'active';

-- name: MarkMediaAssetDeleted :execrows
UPDATE media_assets SET state = 'deleted', deleted_at = now(), updated_at = now()
WHERE id = $1 AND state = 'deleting';

-- 孤児判定用。テーブル全体を読んで Go 側でファイル一覧と突き合わせる
-- （家庭用サーバー規模の行数を前提。件数が増えたら bloom filter 等の検討対象）。
-- name: ListAllMediaAssetRelPaths :many
SELECT rel_path FROM media_assets;

-- 孤児候補を記録する。既存行があれば first_seen を保持する
-- （DO NOTHING。エイジングの起点は「最初に孤児だと気づいた時刻」）。
-- name: UpsertOrphanFile :exec
INSERT INTO orphan_files (rel_path) VALUES ($1)
ON CONFLICT (rel_path) DO NOTHING;

-- name: DeleteOrphanFile :exec
DELETE FROM orphan_files WHERE rel_path = $1;

-- name: ListAllOrphanFiles :many
SELECT rel_path, first_seen FROM orphan_files;

-- エイジング済みの孤児（first_seen が age_cutoff 以前）。
-- name: ListAgedOrphanFiles :many
SELECT rel_path FROM orphan_files WHERE first_seen <= sqlc.arg('age_cutoff')::timestamptz;
