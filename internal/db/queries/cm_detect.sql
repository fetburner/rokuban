-- CM detection jobs use the same desired predicate for the ingest hint and periodic pass.
-- A failed attempt becomes desired again after a newer logo for the station was learned.
-- name: ListMissingCMDetections :many
SELECT r.id
FROM recordings r
JOIN recording_encode_policy p ON p.recording_id = r.id AND p.cm_detect
JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original' AND o.state = 'active'
LEFT JOIN recording_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN cm_logos l ON l.network_id = r.network_id AND l.service_id = r.service_id
WHERE r.id > sqlc.arg('after_recording_id')::bigint
  AND r.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
  AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
  AND (
      ca.recording_id IS NULL
      OR ca.state <> 'failed'
      OR (l.learned_at IS NOT NULL AND ca.attempted_at < l.learned_at)
  )
ORDER BY r.id
LIMIT sqlc.arg('row_limit');

-- name: IsCMDetectionDesired :one
SELECT EXISTS (
    SELECT 1
    FROM recordings r
    JOIN recording_encode_policy p ON p.recording_id = r.id AND p.cm_detect
    JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original' AND o.state = 'active'
    LEFT JOIN recording_cm_attempts ca ON ca.recording_id = r.id
    LEFT JOIN cm_logos l ON l.network_id = r.network_id AND l.service_id = r.service_id
    WHERE r.id = sqlc.arg('recording_id')
      AND r.deleted_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
      AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
      AND (
          ca.recording_id IS NULL
          OR ca.state <> 'failed'
          OR (l.learned_at IS NOT NULL AND ca.attempted_at < l.learned_at)
      )
);

-- name: GetCMDetectionWorkItem :one
SELECT r.id, r.network_id, r.service_id, r.program_duration_ms, o.rel_path,
       COALESCE(p.cm_detect, false)::boolean AS cm_detect,
       (r.deleted_at IS NOT NULL)::boolean AS is_trashed,
       EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id) AS original_missing,
       EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id) AS detected,
       ca.state AS attempt_state
FROM recordings r
LEFT JOIN recording_encode_policy p ON p.recording_id = r.id
LEFT JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original' AND o.state = 'active'
LEFT JOIN recording_cm_attempts ca ON ca.recording_id = r.id
WHERE r.id = sqlc.arg('recording_id');

-- name: GetCMRetryOriginal :one
SELECT EXISTS (
    SELECT 1 FROM recordings r
    JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original' AND o.state = 'active'
    WHERE r.id = sqlc.arg('recording_id')
      AND r.deleted_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
);

-- name: DeleteCMDetection :exec
DELETE FROM recording_cm_detections WHERE recording_id = sqlc.arg('recording_id');

-- name: DeleteCMDetectionAttempt :exec
DELETE FROM recording_cm_attempts WHERE recording_id = sqlc.arg('recording_id');

-- name: MarkCMDetectionRunning :exec
INSERT INTO recording_cm_attempts (recording_id, state, error, attempted_at)
VALUES (sqlc.arg('recording_id'), 'running', NULL, now())
ON CONFLICT (recording_id) DO UPDATE
SET state = 'running', error = NULL, attempted_at = now();

-- name: MarkCMDetectionFailure :exec
UPDATE recording_cm_attempts
SET state = sqlc.arg('state'), error = sqlc.arg('error'), attempted_at = now()
WHERE recording_id = sqlc.arg('recording_id');

-- name: SaveCMDetection :exec
INSERT INTO recording_cm_detections (recording_id, cm_ranges)
VALUES (sqlc.arg('recording_id'), sqlc.arg('cm_ranges')::text::int8multirange)
ON CONFLICT (recording_id) DO UPDATE
SET cm_ranges = EXCLUDED.cm_ranges, detected_at = now();

-- name: UpsertCMLogo :exec
INSERT INTO cm_logos (network_id, service_id, lgd, preview_png, learned_from)
VALUES (sqlc.arg('network_id'), sqlc.arg('service_id'), sqlc.arg('lgd'), sqlc.narg('preview_png'), sqlc.arg('learned_from'))
ON CONFLICT (network_id, service_id) DO UPDATE
SET lgd = EXCLUDED.lgd,
    preview_png = EXCLUDED.preview_png,
    learned_at = now(),
    learned_from = EXCLUDED.learned_from;

-- name: ListCMLogoStates :many
SELECT r.network_id, r.service_id,
       ((array_agg(r.service_name ORDER BY r.id DESC))[1])::text AS service_name,
       count(DISTINCT r.id)::bigint AS recording_count,
       l.learned_at,
       l.preview_png,
       count(DISTINCT ca.recording_id) FILTER (
           WHERE ca.state = 'failed' AND (l.learned_at IS NULL OR ca.attempted_at >= l.learned_at)
       )::bigint AS failed_count
FROM recordings r
LEFT JOIN cm_logos l ON l.network_id = r.network_id AND l.service_id = r.service_id
LEFT JOIN recording_cm_attempts ca ON ca.recording_id = r.id
WHERE r.deleted_at IS NULL
GROUP BY r.network_id, r.service_id, l.learned_at, l.preview_png
ORDER BY r.network_id, r.service_id;

-- name: DeleteCMLogo :execrows
DELETE FROM cm_logos
WHERE network_id = sqlc.arg('network_id') AND service_id = sqlc.arg('service_id');

-- name: GetCMDetectionResult :one
SELECT cm_ranges::text AS cm_ranges, detected_at
FROM recording_cm_detections
WHERE recording_id = sqlc.arg('recording_id');

-- name: GetCMLogo :one
SELECT lgd
FROM cm_logos
WHERE network_id = sqlc.arg('network_id') AND service_id = sqlc.arg('service_id');
