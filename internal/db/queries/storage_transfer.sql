-- Candidate selection and waiting metrics use the same named predicate.
-- name: ListMediaAssetTransferCandidates :many
SELECT a.id AS media_asset_id,
       desired_media_store(
           a.kind, sqlc.arg('original_rules')::jsonb, sqlc.arg('derived_store')::text,
           r.site, (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
       ) AS desired_store
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE storage_transfer_candidate(
    a.id, sqlc.arg('original_rules')::jsonb, sqlc.arg('fs_stores')::text[], sqlc.arg('derived_store')::text
)
  AND storage_transfer_attempt_count(
      a.id,
      desired_media_store(
          a.kind, sqlc.arg('original_rules')::jsonb, sqlc.arg('derived_store')::text,
          r.site, (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
      )
  ) < sqlc.arg('attempt_limit')
ORDER BY a.id
LIMIT sqlc.arg('row_limit');

-- name: CountMediaAssetTransferCandidates :one
SELECT count(*)::bigint AS asset_count,
       COALESCE(sum(a.size_bytes), 0)::bigint AS byte_count
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE storage_transfer_candidate(
    a.id, sqlc.arg('original_rules')::jsonb, sqlc.arg('fs_stores')::text[], sqlc.arg('derived_store')::text
)
  AND storage_transfer_attempt_count(
      a.id,
      desired_media_store(
          a.kind, sqlc.arg('original_rules')::jsonb, sqlc.arg('derived_store')::text,
          r.site, (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
      )
  ) < sqlc.arg('attempt_limit');

-- 背圧の判定に使う。候補の判定は CountMediaAssetTransferCandidates と同じ名前付き述語。
-- name: CountMediaAssetTransferBacklogBySourceStore :many
SELECT source.store::text AS store, count(DISTINCT a.id)::bigint AS asset_count
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
CROSS JOIN LATERAL active_media_asset_copies(a.id, NULL) source
WHERE source.store = ANY(sqlc.arg('source_stores')::text[])
  AND storage_transfer_candidate(
      a.id, sqlc.arg('original_rules')::jsonb, sqlc.arg('fs_stores')::text[], sqlc.arg('derived_store')::text
  )
  AND storage_transfer_attempt_count(
      a.id,
      desired_media_store(
          a.kind, sqlc.arg('original_rules')::jsonb, sqlc.arg('derived_store')::text,
          r.site, (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
      )
  ) < sqlc.arg('attempt_limit')
GROUP BY source.store;

-- name: CountMediaAssetTransferExhausted :one
SELECT count(*)::bigint
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
WHERE storage_transfer_candidate(
    a.id, sqlc.arg('original_rules')::jsonb, sqlc.arg('fs_stores')::text[], sqlc.arg('derived_store')::text
)
  AND storage_transfer_attempt_count(
      a.id,
      desired_media_store(
          a.kind, sqlc.arg('original_rules')::jsonb, sqlc.arg('derived_store')::text,
          r.site, (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
      )
  ) >= sqlc.arg('attempt_limit');

-- Failures belong to a destination copy. A failure row is obsolete exactly when
-- the pending copy it belongs to is, so this references that named predicate
-- instead of repeating its conditions.
-- name: ClearObsoleteMediaAssetTransferAttempts :exec
DELETE FROM media_asset_transfer_attempts t
USING media_asset_copies c
WHERE t.copy_id = c.id
  AND storage_transfer_pending_copy_obsolete(
      c.id,
      sqlc.arg('original_rules')::jsonb,
      sqlc.arg('fs_stores')::text[],
      sqlc.arg('derived_store')::text
  );

-- The caller first locks the logical asset row in a separate statement, so this
-- statement gets a current READ COMMITTED snapshot before deciding to reserve.
-- name: PrepareMediaAssetTransfer :one
WITH asset AS MATERIALIZED (
    SELECT a.id,
           a.recording_id,
           a.size_bytes,
           a.kind,
           a.sha256,
           COALESCE(a.derived_generation, 0)::bigint AS generation,
           desired_media_store(
               a.kind,
               sqlc.arg('original_rules')::jsonb,
               sqlc.arg('derived_store')::text,
               r.site,
               (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
           ) AS desired_store
    FROM media_assets a
    JOIN recordings r ON r.id = a.recording_id
    WHERE a.id = sqlc.arg('media_asset_id')
      AND r.deleted_at IS NULL
), source AS MATERIALIZED (
    SELECT source_copy.id, source_copy.store, source_copy.rel_path
    FROM asset
    JOIN media_asset_copies source_copy ON source_copy.media_asset_id = asset.id
    JOIN media_assets source_asset ON source_asset.id = source_copy.media_asset_id
    WHERE source_copy.store <> sqlc.arg('target_store')
      AND source_copy.store = ANY(sqlc.arg('source_stores')::text[])
      -- 派生物の転送元は、この worker がマウントしている置き場に限る（HTTP で読む経路は原本だけ）。
      AND (asset.kind = 'original' OR source_copy.store = ANY(sqlc.arg('fs_stores')::text[]))
      AND source_copy.state = 'active'
      AND source_copy.generation = COALESCE(source_asset.derived_generation, 0)
      AND NOT EXISTS (
          SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = source_copy.id
      )
    -- マウントした置き場の copy を、internal_url だけで届く copy より先に選ぶ（ローカルから読めるほうを優先）。
    ORDER BY (source_copy.store = ANY(sqlc.arg('mounted_stores')::text[])) DESC,
             source_copy.created_at DESC, source_copy.id
    LIMIT 1
), eligible AS MATERIALIZED (
    SELECT asset.*
    FROM asset
    CROSS JOIN source
    WHERE asset.desired_store = sqlc.arg('target_store')
      AND storage_transfer_candidate(
          sqlc.arg('media_asset_id'), sqlc.arg('original_rules')::jsonb,
          sqlc.arg('source_stores')::text[], sqlc.arg('derived_store')::text
      )
      AND storage_transfer_attempt_count(sqlc.arg('media_asset_id'), sqlc.arg('target_store')) < sqlc.arg('attempt_limit')
), upserted AS (
    INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
    SELECT asset.id, sqlc.arg('target_store'), source.rel_path, asset.generation, 'pending'
    FROM eligible asset
    CROSS JOIN source
    ON CONFLICT (media_asset_id, store, generation) WHERE state <> 'deleted'
    DO UPDATE SET
        rel_path = EXCLUDED.rel_path,
        state = 'pending',
        deleted_at = NULL
    WHERE media_asset_copies.state = 'pending'
       OR (
           media_asset_copies.state = 'active'
           AND EXISTS (
               SELECT 1 FROM missing_media_assets m
               WHERE m.media_asset_copy_id = media_asset_copies.id
           )
       )
    RETURNING id, media_asset_id, store, rel_path
)
SELECT asset.id AS media_asset_id,
       asset.recording_id,
       asset.size_bytes,
       asset.kind,
       asset.sha256,
       asset.generation,
       asset.desired_store,
       source.store AS source_store,
       source.rel_path AS source_rel_path,
       source.id AS source_copy_id,
       upserted.id AS copy_id,
       upserted.rel_path
FROM eligible asset
CROSS JOIN source
JOIN upserted ON upserted.media_asset_id = asset.id;

-- name: CompleteMediaAssetTransfer :one
WITH observed AS (
    UPDATE media_assets a
    SET sha256 = sqlc.arg('sha256')::bytea
    FROM recordings r
    WHERE a.id = sqlc.arg('media_asset_id')
      AND a.recording_id = r.id
      AND r.deleted_at IS NULL
      AND COALESCE(a.derived_generation, 0) = sqlc.arg('generation')
      AND desired_media_store(
          a.kind,
          sqlc.arg('original_rules')::jsonb,
          sqlc.arg('derived_store')::text,
          r.site,
          (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
      ) = sqlc.arg('target_store')
      AND a.size_bytes = sqlc.arg('size_bytes')
      AND (a.sha256 IS NULL OR a.sha256 = sqlc.arg('sha256')::bytea)
      AND EXISTS (
          SELECT 1 FROM media_asset_copies c
          WHERE c.id = sqlc.arg('copy_id')
            AND c.media_asset_id = a.id
            AND c.store = sqlc.arg('target_store')
            AND c.generation = COALESCE(a.derived_generation, 0)
            AND c.state = 'pending'
      )
      AND NOT (
          a.kind = 'original'
          AND EXISTS (
              SELECT 1 FROM until_encoded_deletable_originals(sqlc.arg('derived_store')) u
              WHERE u.asset_id = a.id
          )
      )
    RETURNING a.id
), activated AS (
    UPDATE media_asset_copies c
    SET state = 'active', created_at = now(), deleted_at = NULL
    FROM observed o, media_assets a
    WHERE c.id = sqlc.arg('copy_id')
      AND c.media_asset_id = o.id
      AND a.id = o.id
      AND c.store = sqlc.arg('target_store')
      AND c.generation = COALESCE(a.derived_generation, 0)
      AND c.state = 'pending'
    RETURNING c.id
), cleared_missing AS (
    DELETE FROM missing_media_assets m
    USING activated a
    WHERE m.media_asset_copy_id = a.id
), cleared_attempts AS (
    DELETE FROM media_asset_transfer_attempts t
    USING activated a
    WHERE t.copy_id = a.id
)
SELECT CASE
         WHEN EXISTS (SELECT 1 FROM activated) THEN 'activated'::text
         WHEN EXISTS (
             SELECT 1 FROM media_asset_copies active_copy
             WHERE active_copy.id = sqlc.arg('copy_id')
               AND active_copy.media_asset_id = sqlc.arg('media_asset_id')
               AND active_copy.store = sqlc.arg('target_store')
               AND active_copy.state = 'active'
         ) THEN 'already_active'::text
         WHEN EXISTS (
             SELECT 1 FROM media_assets current_asset
             WHERE current_asset.id = sqlc.arg('media_asset_id')
               AND current_asset.sha256 IS NOT NULL
               AND current_asset.sha256 <> sqlc.arg('sha256')::bytea
         ) THEN 'sha_mismatch'::text
         ELSE 'stale'::text
       END AS outcome;

-- name: RecordMediaAssetTransferFailure :exec
INSERT INTO media_asset_transfer_attempts (copy_id, attempt_count, last_error)
VALUES (sqlc.arg('copy_id'), 1, left(sqlc.arg('last_error'), 2048))
ON CONFLICT (copy_id) DO UPDATE SET
    attempt_count = media_asset_transfer_attempts.attempt_count + 1,
    last_error = EXCLUDED.last_error,
    updated_at = now();

-- Record ownership only while the destination copy is still pending. The caller
-- holds the same row locks as obsolete-copy reclaim so a deleted reservation
-- cannot acquire ownership after it has been retired.
-- name: MarkMediaAssetTransferObjectOwned :execrows
INSERT INTO media_asset_transfer_objects (copy_id, rel_path)
SELECT c.id, sqlc.arg('rel_path')
FROM media_asset_copies c
WHERE c.id = sqlc.arg('copy_id')
  AND c.store = sqlc.arg('store_name')
  AND c.state = 'pending'
ON CONFLICT (copy_id, rel_path) DO NOTHING;

-- name: ListMediaAssetTransferObjectsOwned :many
SELECT o.rel_path
FROM media_asset_transfer_objects o
JOIN media_asset_copies c ON c.id = o.copy_id
WHERE o.copy_id = sqlc.arg('copy_id')
  AND c.store = sqlc.arg('store_name')
  AND c.state = 'pending'
ORDER BY o.rel_path;

-- name: DeleteMediaAssetTransferObjectOwned :exec
DELETE FROM media_asset_transfer_objects o
USING media_asset_copies c
WHERE o.copy_id = c.id
  AND o.copy_id = sqlc.arg('copy_id')
  AND c.store = sqlc.arg('store_name')
  AND o.rel_path = sqlc.arg('rel_path');

-- name: DeleteMediaAssetTransferObjectsForCopy :exec
DELETE FROM media_asset_transfer_objects
WHERE copy_id = sqlc.arg('copy_id');

-- A damaged derived source is retired as a physical copy: active -> deleting
-- here, then the transfer worker unlinks the file and marks it deleted. The
-- logical row stays available for encode reconcile, which sees that its current
-- generation has no active copy and publishes a replacement. The caller locks the
-- recording and then the derived asset row before this statement, the same order
-- as MarkUntilEncodedDeleting, so the original check below cannot race with an
-- until_encoded original deletion.
-- name: MarkMediaAssetCopyCorruptDeleting :execrows
UPDATE media_asset_copies c
SET state = 'deleting', deleted_at = NULL
FROM media_assets a
WHERE c.id = sqlc.arg('copy_id')
  AND c.media_asset_id = sqlc.arg('media_asset_id')
  AND c.store = sqlc.arg('store')
  AND c.media_asset_id = a.id
  AND a.recording_id = sqlc.arg('recording_id')
  AND c.generation = COALESCE(a.derived_generation, 0)
  AND c.state = 'active'
  AND a.kind <> 'original'
  AND EXISTS (
      SELECT 1
      FROM media_assets original
      JOIN media_asset_copies original_copy ON original_copy.media_asset_id = original.id
      WHERE original.recording_id = a.recording_id
        AND original.kind = 'original'
        AND original_copy.state = 'active'
        AND original_copy.generation = COALESCE(original.derived_generation, 0)
        AND NOT EXISTS (
            SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = original_copy.id
        )
  );

-- name: DiscardPendingMediaAssetCopy :exec
WITH discarded AS (
    UPDATE media_asset_copies c
    SET state = 'deleted', deleted_at = now()
    WHERE c.id = sqlc.arg('copy_id')
      AND c.state = 'pending'
    RETURNING c.id
), cleared_missing AS (
    DELETE FROM missing_media_assets m USING discarded d
    WHERE m.media_asset_copy_id = d.id
), cleared_transfer_objects AS (
    DELETE FROM media_asset_transfer_objects o USING discarded d
    WHERE o.copy_id = d.id
)
DELETE FROM media_asset_transfer_attempts t USING discarded d
WHERE t.copy_id = d.id;

-- Only reclaim a pending transfer after the current policy has made the
-- reservation obsolete. Keep failed transfers that are still desired intact.
-- name: ListObsoletePendingMediaAssetTransferCopies :many
SELECT c.id AS copy_id, a.id AS media_asset_id, c.store, c.rel_path,
       c.state, a.size_bytes, a.kind, c.generation
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.store = sqlc.arg('store_name')
  AND c.state = 'pending'
  AND storage_transfer_pending_copy_obsolete(
      c.id,
      sqlc.arg('original_rules')::jsonb,
      sqlc.arg('fs_stores')::text[],
      sqlc.arg('derived_store')::text
  )
ORDER BY c.id
LIMIT sqlc.arg('row_limit');

-- The caller holds the path flock before this row lock, matching transfer's
-- filesystem-lock then transaction order.
-- name: LockPendingMediaAssetTransferCopy :one
SELECT c.id
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
JOIN recordings r ON r.id = a.recording_id
WHERE c.id = sqlc.arg('copy_id')
  AND c.store = sqlc.arg('store_name')
  AND c.state = 'pending'
FOR UPDATE OF r, a, c;

-- Re-evaluate after the row locks have been acquired so a concurrent restore,
-- generation advance, placement change, or retention change cannot be hidden by the candidate read.
-- name: IsPendingMediaAssetTransferCopyObsolete :one
SELECT storage_transfer_pending_copy_obsolete(
    sqlc.arg('copy_id'),
    sqlc.arg('original_rules')::jsonb,
    sqlc.arg('fs_stores')::text[],
    sqlc.arg('derived_store')::text
)::boolean AS obsolete;

-- name: MarkObsoletePendingMediaAssetTransferCopyDeleted :execrows
UPDATE media_asset_copies c
SET state = 'deleted', deleted_at = now()
WHERE c.id = sqlc.arg('copy_id')
  AND c.store = sqlc.arg('store_name')
  AND c.state = 'pending'
  AND storage_transfer_pending_copy_obsolete(
      c.id,
      sqlc.arg('original_rules')::jsonb,
      sqlc.arg('fs_stores')::text[],
      sqlc.arg('derived_store')::text
  );

-- name: DeleteMediaAssetTransferAttemptsForCopy :exec
DELETE FROM media_asset_transfer_attempts
WHERE copy_id = sqlc.arg('copy_id');

-- Placement cleanup runs inside delete_reconcile. Its list and mark operations
-- both reference the named predicate so the row lock closes the policy-switch race.
-- Only active copies are new candidates; a copy already deleting is picked up
-- again by ListMediaAssetsPendingDelete, which references the same predicate.
-- name: ListMediaAssetCopiesToDelete :many
SELECT c.id AS copy_id, c.media_asset_id, c.store, c.rel_path, c.state,
       a.recording_id, a.size_bytes, a.kind, c.generation
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.store = sqlc.arg('store_name')
  AND c.state = 'active'
  AND media_asset_copy_prunable(
      c.id,
      sqlc.arg('original_rules')::jsonb,
      sqlc.arg('derived_store')::text,
      sqlc.arg('overlap_cutoff')::timestamptz
  )
ORDER BY c.id
LIMIT sqlc.arg('row_limit');

-- name: GetMediaAssetCopyForDeletion :one
SELECT c.id AS copy_id, c.media_asset_id, c.store, c.rel_path, c.state,
       a.recording_id, a.size_bytes, a.kind, c.generation
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.id = sqlc.arg('copy_id')
  AND c.media_asset_id = sqlc.arg('media_asset_id')
  AND c.store = sqlc.arg('store');

-- name: LockMediaAssetForStorageTransfer :one
SELECT id FROM media_assets WHERE id = sqlc.arg('media_asset_id') FOR UPDATE;

-- name: MarkMediaAssetCopyDeleting :one
UPDATE media_asset_copies c
SET state = 'deleting', deleted_at = NULL
WHERE c.id = sqlc.arg('copy_id')
  AND c.media_asset_id = sqlc.arg('media_asset_id')
  AND c.store = sqlc.arg('store')
  AND c.state = 'active'
  AND media_asset_copy_prunable(
      c.id,
      sqlc.arg('original_rules')::jsonb,
      sqlc.arg('derived_store')::text,
      sqlc.arg('overlap_cutoff')::timestamptz
  )
RETURNING c.rel_path;

-- Progress rows follow the pending copy: the caller deletes them in the same
-- transaction that makes the copy active, deleted, or reclaimed.
-- name: InsertMediaAssetTransferProgressIfAbsent :execrows
INSERT INTO media_asset_copy_transfer_progress (copy_id, lease_owner, lease_expires_at, generation, handed_off, offset_bytes, hash_state)
SELECT c.id, sqlc.arg('owner')::uuid, now() + make_interval(secs => sqlc.arg('ttl_seconds')::double precision), 1, false, 0, NULL
FROM media_asset_copies c
WHERE c.id = sqlc.arg('copy_id')
  AND c.state = 'pending'
ON CONFLICT (copy_id) DO NOTHING;

-- name: LockMediaAssetTransferProgress :one
SELECT p.generation, p.handed_off, p.offset_bytes, p.hash_state,
       (p.lease_expires_at > now())::boolean AS lease_live
FROM media_asset_copy_transfer_progress p
WHERE p.copy_id = sqlc.arg('copy_id')
FOR UPDATE;

-- name: ClaimMediaAssetTransferProgress :execrows
UPDATE media_asset_copy_transfer_progress p
SET lease_owner = sqlc.arg('owner')::uuid,
    lease_expires_at = now() + make_interval(secs => sqlc.arg('ttl_seconds')::double precision),
    generation = sqlc.arg('generation'),
    handed_off = false,
    offset_bytes = sqlc.arg('offset_bytes'),
    hash_state = sqlc.narg('hash_state')::bytea
WHERE p.copy_id = sqlc.arg('copy_id');

-- The reset is only valid for the lease holder, so a zombie that lost its lease cannot bump the generation.
-- name: ResetMediaAssetTransferProgress :one
UPDATE media_asset_copy_transfer_progress p
SET generation = p.generation + 1,
    offset_bytes = 0,
    hash_state = NULL,
    handed_off = false
WHERE p.copy_id = sqlc.arg('copy_id')
  AND p.lease_owner = sqlc.arg('owner')::uuid
  AND p.lease_expires_at > now()
RETURNING p.generation;

-- name: SaveMediaAssetTransferProgress :execrows
UPDATE media_asset_copy_transfer_progress p
SET handed_off = true,
    offset_bytes = sqlc.arg('offset_bytes'),
    hash_state = sqlc.narg('hash_state')::bytea,
    lease_expires_at = now()
WHERE p.copy_id = sqlc.arg('copy_id')
  AND p.lease_owner = sqlc.arg('owner')::uuid
  AND p.generation = sqlc.arg('generation')
  AND p.lease_expires_at > now();

-- name: ReleaseMediaAssetTransferProgress :exec
UPDATE media_asset_copy_transfer_progress p
SET lease_expires_at = now(),
    handed_off = false
WHERE p.copy_id = sqlc.arg('copy_id')
  AND p.lease_owner = sqlc.arg('owner')::uuid;

-- name: GetMediaAssetTransferProgressGeneration :one
SELECT p.generation
FROM media_asset_copy_transfer_progress p
WHERE p.copy_id = sqlc.arg('copy_id');

-- name: DeleteMediaAssetTransferProgress :exec
DELETE FROM media_asset_copy_transfer_progress
WHERE copy_id = sqlc.arg('copy_id');
