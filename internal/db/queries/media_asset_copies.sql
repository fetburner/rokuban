-- Physical copies remain separate from media_assets so their location and lifecycle
-- can evolve without moving the logical asset's satellite rows.

-- name: UpsertMediaAssetCopy :exec
WITH updated AS (
    UPDATE media_asset_copies copy
    SET rel_path   = sqlc.arg('rel_path'),
        state      = sqlc.arg('state'),
        created_at = COALESCE(sqlc.narg('created_at')::timestamptz, copy.created_at),
        deleted_at = sqlc.narg('deleted_at')
    WHERE copy.media_asset_id = sqlc.arg('media_asset_id')
      AND copy.store = sqlc.arg('store')
      AND copy.state IN ('active', 'deleting')
      AND sqlc.arg('state')::text <> 'pending'
    RETURNING copy.id
)
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at, deleted_at)
SELECT
    sqlc.arg('media_asset_id'), sqlc.arg('store'), sqlc.arg('rel_path'),
    sqlc.arg('state'), COALESCE(sqlc.narg('created_at')::timestamptz, now()), sqlc.narg('deleted_at')
WHERE NOT EXISTS (SELECT 1 FROM updated)
ON CONFLICT (media_asset_id, store) WHERE state = 'active' DO UPDATE SET
    rel_path   = EXCLUDED.rel_path,
    state      = EXCLUDED.state,
    created_at = COALESCE(sqlc.narg('created_at')::timestamptz, media_asset_copies.created_at),
    deleted_at = EXCLUDED.deleted_at;

-- A pending copy may coexist with the current active copy while a remote upload runs.
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

-- name: InsertPendingMediaAssetCopy :execrows
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state)
VALUES (sqlc.arg('media_asset_id'), sqlc.arg('store'), sqlc.arg('rel_path'), 'pending')
ON CONFLICT (store, rel_path) WHERE state <> 'deleted' DO NOTHING;

-- name: GetMediaAssetCopyByPath :one
SELECT id, media_asset_id, state
FROM media_asset_copies
WHERE store = sqlc.arg('store') AND rel_path = sqlc.arg('rel_path') AND state <> 'deleted';

-- name: GetActiveMediaAssetCopy :one
SELECT id, rel_path
FROM media_asset_copies
WHERE media_asset_id = sqlc.arg('media_asset_id')
  AND store = sqlc.arg('store')
  AND state = 'active';

-- name: SetMediaAssetCopiesState :execrows
UPDATE media_asset_copies
SET state = sqlc.arg('state'),
    deleted_at = CASE WHEN sqlc.arg('state')::text = 'deleted' THEN COALESCE(deleted_at, now()) ELSE NULL END
WHERE media_asset_id = sqlc.arg('media_asset_id')
  AND store = sqlc.arg('store')
  AND state = sqlc.arg('from_state');

-- name: SetMediaAssetCopyStateByID :execrows
UPDATE media_asset_copies
SET state = sqlc.arg('state'),
    deleted_at = CASE WHEN sqlc.arg('state')::text = 'deleted' THEN COALESCE(deleted_at, now()) ELSE NULL END
WHERE id = sqlc.arg('id')
  AND state = sqlc.arg('from_state');

-- name: UpdateMediaAssetCopyPathStateByID :execrows
UPDATE media_asset_copies
SET rel_path = sqlc.arg('rel_path'),
    state = sqlc.arg('state'),
    deleted_at = CASE WHEN sqlc.arg('state')::text = 'deleted' THEN COALESCE(deleted_at, now()) ELSE NULL END
WHERE id = sqlc.arg('id');

-- name: InsertMediaAssetCopy :execrows
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state)
VALUES (sqlc.arg('media_asset_id'), sqlc.arg('store'), sqlc.arg('rel_path'), sqlc.arg('state'))
ON CONFLICT (store, rel_path) WHERE state <> 'deleted' DO NOTHING;

-- Lock the logical asset and copy before a separate state transition and finalizer.
-- The parent lock serializes finalizers for different copies of the same asset.
-- name: LockMediaAssetForCopyFinalizationByID :one
SELECT a.id
FROM media_assets a
JOIN media_asset_copies copy ON copy.media_asset_id = a.id
WHERE copy.id = sqlc.arg('id')
FOR UPDATE OF a, copy;

-- name: MarkPendingMediaAssetCopyDeletedByID :execrows
UPDATE media_asset_copies
SET state = 'deleted', deleted_at = COALESCE(deleted_at, now())
WHERE id = sqlc.arg('id') AND state = 'pending';

