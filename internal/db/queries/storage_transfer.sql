-- Transfer reconcile creates jobs from current desired state. Source data is
-- read only from an active, non-missing physical copy.
-- name: ListMediaAssetTransferCandidates :many
WITH desired_assets AS (
    SELECT a.id,
           a.recording_id,
           a.size_bytes,
           a.kind,
           desired_media_store(
               a.kind,
               sqlc.arg('original_rules')::jsonb,
               sqlc.arg('derived_store')::text,
               r.site,
               (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
           ) AS desired_store
    FROM media_assets a
    JOIN recordings r ON r.id = a.recording_id
    WHERE a.state = 'active'
      AND r.deleted_at IS NULL
)
SELECT d.id AS media_asset_id, d.desired_store
FROM desired_assets d
WHERE d.desired_store IS NOT NULL
  AND d.desired_store = ANY(sqlc.arg('fs_stores')::text[])
  AND NOT EXISTS (
      SELECT 1 FROM media_asset_copies target
      WHERE target.media_asset_id = d.id
        AND target.store = d.desired_store
        AND target.state = 'active'
        AND NOT EXISTS (
            SELECT 1 FROM missing_media_assets m
            WHERE m.media_asset_id = d.id AND m.store = d.desired_store
        )
  )
  AND EXISTS (
      SELECT 1 FROM media_asset_copies source
      WHERE source.media_asset_id = d.id
        AND source.store <> d.desired_store
        AND source.state = 'active'
        AND NOT EXISTS (
            SELECT 1 FROM missing_media_assets m
            WHERE m.media_asset_id = d.id AND m.store = source.store
        )
  )
  AND NOT EXISTS (
      SELECT 1 FROM media_asset_transfer_attempts t
      WHERE t.media_asset_id = d.id
        AND t.store = d.desired_store
        AND t.attempt_count >= sqlc.arg('attempt_limit')
  )
  AND NOT (
      d.kind = 'original'
      AND EXISTS (
          SELECT 1 FROM until_encoded_deletable_originals(sqlc.arg('derived_store')) u
          WHERE u.id = d.id
      )
  )
ORDER BY d.id
LIMIT sqlc.arg('row_limit');

-- name: CountMediaAssetTransferCandidates :one
WITH desired_assets AS (
    SELECT a.id,
           a.size_bytes,
           a.kind,
           desired_media_store(
               a.kind,
               sqlc.arg('original_rules')::jsonb,
               sqlc.arg('derived_store')::text,
               r.site,
               (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
           ) AS desired_store
    FROM media_assets a
    JOIN recordings r ON r.id = a.recording_id
    WHERE a.state = 'active'
      AND r.deleted_at IS NULL
), candidates AS (
    SELECT d.id, d.size_bytes
    FROM desired_assets d
    WHERE d.desired_store IS NOT NULL
      AND d.desired_store = ANY(sqlc.arg('fs_stores')::text[])
      AND NOT EXISTS (
          SELECT 1 FROM media_asset_copies target
          WHERE target.media_asset_id = d.id
            AND target.store = d.desired_store
            AND target.state = 'active'
            AND NOT EXISTS (
                SELECT 1 FROM missing_media_assets m
                WHERE m.media_asset_id = d.id AND m.store = d.desired_store
            )
      )
      AND EXISTS (
          SELECT 1 FROM media_asset_copies source
          WHERE source.media_asset_id = d.id
            AND source.store <> d.desired_store
            AND source.state = 'active'
            AND NOT EXISTS (
                SELECT 1 FROM missing_media_assets m
                WHERE m.media_asset_id = d.id AND m.store = source.store
            )
      )
      AND NOT EXISTS (
          SELECT 1 FROM media_asset_transfer_attempts t
          WHERE t.media_asset_id = d.id
            AND t.store = d.desired_store
            AND t.attempt_count >= sqlc.arg('attempt_limit')
      )
      AND NOT (
          d.kind = 'original'
          AND EXISTS (
              SELECT 1 FROM until_encoded_deletable_originals(sqlc.arg('derived_store')) u
              WHERE u.id = d.id
          )
      )
)
SELECT count(*)::bigint AS asset_count, COALESCE(sum(size_bytes), 0)::bigint AS byte_count
FROM candidates;

-- Lock the logical row before reserving the destination copy. This serializes
-- transfer publication with deletion and recording state changes.
-- name: PrepareMediaAssetTransfer :one
WITH asset AS MATERIALIZED (
    SELECT a.id,
           a.recording_id,
           a.rel_path,
           a.size_bytes,
           a.kind,
           a.sha256,
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
      AND a.state = 'active'
      AND r.deleted_at IS NULL
    FOR UPDATE OF a, r
), source AS MATERIALIZED (
    SELECT c.id, c.store, c.rel_path
    FROM asset a
    JOIN media_asset_copies c ON c.media_asset_id = a.id
    WHERE c.store <> sqlc.arg('target_store')
      AND c.store = ANY(sqlc.arg('fs_stores')::text[])
      AND c.state = 'active'
      AND NOT EXISTS (
          SELECT 1 FROM missing_media_assets m
          WHERE m.media_asset_id = a.id AND m.store = c.store
      )
    ORDER BY c.created_at DESC, c.id
    LIMIT 1
), previous AS (
    SELECT c.id, c.state
    FROM media_asset_copies c
    WHERE c.media_asset_id = sqlc.arg('media_asset_id')
      AND c.store = sqlc.arg('target_store')
), upserted AS (
    INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state)
    SELECT a.id, sqlc.arg('target_store'), a.rel_path, 'pending'
    FROM asset a
    CROSS JOIN source s
    WHERE a.desired_store = sqlc.arg('target_store')
      AND NOT (
          a.kind = 'original'
          AND EXISTS (
              SELECT 1 FROM until_encoded_deletable_originals(sqlc.arg('derived_store')) u
              WHERE u.id = a.id
          )
      )
      AND NOT EXISTS (
          SELECT 1 FROM media_asset_transfer_attempts t
          WHERE t.media_asset_id = a.id
            AND t.store = sqlc.arg('target_store')
            AND t.attempt_count >= sqlc.arg('attempt_limit')
      )
    ON CONFLICT (media_asset_id, store) DO UPDATE SET
        rel_path = EXCLUDED.rel_path,
        state = 'pending',
        deleted_at = NULL
    WHERE media_asset_copies.state <> 'active'
       OR EXISTS (
           SELECT 1 FROM missing_media_assets m
           WHERE m.media_asset_id = EXCLUDED.media_asset_id
             AND m.store = EXCLUDED.store
       )
    RETURNING id, media_asset_id
)
SELECT a.id AS media_asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.kind,
       a.sha256,
       a.desired_store,
       s.store AS source_store,
       s.rel_path AS source_rel_path,
       s.id AS source_copy_id,
       u.id AS copy_id,
       COALESCE(p.state, '')::text AS previous_copy_state
FROM asset a
CROSS JOIN source s
JOIN upserted u ON u.media_asset_id = a.id
LEFT JOIN previous p ON p.id = u.id;

-- name: CompleteMediaAssetTransfer :one
WITH observed AS (
    UPDATE media_assets a
    SET sha256 = COALESCE(a.sha256, sqlc.arg('sha256')::bytea)
    FROM recordings r
    WHERE a.id = sqlc.arg('media_asset_id')
      AND a.recording_id = r.id
      AND a.state = 'active'
      AND r.deleted_at IS NULL
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
            AND c.state = 'pending'
      )
      AND NOT (
          a.kind = 'original'
          AND EXISTS (
              SELECT 1 FROM until_encoded_deletable_originals(sqlc.arg('derived_store')) u
              WHERE u.id = a.id
          )
      )
    RETURNING a.id
), activated AS (
    UPDATE media_asset_copies c
    SET state = 'active', created_at = now(), deleted_at = NULL
    FROM observed o
    WHERE c.id = sqlc.arg('copy_id')
      AND c.media_asset_id = o.id
      AND c.store = sqlc.arg('target_store')
      AND c.state = 'pending'
    RETURNING c.media_asset_id, c.store
), cleared_missing AS (
    DELETE FROM missing_media_assets m
    USING activated a
    WHERE m.media_asset_id = a.media_asset_id AND m.store = a.store
), cleared_attempts AS (
    DELETE FROM media_asset_transfer_attempts t
    USING activated a
    WHERE t.media_asset_id = a.media_asset_id AND t.store = a.store
)
SELECT count(*)::bigint AS activated_count FROM activated;

