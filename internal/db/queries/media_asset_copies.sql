-- Physical copies remain separate from media_assets so their location and lifecycle
-- can evolve without moving the logical asset's satellite rows.

-- name: UpsertMediaAssetCopy :execrows
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at, deleted_at)
VALUES (
    sqlc.arg('media_asset_id'), sqlc.arg('store'), sqlc.arg('rel_path'), sqlc.arg('generation'),
    sqlc.arg('state'), COALESCE(sqlc.narg('created_at')::timestamptz, now()), sqlc.narg('deleted_at')
)
ON CONFLICT (store, rel_path) WHERE state <> 'deleted' DO UPDATE SET
    state      = EXCLUDED.state,
    created_at = COALESCE(sqlc.narg('created_at')::timestamptz, media_asset_copies.created_at),
    deleted_at = EXCLUDED.deleted_at
WHERE media_asset_copies.media_asset_id = EXCLUDED.media_asset_id
  AND media_asset_copies.generation = EXCLUDED.generation;

-- Keep the rolling-migration mirror's lifecycle state in sync without re-reading
-- the logical media_assets columns. The copy row must exist before a transition
-- can start, as enforced by the worker's adoption check and the transition query.
-- name: SetMediaAssetCopyState :execrows
UPDATE media_asset_copies
SET state = sqlc.arg('state'),
    deleted_at = CASE
        WHEN sqlc.arg('state')::text = 'deleted' THEN COALESCE(deleted_at, now())
        ELSE NULL
    END
WHERE id = sqlc.arg('id')
  AND store = sqlc.arg('store_name')
  AND state = sqlc.arg('expected_state');

-- media_assets.state remains the logical publication state mirrored for readers
-- outside the copy-aware paths. Only current-generation copies contribute.
-- name: ReconcileMediaAssetStateFromCopies :exec
WITH desired_state AS (
    SELECT asset.id,
           CASE
               WHEN EXISTS (
                   SELECT 1 FROM media_asset_copies copy
                   WHERE copy.media_asset_id = asset.id
                     AND copy.generation = COALESCE(asset.derived_generation, 0)
                     AND copy.state = 'active'
               ) THEN 'active'
               WHEN EXISTS (
                   SELECT 1 FROM media_asset_copies copy
                   WHERE copy.media_asset_id = asset.id
                     AND copy.generation = COALESCE(asset.derived_generation, 0)
                     AND copy.state = 'deleting'
               ) THEN 'deleting'
               ELSE 'deleted'
           END AS state,
           CASE
               WHEN EXISTS (
                   SELECT 1 FROM media_asset_copies copy
                   WHERE copy.media_asset_id = asset.id
                     AND copy.generation = COALESCE(asset.derived_generation, 0)
                     AND copy.state IN ('active', 'deleting')
               ) THEN NULL::timestamptz
               ELSE COALESCE(asset.deleted_at, now())
           END AS deleted_at
    FROM media_assets asset
    WHERE asset.id = sqlc.arg('asset_id')
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