-- name: ClaimAgedPendingMediaAssetCopyByID :execrows
UPDATE media_asset_copies
SET state = 'deleting'
WHERE id = sqlc.arg('id')
  AND state = 'pending'
  AND created_at < sqlc.arg('created_before')::timestamptz;

-- name: UpdateActiveMediaAssetCopyPath :execrows
UPDATE media_asset_copies
SET rel_path = sqlc.arg('rel_path')
WHERE media_asset_id = sqlc.arg('media_asset_id')
  AND store = sqlc.arg('store')
  AND state = 'active';

-- name: ListAgedPendingMediaAssetCopies :many
SELECT copy.id, copy.media_asset_id, copy.store, copy.rel_path, copy.created_at, asset.kind
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.store = sqlc.arg('store')
  AND copy.state = 'pending'
  AND copy.created_at < sqlc.arg('created_before')::timestamptz
ORDER BY copy.created_at, copy.id
LIMIT sqlc.arg('row_limit');

-- name: ListDeletingMediaAssetCopies :many
SELECT copy.id, copy.media_asset_id, copy.store, copy.rel_path, a.recording_id, a.kind, a.size_bytes
FROM media_asset_copies copy
JOIN media_assets a ON a.id = copy.media_asset_id
WHERE copy.store = sqlc.arg('store') AND copy.state = 'deleting'
ORDER BY copy.id
LIMIT sqlc.arg('row_limit');

-- name: MarkMediaAssetCopyDeletedByID :execrows
UPDATE media_asset_copies
SET state = 'deleted', deleted_at = COALESCE(deleted_at, now())
WHERE id = sqlc.arg('id') AND state = 'deleting';

-- This runs after the copy update as a separate statement so READ COMMITTED gets a
-- fresh snapshot. The caller locks the parent asset first to serialize finalizers.
-- name: FinalizeMediaAssetAfterCopyDelete :exec
WITH completed AS (
    UPDATE media_assets a
    SET state = 'deleted', deleted_at = COALESCE(a.deleted_at, now()), updated_at = now()
    WHERE a.id = sqlc.arg('media_asset_id')
      AND a.state IN ('active', 'deleting')
      AND NOT EXISTS (
          SELECT 1 FROM media_asset_copies copy
          WHERE copy.media_asset_id = a.id AND copy.state IN ('pending', 'active', 'deleting')
      )
    RETURNING a.id, a.recording_id, a.profile
)
DELETE FROM encoded_asset_removal_requests request
USING completed
WHERE request.recording_id = completed.recording_id
  AND request.profile = completed.profile;

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
WHERE media_asset_id = sqlc.arg('media_asset_id')
  AND store = sqlc.arg('store')
  AND state IN ('active', 'deleting')
  AND rel_path = (
      SELECT asset.rel_path FROM media_assets asset
      WHERE asset.id = media_asset_copies.media_asset_id
  );

-- 削除遷移の判定は media_asset_copies の state を読む。判定する文より前に対象コピー行を
-- ロックすると、次の文は新しい snapshot で評価され、別の tx が commit した deleted を見る。
-- ロックしない判定は READ COMMITTED の再評価でも古い版を見て、deleted を deleting に戻しうる。
-- 行が無ければ何もロックしない（後続の UPDATE が 0 行になる）。
-- name: LockMediaAssetCopy :exec
SELECT 1 FROM media_asset_copies
WHERE media_asset_id = sqlc.arg('media_asset_id')
  AND store = sqlc.arg('store')
FOR UPDATE;

-- name: AdoptUnassignedMediaAssetCopies :exec
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at, deleted_at)
SELECT a.id, sqlc.arg('store'), a.rel_path, a.state, a.created_at, a.deleted_at
FROM media_assets a
WHERE NOT EXISTS (
    SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = a.id
)
ON CONFLICT DO NOTHING;

-- A path's generation belongs to its physical copy. Include deleted copies because their
-- generation remains part of the allocator's history.
-- name: ReconcileDerivedGenerationFromCopyPaths :exec
UPDATE media_assets AS asset
SET derived_generation = GREATEST(
    COALESCE(asset.derived_generation, 0),
    COALESCE((
        SELECT max(media_asset_generation_from_rel_path(copy.rel_path))
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

-- name: AdoptUnassignedMissingMediaAssets :exec
UPDATE missing_media_assets SET store = sqlc.arg('store') WHERE store = '';

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
  + (SELECT count(*) FROM missing_media_assets WHERE store = '')
  + (SELECT count(*) FROM storage_sync WHERE store = '') AS count;
