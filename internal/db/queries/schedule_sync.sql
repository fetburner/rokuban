-- name: UpsertScheduleSync :exec
INSERT INTO schedule_sync (
    site, program_id, reservation_id, state,
    options, tags, failed_reason, observed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, now())
ON CONFLICT (site, program_id) DO UPDATE SET
    reservation_id = EXCLUDED.reservation_id,
    state          = EXCLUDED.state,
    options        = EXCLUDED.options,
    tags           = EXCLUDED.tags,
    failed_reason  = EXCLUDED.failed_reason,
    observed_at    = now();

-- name: ListScheduleSyncsBySite :many
SELECT * FROM schedule_sync WHERE site = $1;

-- name: DeleteStaleScheduleSyncs :exec
DELETE FROM schedule_sync
WHERE site = $1 AND observed_at < $2;
