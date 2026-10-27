-- Physical copies remain separate from media_assets so their location and lifecycle
-- can evolve without moving the logical asset's satellite rows.

-- name: UpsertMediaAssetCopy :exec
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, deleted_at)
VALUES (
    sqlc.arg('media_asset_id'), sqlc.arg('store'), sqlc.arg('rel_path'),
    sqlc.arg('state'), sqlc.narg('deleted_at')
)
ON CONFLICT (media_asset_id, store) DO UPDATE SET
    rel_path   = EXCLUDED.rel_path,
    state      = EXCLUDED.state,
    deleted_at = EXCLUDED.deleted_at;

-- A pre-migration writer may have committed the legacy row without a copy row,
-- or changed its path while an old pod was still running. The configured store is
-- the legacy media_dir during this migration stage, so refresh that copy from the
-- logical row as part of the lifecycle transition.
-- name: SetMediaAssetCopyState :execrows
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at, deleted_at)
SELECT a.id, sqlc.arg('store'), a.rel_path, sqlc.arg('state'), now(),
       CASE WHEN sqlc.arg('state')::text = 'deleted' THEN COALESCE(a.deleted_at, now()) END
FROM media_assets a
WHERE a.id = sqlc.arg('media_asset_id')
ON CONFLICT (media_asset_id, store) DO UPDATE SET
    rel_path = EXCLUDED.rel_path,
    state = EXCLUDED.state,
    deleted_at = EXCLUDED.deleted_at;

-- name: AdoptUnassignedMediaAssetCopies :exec
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at, deleted_at)
SELECT a.id, sqlc.arg('store'), a.rel_path, a.state, a.created_at, a.deleted_at
FROM media_assets a
WHERE EXISTS (
          SELECT 1 FROM media_asset_copies c
          WHERE c.media_asset_id = a.id AND c.store = ''
      )
   OR NOT EXISTS (
          SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = a.id
      )
   OR EXISTS (
          SELECT 1 FROM media_asset_copies c
          WHERE c.media_asset_id = a.id AND c.store = sqlc.arg('store')
      )
ON CONFLICT (media_asset_id, store) DO UPDATE SET
    rel_path = EXCLUDED.rel_path,
    state = EXCLUDED.state,
    deleted_at = EXCLUDED.deleted_at;

-- Old writers may advance a derived path after migration but before they stop.
-- Keep the monotonic allocator ahead of the path that adoption copies forward.
-- name: ReconcileDerivedGenerationFromLegacyPaths :exec
UPDATE media_assets
SET derived_generation = GREATEST(
    COALESCE(derived_generation, 0),
    COALESCE(media_asset_generation_from_rel_path(rel_path), 0)
)
WHERE kind <> 'original';

-- name: DeleteUnassignedMediaAssetCopies :exec
DELETE FROM media_asset_copies WHERE store = '';

-- name: AdoptUnassignedIngestTemps :exec
UPDATE recording_ingest_progress SET temp_store = sqlc.arg('store') WHERE temp_store = '';

-- name: AdoptUnassignedOrphans :exec
UPDATE orphan_files SET store = sqlc.arg('store') WHERE store = '';

-- name: AdoptUnassignedMissingMediaAssets :exec
UPDATE missing_media_assets SET store = sqlc.arg('store') WHERE store = '';

-- name: AdoptUnassignedStorageSync :exec
UPDATE storage_sync SET store = sqlc.arg('store') WHERE store = '';

-- A worker must not start while any migrated row still lacks its physical store.
-- name: CountUnassignedStorageRows :one
SELECT
    (SELECT count(*) FROM media_asset_copies WHERE store = '')
  + (SELECT count(*) FROM media_assets a WHERE NOT EXISTS (
        SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = a.id
    ))
  + (SELECT count(*) FROM recording_ingest_progress WHERE temp_store = '')
  + (SELECT count(*) FROM orphan_files WHERE store = '')
  + (SELECT count(*) FROM missing_media_assets WHERE store = '')
  + (SELECT count(*) FROM storage_sync WHERE store = '') AS count;
