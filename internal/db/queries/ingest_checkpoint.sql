-- Ingest's durable byte offset and hash state have the lifetime of a writer lease,
-- which is separate from the streamer-facing recording_ingest_progress row.
-- name: GetIngestOriginalRelPath :one
SELECT copy.rel_path
FROM media_assets asset
JOIN media_asset_copies copy ON copy.media_asset_id = asset.id
WHERE asset.recording_id = sqlc.arg('recording_id')
  AND asset.kind = 'original'
  AND copy.store = sqlc.arg('store_name')
ORDER BY (copy.state = 'active')::boolean DESC, copy.created_at DESC, copy.id DESC
LIMIT 1;

-- name: InsertRecordingIngestCheckpointIfAbsent :execrows
INSERT INTO recording_ingest_checkpoints (
    recording_id, lease_owner, lease_expires_at, writer_generation, handed_off, offset_bytes, hash_state
)
VALUES (
    sqlc.arg('recording_id'), sqlc.arg('owner')::uuid,
    now() + make_interval(secs => sqlc.arg('ttl_seconds')::double precision), 1, false, 0, NULL
)
ON CONFLICT (recording_id) DO NOTHING;

-- name: LockRecordingIngestCheckpoint :one
SELECT writer_generation, canonical_rel_path, handed_off, offset_bytes, hash_state,
       (lease_expires_at > now())::boolean AS lease_live
FROM recording_ingest_checkpoints
WHERE recording_id = sqlc.arg('recording_id')
FOR UPDATE;

-- Reserve the original canonical path once. Retries retain it even when a writer
-- generation is reset; publication reads the returned value from this row.
-- name: ReserveRecordingIngestRelPath :one
UPDATE recording_ingest_checkpoints
SET canonical_rel_path = COALESCE(canonical_rel_path, sqlc.arg('rel_path')::text)
WHERE recording_id = sqlc.arg('recording_id')
  AND lease_owner = sqlc.arg('owner')::uuid
  AND lease_expires_at > now()
RETURNING canonical_rel_path;

-- name: LockLeasedRecordingIngestCheckpoint :one
SELECT recording_id
FROM recording_ingest_checkpoints
WHERE recording_id = sqlc.arg('recording_id')
  AND lease_owner = sqlc.arg('owner')::uuid
  AND writer_generation = sqlc.arg('writer_generation')
  AND lease_expires_at > now()
FOR UPDATE;

-- name: ClaimRecordingIngestCheckpoint :execrows
UPDATE recording_ingest_checkpoints
SET lease_owner = sqlc.arg('owner')::uuid,
    lease_expires_at = now() + make_interval(secs => sqlc.arg('ttl_seconds')::double precision),
    writer_generation = sqlc.arg('writer_generation'),
    handed_off = false,
    offset_bytes = sqlc.arg('offset_bytes'),
    hash_state = sqlc.narg('hash_state')::bytea
WHERE recording_id = sqlc.arg('recording_id');

-- name: SaveRecordingIngestCheckpoint :execrows
UPDATE recording_ingest_checkpoints
SET handed_off = true,
    offset_bytes = sqlc.arg('offset_bytes'),
    hash_state = sqlc.arg('hash_state')::bytea,
    lease_expires_at = now()
WHERE recording_id = sqlc.arg('recording_id')
  AND lease_owner = sqlc.arg('owner')::uuid
  AND writer_generation = sqlc.arg('writer_generation')
  AND lease_expires_at > now();

-- The final handoff is saved before the canonical link and keeps the lease live,
-- because the publication tx still needs the lease. A retry restores the hash from here.
-- name: SaveRecordingIngestFinalHandoff :execrows
UPDATE recording_ingest_checkpoints
SET handed_off = true,
    offset_bytes = sqlc.arg('offset_bytes'),
    hash_state = sqlc.arg('hash_state')::bytea
WHERE recording_id = sqlc.arg('recording_id')
  AND lease_owner = sqlc.arg('owner')::uuid
  AND writer_generation = sqlc.arg('writer_generation')
  AND lease_expires_at > now();

-- name: ResetRecordingIngestCheckpoint :one
UPDATE recording_ingest_checkpoints
SET writer_generation = writer_generation + 1,
    handed_off = false,
    offset_bytes = 0,
    hash_state = NULL
WHERE recording_id = sqlc.arg('recording_id')
  AND lease_owner = sqlc.arg('owner')::uuid
  AND writer_generation = sqlc.arg('writer_generation')
  AND lease_expires_at > now()
