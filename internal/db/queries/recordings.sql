-- name: CreateRecording :one
INSERT INTO recordings (
    reservation_id, rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status, started_at, ended_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17,
    $18, $19, $20
) RETURNING id;

-- name: UpdateRecordingStatus :exec
UPDATE recordings SET
    status     = CASE WHEN status IN ('finished', 'failed') THEN status ELSE sqlc.arg('new_status') END,
    started_at = COALESCE(started_at, sqlc.arg('started_at')),
    ended_at   = CASE WHEN sqlc.narg('ended_at')::timestamptz IS NOT NULL THEN sqlc.narg('ended_at') ELSE ended_at END,
    updated_at = now()
WHERE id = sqlc.arg('id');

-- name: CreateFailedRecording :exec
INSERT INTO recordings (
    reservation_id, rule_id, source, site,
    network_id, service_id, event_id, service_name,
    channel_type, channel, title, description,
    extended, genres, is_free,
    program_start_at, program_duration_ms,
    status, quality_events
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17,
    'failed', $18
)
ON CONFLICT (site, network_id, service_id, event_id) WHERE deleted_at IS NULL
DO UPDATE SET
    quality_events = recordings.quality_events || EXCLUDED.quality_events,
    updated_at = now();

-- name: AppendQualityEvents :exec
UPDATE recordings
SET quality_events = quality_events || sqlc.arg('events')::jsonb,
    updated_at = now()
WHERE id = sqlc.arg('id');