-- name: RecordMediaAssetTransferFailure :exec
INSERT INTO media_asset_transfer_attempts (media_asset_id, store, attempt_count, last_error)
VALUES (sqlc.arg('media_asset_id'), sqlc.arg('store'), 1, left(sqlc.arg('last_error'), 2048))
ON CONFLICT (media_asset_id, store) DO UPDATE SET
    attempt_count = media_asset_transfer_attempts.attempt_count + 1,
    last_error = EXCLUDED.last_error,
    updated_at = now();

-- name: MarkMediaAssetCopyCorrupt :exec
WITH deleted AS (
    UPDATE media_asset_copies c
    SET state = 'deleted', deleted_at = now()
    WHERE c.id = sqlc.arg('copy_id')
      AND c.media_asset_id = sqlc.arg('media_asset_id')
      AND c.store = sqlc.arg('store')
      AND c.state <> 'deleted'
    RETURNING c.media_asset_id, c.store
), cleared AS (
    DELETE FROM media_asset_transfer_attempts t
    USING deleted d
    WHERE t.media_asset_id = d.media_asset_id AND t.store = d.store
)
UPDATE media_assets a
SET state = 'deleted', deleted_at = now(), updated_at = now()
WHERE a.id = (SELECT media_asset_id FROM deleted)
  AND a.kind <> 'original'
  AND a.state = 'active'
  AND NOT EXISTS (
      SELECT 1 FROM media_asset_copies c
      WHERE c.media_asset_id = a.id AND c.state = 'active'
        AND NOT EXISTS (
            SELECT 1 FROM deleted d
            WHERE d.media_asset_id = c.media_asset_id AND d.store = c.store
        )
  )
  AND NOT EXISTS (
      SELECT 1 FROM media_asset_copies c
      WHERE c.media_asset_id = a.id AND c.state = 'pending'
  );