RETURNING writer_generation;

-- name: ReleaseRecordingIngestCheckpoint :exec
UPDATE recording_ingest_checkpoints
SET lease_expires_at = now()
WHERE recording_id = sqlc.arg('recording_id')
  AND lease_owner = sqlc.arg('owner')::uuid;

-- The checkpoint row stays and its generation moves on, so the next writer never reuses
-- a temp name that an earlier discard may have left behind (the temp file name carries the generation).
-- name: DiscardRecordingIngestCheckpoint :execrows
WITH owned AS (
    SELECT checkpoint.recording_id
    FROM recording_ingest_checkpoints checkpoint
    WHERE checkpoint.recording_id = sqlc.arg('recording_id')
      AND checkpoint.lease_owner = sqlc.arg('owner')::uuid
      AND checkpoint.writer_generation = sqlc.arg('writer_generation')
    FOR UPDATE
), progress AS (
    DELETE FROM recording_ingest_progress p
    USING owned
    WHERE p.recording_id = owned.recording_id
)
UPDATE recording_ingest_checkpoints c
SET writer_generation = c.writer_generation + 1,
    lease_expires_at = now(),
    handed_off = false,
    offset_bytes = 0,
    hash_state = NULL
FROM owned
WHERE c.recording_id = owned.recording_id;

-- name: DeleteRecordingIngestCheckpoint :exec
DELETE FROM recording_ingest_checkpoints
WHERE recording_id = sqlc.arg('recording_id');

-- name: GetIngestWriterStateByMirakcRecord :one
SELECT checkpoint.recording_id, checkpoint.writer_generation,
       COALESCE(progress.temp_rel_path, '') AS temp_rel_path,
       (progress.recording_id IS NOT NULL)::boolean AS has_progress
FROM record_sync sync
JOIN recording_ingest_checkpoints checkpoint ON checkpoint.recording_id = sync.recording_id
LEFT JOIN recording_ingest_progress progress ON progress.recording_id = checkpoint.recording_id
WHERE sync.site = sqlc.arg('site')
  AND sync.record_id = sqlc.arg('record_id');

-- Progress rows point to a writer only after it reaches the previous visible offset.
-- Renewing the DB lease and changing the visible pointer share one statement.
-- name: UpdateRecordingIngestProgress :execrows
WITH lease AS (
    UPDATE recording_ingest_checkpoints
    SET lease_expires_at = now() + make_interval(secs => sqlc.arg('ttl_seconds')::double precision)
    WHERE recording_ingest_checkpoints.recording_id = sqlc.arg('recording_id')
      AND recording_ingest_checkpoints.lease_owner = sqlc.arg('owner')::uuid
      AND recording_ingest_checkpoints.writer_generation = sqlc.arg('writer_generation')
      AND recording_ingest_checkpoints.lease_expires_at > now()
    RETURNING recording_ingest_checkpoints.recording_id
)
INSERT INTO recording_ingest_progress (
    recording_id, written_bytes, expected_bytes, temp_rel_path, temp_store, temp_generation, observed_at
)
SELECT lease.recording_id, sqlc.arg('written_bytes'), sqlc.narg('expected_bytes')::bigint,
       sqlc.arg('temp_rel_path'), sqlc.arg('temp_store'), sqlc.arg('temp_generation'), now()
FROM lease
ON CONFLICT (recording_id) DO UPDATE SET
    written_bytes = CASE
        WHEN recording_ingest_progress.temp_rel_path = EXCLUDED.temp_rel_path
          OR EXCLUDED.written_bytes >= recording_ingest_progress.written_bytes
        THEN EXCLUDED.written_bytes
        ELSE recording_ingest_progress.written_bytes
    END,
    temp_rel_path = CASE
        WHEN recording_ingest_progress.temp_rel_path = EXCLUDED.temp_rel_path
          OR EXCLUDED.written_bytes >= recording_ingest_progress.written_bytes
        THEN EXCLUDED.temp_rel_path
        ELSE recording_ingest_progress.temp_rel_path
    END,
    temp_generation = CASE
        WHEN recording_ingest_progress.temp_rel_path = EXCLUDED.temp_rel_path
          OR EXCLUDED.written_bytes >= recording_ingest_progress.written_bytes
        THEN EXCLUDED.temp_generation
        ELSE recording_ingest_progress.temp_generation
    END,
    expected_bytes = EXCLUDED.expected_bytes,
    observed_at = now();
