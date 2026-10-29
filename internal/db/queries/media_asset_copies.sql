-- Physical copies remain separate from media_assets so their location and lifecycle
-- can evolve without moving the logical asset's satellite rows.

-- 競合したときは state / deleted_at を書かない。生きた行の state を決めるのは削除エンジンの
-- 遷移だけで、再登録や再公開が deleting を active に戻すと unlink 予定のファイルを公開してしまう。
-- name: UpsertMediaAssetCopy :execrows
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at, deleted_at)
VALUES (
    sqlc.arg('media_asset_id'), sqlc.arg('store'), sqlc.arg('rel_path'), sqlc.arg('generation'),
    sqlc.arg('state'), COALESCE(sqlc.narg('created_at')::timestamptz, now()), sqlc.narg('deleted_at')
)
ON CONFLICT (store, rel_path) WHERE state <> 'deleted' DO UPDATE SET
    created_at = COALESCE(sqlc.narg('created_at')::timestamptz, media_asset_copies.created_at)
WHERE media_asset_copies.media_asset_id = EXCLUDED.media_asset_id
  AND media_asset_copies.generation = EXCLUDED.generation;

-- Keep the rolling-migration mirror's lifecycle state in sync without re-reading
-- the logical media_assets columns. The copy row must exist before a transition
-- can start, as enforced by the worker's adoption check and the transition query.
-- name: SetMediaAssetCopyState :execrows
UPDATE media_asset_copies
SET state = sqlc.arg('state'),
    deleted_at = CASE
        WHEN sqlc.arg('state')::text = 'deleted' THEN COALESCE(sqlc.narg('deleted_at')::timestamptz, deleted_at, now())
        ELSE NULL
    END
WHERE id = sqlc.arg('id')
  AND store = sqlc.arg('store_name')
  AND state = sqlc.arg('expected_state');

-- Catalog restore identifies copies by physical path because catalog rows do
-- not expose database copy IDs. Lock the current path owner before transitioning a
-- copy verified missing; historical tombstones can coexist with a reused live path.
-- name: LockMediaAssetCopyByPhysicalPath :one
SELECT copy.id, copy.media_asset_id, copy.generation, copy.state
FROM media_assets asset
JOIN media_asset_copies copy ON copy.media_asset_id = asset.id
WHERE copy.store = sqlc.arg('store')
  AND copy.rel_path = sqlc.arg('rel_path')
  AND copy.state <> 'deleted'
FOR UPDATE OF asset, copy;

-- Repeated rescue of an already missing copy must not add duplicate tombstones.
-- name: CatalogHasDeletedMediaAssetCopy :one
SELECT EXISTS (
    SELECT 1 FROM media_asset_copies copy
    WHERE copy.media_asset_id = sqlc.arg('media_asset_id')
      AND copy.store = sqlc.arg('store')
      AND copy.rel_path = sqlc.arg('rel_path')
      AND copy.generation = sqlc.arg('generation')
      AND copy.state = 'deleted'
);

-- media_assets.state remains the logical publication state mirrored for readers
-- outside the copy-aware paths. Only current-generation copies contribute.
-- active かどうかは active_media_asset_copies の現行判定をそのまま使う。読み手と同じ関数を
-- 引かないと、関数側だけを直したときに encode の committed 判定と不足の列挙が食い違う。
-- name: ReconcileMediaAssetStateFromCopies :exec
WITH observed_state AS (
    SELECT asset.id,
           asset.deleted_at,
           CASE
               WHEN EXISTS (SELECT 1 FROM active_media_asset_copies(asset.id, NULL)) THEN 'active'
               WHEN EXISTS (
                   SELECT 1 FROM media_asset_copies copy
                   WHERE copy.media_asset_id = asset.id
                     AND copy.generation = COALESCE(asset.derived_generation, 0)
                     AND copy.state = 'deleting'
               ) THEN 'deleting'
               ELSE 'deleted'
           END AS state
    FROM media_assets asset
    WHERE asset.id = sqlc.arg('asset_id')
),
desired_state AS (
    SELECT id,
           state,
           CASE WHEN state = 'deleted' THEN COALESCE(deleted_at, now()) END AS deleted_at
    FROM observed_state
)
UPDATE media_assets asset
SET state = desired_state.state,
    deleted_at = desired_state.deleted_at,
    updated_at = CASE
        WHEN asset.state IS DISTINCT FROM desired_state.state
          OR asset.deleted_at IS DISTINCT FROM desired_state.deleted_at
        THEN now()
        ELSE asset.updated_at
    END
