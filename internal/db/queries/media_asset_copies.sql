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

-- name: SetMediaAssetCopyState :execrows
UPDATE media_asset_copies
SET state = sqlc.arg('state'),
    deleted_at = CASE
        WHEN sqlc.arg('state')::text = 'deleted' THEN COALESCE(deleted_at, now())
        ELSE NULL
    END
WHERE media_asset_id = sqlc.arg('media_asset_id') AND store = sqlc.arg('store');

-- name: AdoptUnassignedMediaAssetCopies :exec
UPDATE media_asset_copies SET store = sqlc.arg('store') WHERE store = '';

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
  + (SELECT count(*) FROM recording_ingest_progress WHERE temp_store = '')
  + (SELECT count(*) FROM orphan_files WHERE store = '')
  + (SELECT count(*) FROM missing_media_assets WHERE store = '')
  + (SELECT count(*) FROM storage_sync WHERE store = '') AS count;
