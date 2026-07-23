-- name: GetRecordSyncRecordingID :one
SELECT recording_id FROM record_sync
WHERE site = $1 AND record_id = $2;

-- name: UpsertRecordSync :exec
INSERT INTO record_sync (
    site, record_id, recording_id, program_id,
    status, content_path, content_length,
    tags, failed_reason, observed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (site, record_id) DO UPDATE SET
    recording_id = COALESCE(record_sync.recording_id, EXCLUDED.recording_id),
    program_id   = EXCLUDED.program_id,
    status       = EXCLUDED.status,
    content_path = EXCLUDED.content_path,
    content_length = EXCLUDED.content_length,
    tags         = EXCLUDED.tags,
    failed_reason  = EXCLUDED.failed_reason,
    observed_at  = now();