FROM desired_state
WHERE asset.id = desired_state.id;

-- 削除遷移の判定は media_asset_copies の state を読む。判定する文より前に対象コピー行を
-- ロックすると、次の文は新しい snapshot で評価され、別の tx が commit した deleted を見る。
-- ロックしない判定は READ COMMITTED の再評価でも古い版を見て、deleted を deleting に戻しうる。
-- 行が無ければ何もロックしない（後続の UPDATE が 0 行になる）。
-- name: LockMediaAssetCopy :one
SELECT copy.media_asset_id, copy.state
FROM media_assets asset
JOIN media_asset_copies copy ON copy.media_asset_id = asset.id
WHERE copy.id = sqlc.arg('copy_id')
  AND copy.store = sqlc.arg('store_name')
FOR UPDATE OF asset, copy;

-- S3 の置き場は、PUT の前に pending のコピー行を commit する。コピー行は論理行を必要とするので、
-- 初回の公開だけは論理行をここで作る。state と deleted_at は書かず、呼び出し側が
-- ReconcileMediaAssetStateFromCopies で決め直す（現行のコピーが無い行は deleted になる）。
-- name: InsertMediaAssetForPendingCopy :one
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, derived_generation)
VALUES (
    sqlc.arg('recording_id'), sqlc.arg('kind'), sqlc.narg('profile'),
    sqlc.arg('rel_path'), sqlc.arg('size_bytes'), sqlc.arg('derived_generation')
)
ON CONFLICT (recording_id, kind, profile) DO NOTHING
RETURNING id;

-- name: GetMediaAssetIDForPendingCopy :one
SELECT id FROM media_assets
WHERE recording_id = sqlc.arg('recording_id')
  AND kind = sqlc.arg('kind')
  AND profile IS NOT DISTINCT FROM sqlc.narg('profile')::text;

-- name: LockMediaAssetForPendingCopy :one
SELECT id FROM media_assets
WHERE id = sqlc.arg('media_asset_id')
FOR UPDATE;

-- 回収済みの世代も数える。回収した key の世代を別の出力へ割り当て直さないためである。
-- name: GetMaxMediaAssetCopyGeneration :one
SELECT COALESCE(MAX(generation), -1)::bigint AS max_generation
FROM media_asset_copies
WHERE media_asset_id = sqlc.arg('media_asset_id');

-- 同じ key の行が既にあれば何もしない。呼び出し側が GetMediaAssetCopyByPath で、同じ論理資産・
-- 同じ世代の pending かを確かめる。
-- name: InsertPendingMediaAssetCopy :execrows
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES (sqlc.arg('media_asset_id'), sqlc.arg('store'), sqlc.arg('rel_path'), sqlc.arg('generation'), 'pending')
ON CONFLICT (store, rel_path) WHERE state <> 'deleted' DO NOTHING;

-- name: GetMediaAssetCopyByPath :one
SELECT id, media_asset_id, generation, state
FROM media_asset_copies
WHERE store = sqlc.arg('store') AND rel_path = sqlc.arg('rel_path') AND state <> 'deleted';

