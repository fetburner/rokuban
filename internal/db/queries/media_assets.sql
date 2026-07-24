-- name: CreateMediaAsset :one
INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: InsertDropStat :exec
INSERT INTO drop_stats (media_asset_id, pid, packets, drops, errors, scrambled)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetRecordingByID :one
SELECT id, reservation_id, rule_id, source, site,
       network_id, service_id, event_id, service_name,
       channel_type, channel, title, description,
       extended, genres, is_free,
       program_start_at, program_duration_ms,
       status, started_at, ended_at,
       keep_original, encode_profiles, quality_events,
       deleted_at, created_at, updated_at
FROM recordings WHERE id = $1;