-- name: DiscardPendingMediaAssetCopy :exec
WITH discarded AS (
    UPDATE media_asset_copies c
    SET state = 'deleted', deleted_at = now()
    WHERE c.media_asset_id = sqlc.arg('media_asset_id')
      AND c.store = sqlc.arg('store')
      AND c.state = 'pending'
    RETURNING c.media_asset_id, c.store
), cleared AS (
    DELETE FROM media_asset_transfer_attempts t
    USING discarded d
    WHERE t.media_asset_id = d.media_asset_id AND t.store = d.store
)
UPDATE media_assets a
SET state = 'deleted', deleted_at = now(), updated_at = now()
WHERE a.id = (SELECT media_asset_id FROM discarded)
  AND a.kind <> 'original'
  AND a.state = 'active'
  AND NOT EXISTS (
      SELECT 1 FROM media_asset_copies c
      WHERE c.media_asset_id = a.id AND c.state IN ('active', 'pending')
        AND NOT EXISTS (
            SELECT 1 FROM discarded d
            WHERE d.media_asset_id = c.media_asset_id AND d.store = c.store
        )
  );

-- name: ListMediaAssetCopiesToDelete :many
SELECT c.id AS copy_id, c.media_asset_id, c.store, c.rel_path, a.size_bytes, a.kind
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.state IN ('active', 'deleting')
  AND media_asset_copy_prunable(
      c.media_asset_id,
      c.store,
      sqlc.arg('original_rules')::jsonb,
      sqlc.arg('derived_store'),
      sqlc.arg('overlap_us')::bigint * interval '1 microsecond'
  )
ORDER BY c.media_asset_id, c.store
LIMIT sqlc.arg('row_limit');

-- name: GetMediaAssetCopyForDeletion :one
SELECT c.id AS copy_id, c.media_asset_id, c.store, c.rel_path, a.size_bytes, a.kind
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.id = sqlc.arg('copy_id')
  AND c.media_asset_id = sqlc.arg('media_asset_id')
  AND c.store = sqlc.arg('store');

-- Callers lock this logical row in a separate statement, then invoke the
-- predicate again in this statement so concurrent copy removals serialize.
-- name: LockMediaAssetForStorageTransfer :one
SELECT id FROM media_assets WHERE id = sqlc.arg('media_asset_id') FOR UPDATE;

-- name: MarkMediaAssetCopyDeleting :one
UPDATE media_asset_copies c
SET state = 'deleting'
WHERE c.id = sqlc.arg('copy_id')
  AND c.media_asset_id = sqlc.arg('media_asset_id')
  AND c.store = sqlc.arg('store')
  AND c.state IN ('active', 'deleting')
  AND media_asset_copy_prunable(
      c.media_asset_id,
      c.store,
      sqlc.arg('original_rules')::jsonb,
      sqlc.arg('derived_store'),
      sqlc.arg('overlap_us')::bigint * interval '1 microsecond'
  )
RETURNING c.rel_path;

-- name: MarkMediaAssetCopyDeleted :one
WITH deleted AS (
    UPDATE media_asset_copies c
    SET state = 'deleted', deleted_at = now()
    WHERE c.id = sqlc.arg('copy_id')
      AND c.media_asset_id = sqlc.arg('media_asset_id')
      AND c.store = sqlc.arg('store')
      AND c.state = 'deleting'
    RETURNING c.media_asset_id, c.store
), cleared_missing AS (
    DELETE FROM missing_media_assets m
    USING deleted d
    WHERE m.media_asset_id = d.media_asset_id AND m.store = d.store
)
SELECT count(*)::bigint FROM deleted;