-- pending を active にする。generation は書き換えない。created_at は公開の時刻に更新する。
-- 旧世代を消す overlap の起点は、現行世代の最初のコピー行の created_at なので、
-- 予約した時刻のままだと encode の所要時間の分だけ猶予が短くなる。
-- name: ActivatePendingMediaAssetCopy :execrows
UPDATE media_asset_copies
SET state = 'active', created_at = now(), deleted_at = NULL
WHERE media_asset_id = sqlc.arg('media_asset_id')
  AND store = sqlc.arg('store')
  AND rel_path = sqlc.arg('rel_path')
  AND generation = sqlc.arg('generation')
  AND state = 'pending';

-- 公開されないまま猶予を過ぎた pending。予約の時刻で数える。
-- name: ListAgedPendingMediaAssetCopies :many
SELECT copy.id AS copy_id, asset.id AS media_asset_id, asset.recording_id,
       copy.rel_path, asset.size_bytes, asset.kind, copy.generation
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.store = sqlc.arg('store_name')
  AND copy.state = 'pending'
  AND copy.created_at < sqlc.arg('created_before')::timestamptz
ORDER BY copy.id
LIMIT sqlc.arg('row_limit');

-- 回収する pending 行を、公開の activate と直列化するためにロックする。
-- 公開が先に active にしていれば 0 行になり、回収は何も消さない。
-- name: LockAgedPendingMediaAssetCopy :one
SELECT copy.media_asset_id
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.id = sqlc.arg('copy_id')
  AND copy.store = sqlc.arg('store_name')
  AND copy.state = 'pending'
  AND copy.created_at < sqlc.arg('created_before')::timestamptz
FOR UPDATE OF asset, copy;

-- name: MarkPendingMediaAssetCopyDeleted :execrows
UPDATE media_asset_copies
SET state = 'deleted', deleted_at = COALESCE(deleted_at, now())
WHERE id = sqlc.arg('copy_id')
  AND store = sqlc.arg('store_name')
  AND state = 'pending';

-- name: AdoptUnassignedMediaAssetCopies :exec
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at, deleted_at)
SELECT a.id, sqlc.arg('store'), a.rel_path,
       CASE WHEN a.kind = 'original' THEN 0
            ELSE COALESCE(media_asset_generation_from_rel_path(a.rel_path), 0)
       END,
       a.state, a.created_at, a.deleted_at
FROM media_assets a
WHERE NOT EXISTS (
    SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = a.id
)
ON CONFLICT (store, rel_path) WHERE state <> 'deleted' DO NOTHING;

-- A path's generation belongs to its physical copy. Include deleted copies because their
-- generation remains part of the allocator's history.
-- name: ReconcileDerivedGenerationFromCopyPaths :exec
UPDATE media_assets AS asset
SET derived_generation = GREATEST(
    COALESCE(asset.derived_generation, 0),
    COALESCE((
        SELECT max(copy.generation)
        FROM media_asset_copies copy
        WHERE copy.media_asset_id = asset.id
    ), 0)
)
WHERE asset.kind <> 'original';


-- name: StoreHasMediaAssetCopies :one
SELECT EXISTS (SELECT 1 FROM media_asset_copies WHERE store = sqlc.arg('store'));

-- name: AdoptUnassignedIngestTemps :exec
UPDATE recording_ingest_progress SET temp_store = sqlc.arg('store') WHERE temp_store = '';

-- name: AdoptUnassignedOrphans :exec
UPDATE orphan_files SET store = sqlc.arg('store') WHERE store = '';

-- name: AdoptUnassignedStorageSync :exec
UPDATE storage_sync SET store = sqlc.arg('store') WHERE store = '';

-- A worker must not start while any migrated row still lacks its physical store.
-- name: CountUnassignedStorageRows :one
SELECT
    (SELECT count(*) FROM media_assets a WHERE NOT EXISTS (
        SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = a.id
    ))
  + (SELECT count(*) FROM recording_ingest_progress WHERE temp_store = '')
  + (SELECT count(*) FROM orphan_files WHERE store = '')
  + (SELECT count(*) FROM storage_sync WHERE store = '')
  + (SELECT count(*) FROM circuit_breakers WHERE scope = '' AND name = 'delete_reconcile') AS count;
