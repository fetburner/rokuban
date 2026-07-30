-- In-place media registration shared by `rokuban rescue` and M3-10 imports.
-- Bytes are already under storage.media_dir; these queries only publish DB rows.

-- name: GetPublishedInPlaceAssetByRelPath :one
SELECT id, recording_id, kind, profile
FROM media_assets
WHERE rel_path = $1 AND state <> 'deleted';

-- name: UpsertInPlaceRecording :one
INSERT INTO recordings (
    source, site,
    network_id, service_id, event_id,
    service_name, channel_type, channel,
    title, program_start_at, program_duration_ms,
    status, started_at, ended_at
) VALUES (
    sqlc.arg('source'), sqlc.arg('site'),
    sqlc.arg('network_id'), sqlc.arg('service_id'), sqlc.arg('event_id'),
    sqlc.arg('service_name'), sqlc.arg('channel_type'), sqlc.arg('channel'),
    sqlc.arg('title'), sqlc.arg('program_start_at'), sqlc.arg('program_duration_ms'),
    sqlc.arg('status'), sqlc.narg('started_at'), sqlc.narg('ended_at')
)
ON CONFLICT (site, network_id, service_id, event_id) WHERE deleted_at IS NULL
DO UPDATE SET
    source              = EXCLUDED.source,
    service_name        = EXCLUDED.service_name,
    channel_type        = EXCLUDED.channel_type,
    channel             = EXCLUDED.channel,
    title               = EXCLUDED.title,
    program_start_at    = EXCLUDED.program_start_at,
    program_duration_ms = EXCLUDED.program_duration_ms,
    status              = EXCLUDED.status,
    started_at          = COALESCE(recordings.started_at, EXCLUDED.started_at),
    ended_at            = COALESCE(recordings.ended_at, EXCLUDED.ended_at),
    updated_at          = now()
RETURNING id;

-- name: UpsertInPlaceMediaAsset :one
INSERT INTO media_assets (
    recording_id, kind, profile, rel_path, size_bytes, state
) VALUES (
    sqlc.arg('recording_id'), sqlc.arg('kind'), sqlc.narg('profile'),
    sqlc.arg('rel_path'), sqlc.arg('size_bytes'), 'active'
)
ON CONFLICT (recording_id, kind, profile)
DO UPDATE SET
    size_bytes = EXCLUDED.size_bytes,
    state      = 'active',
    deleted_at = NULL,
    updated_at = now()
-- A tuple is immutable once published. A different path here means a synthetic identity
-- collision or conflicting import; fail (RETURNING no row) instead of orphaning the old file.
WHERE media_assets.rel_path = EXCLUDED.rel_path
RETURNING id;
