-- CM detection jobs use the same desired predicate for the ingest hint and periodic pass.
-- The predicate lives in the cm_detection_desired view so a new caller cannot drift from
-- the reconcile definition.
-- name: ListMissingCMDetections :many
SELECT desired.recording_id,
       COALESCE(GREATEST((EXTRACT(EPOCH FROM (r.ended_at - r.started_at)) * 1000)::bigint, 0), 0)::bigint AS recording_duration_ms
FROM cm_detection_desired desired
JOIN recordings r ON r.id = desired.recording_id
WHERE desired.recording_id > sqlc.arg('after_recording_id')::bigint
ORDER BY desired.recording_id
LIMIT sqlc.arg('row_limit');

-- name: GetCMRecordingDuration :one
SELECT COALESCE(GREATEST((EXTRACT(EPOCH FROM (ended_at - started_at)) * 1000)::bigint, 0), 0)::bigint AS recording_duration_ms
FROM recordings
WHERE id = sqlc.arg('recording_id');

-- name: IsCMDetectionDesired :one
SELECT EXISTS (
    SELECT 1 FROM cm_detection_desired desired
    WHERE desired.recording_id = sqlc.arg('recording_id')
);

-- name: GetCMDetectionWorkItem :one
SELECT r.id, r.network_id, r.service_id, o.rel_path,
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

-- name: BeginCMDetectionAttempt :one
INSERT INTO recording_cm_attempts (recording_id, state, error, attempted_at, attempt_count)
VALUES (sqlc.arg('recording_id'), 'running', NULL, now(), 1)
ON CONFLICT (recording_id) DO UPDATE
SET state = CASE
        -- Work calls this only after cm_detection_desired says a failed attempt
        -- has a newer logo or area to analyze.
        WHEN recording_cm_attempts.state = 'failed' THEN 'running'
        -- Only a still-running attempt at the limit is a dead process. A canceled
        -- attempt is rolled back to retrying and must remain eligible for another try.
        WHEN recording_cm_attempts.state = 'running'
             AND recording_cm_attempts.attempt_count >= sqlc.arg('max_attempts')::integer THEN 'failed'
        ELSE 'running'
    END,
    attempt_count = CASE
        WHEN recording_cm_attempts.state = 'failed' THEN recording_cm_attempts.attempt_count + 1
        WHEN recording_cm_attempts.state = 'running'
             AND recording_cm_attempts.attempt_count >= sqlc.arg('max_attempts')::integer THEN recording_cm_attempts.attempt_count
        ELSE recording_cm_attempts.attempt_count + 1
    END,
    stage = CASE
        WHEN recording_cm_attempts.state = 'failed' THEN NULL
        WHEN recording_cm_attempts.state = 'running'
             AND recording_cm_attempts.attempt_count >= sqlc.arg('max_attempts')::integer THEN 'stopped'
        ELSE NULL
    END,
    error = CASE
        WHEN recording_cm_attempts.state = 'failed' THEN NULL
        WHEN recording_cm_attempts.state = 'running'
             AND recording_cm_attempts.attempt_count >= sqlc.arg('max_attempts')::integer THEN 'CM detection process stopped while job was running'
        ELSE NULL
    END,
    attempted_at = CASE
        WHEN recording_cm_attempts.state = 'failed' THEN now()
        WHEN recording_cm_attempts.state = 'running'
             AND recording_cm_attempts.attempt_count >= sqlc.arg('max_attempts')::integer THEN recording_cm_attempts.attempted_at
        ELSE now()
    END
RETURNING attempt_count, state, (state = 'running')::boolean AS should_run;

-- name: MarkCMDetectionFailure :execrows
-- **attempted_at は書き換えない**（ジョブ開始時刻のまま）。再投入の判定は
-- 学習済みロゴの `learned_at` と比べるので、失敗終了時刻で上書きしない。
UPDATE recording_cm_attempts
SET state = sqlc.arg('state'), stage = sqlc.narg('stage'), error = sqlc.arg('error')
WHERE recording_id = sqlc.arg('recording_id')
  AND attempt_count = sqlc.arg('attempt_count')
  AND state = 'running';

-- name: DeleteCMDetectionRunningAttempt :execrows
DELETE FROM recording_cm_attempts
WHERE recording_id = sqlc.arg('recording_id')
  AND attempt_count = sqlc.arg('attempt_count')
  AND state = 'running';

-- name: CancelCMDetectionRunningAttempt :execrows
-- A canceled retry does not consume an attempt. Keep the prior failure budget,
-- while restoring the attempt number so the next actual try uses the same count.
UPDATE recording_cm_attempts
SET state = 'retrying', attempt_count = sqlc.arg('attempt_count')::integer - 1,
    stage = NULL, error = NULL
WHERE recording_id = sqlc.arg('recording_id')
  AND attempt_count = sqlc.arg('attempt_count')
  AND attempt_count > 1
  AND state = 'running';

-- name: LockCMDetectionAttempt :one
SELECT attempt_count
FROM recording_cm_attempts
WHERE recording_id = sqlc.arg('recording_id')
  AND state = 'running'
FOR UPDATE;

-- name: SaveCMDetection :exec
INSERT INTO recording_cm_detections (recording_id, cm_ranges)
VALUES (sqlc.arg('recording_id'), sqlc.arg('cm_ranges')::text::int8multirange)
ON CONFLICT (recording_id) DO UPDATE
SET cm_ranges = EXCLUDED.cm_ranges, detected_at = now();

-- name: LockCMStation :exec
-- 局ごとのロゴ状態（cm_logos と cm_logo_areas）を書く tx の先頭で取る。
-- 枠の保存（PUT）と学習結果の保存が同じ局で並んだときに直列化する。
SELECT pg_advisory_xact_lock(sqlc.arg('network_id')::int, sqlc.arg('service_id')::int);

-- name: InsertLearnedCMLogo :execrows
-- ジョブが学習したロゴを、**ロゴ不在かつ枠がジョブの読んだ時点のまま**のときだけ書く
-- （同じ文で再評価する）。枠が変わっていれば、そのロゴは古い枠で学習されたもので、
-- 書くと枠の保存が消したはずのロゴが復活する。observed_area_updated_at が NULL は
-- 「ジョブが読んだとき枠は無かった」で、いま枠があれば不一致になる。
INSERT INTO cm_logos (network_id, service_id, lgd, preview_png, learned_from, coded_width, coded_height)
SELECT sqlc.arg('network_id')::int, sqlc.arg('service_id')::int, sqlc.arg('lgd')::bytea,
       sqlc.narg('preview_png')::bytea, sqlc.arg('learned_from')::bigint,
       sqlc.arg('coded_width')::int, sqlc.arg('coded_height')::int
WHERE NOT EXISTS (
    SELECT 1 FROM cm_logos l
    WHERE l.network_id = sqlc.arg('network_id')::int AND l.service_id = sqlc.arg('service_id')::int
)
AND (
    SELECT a.updated_at FROM cm_logo_areas a
    WHERE a.network_id = sqlc.arg('network_id')::int AND a.service_id = sqlc.arg('service_id')::int
) IS NOT DISTINCT FROM sqlc.narg('observed_area_updated_at')::timestamptz;

-- name: UpsertCMLogo :exec
INSERT INTO cm_logos (network_id, service_id, lgd, preview_png, learned_from, coded_width, coded_height)
VALUES (sqlc.arg('network_id'), sqlc.arg('service_id'), sqlc.arg('lgd'), sqlc.narg('preview_png'), sqlc.arg('learned_from'), sqlc.arg('coded_width'), sqlc.arg('coded_height'))
ON CONFLICT (network_id, service_id) DO UPDATE
SET lgd = EXCLUDED.lgd,
    preview_png = EXCLUDED.preview_png,
    learned_at = now(),
    learned_from = EXCLUDED.learned_from,
    coded_width = EXCLUDED.coded_width,
    coded_height = EXCLUDED.coded_height;

-- name: ListCMLogoStates :many
SELECT r.network_id, r.service_id,
       ((array_agg(r.service_name ORDER BY r.id DESC))[1])::text AS service_name,
       ((array_agg(r.site ORDER BY r.id DESC))[1])::text AS site,
       count(DISTINCT r.id)::bigint AS recording_count,
       l.learned_at,
       l.preview_png,
       count(DISTINCT ca.recording_id) FILTER (
           WHERE ca.state = 'failed'
             AND ca.stage IS DISTINCT FROM 'adopt'
             AND desired.recording_id IS NULL
       )::bigint AS failed_count,
       count(DISTINCT desired.recording_id)::bigint AS pending_count,
       COALESCE(((array_agg(ca.stage ORDER BY ca.attempted_at DESC) FILTER (
           WHERE ca.state = 'failed'
             AND ca.stage IS DISTINCT FROM 'adopt'
             AND desired.recording_id IS NULL
       ))[1])::text, '')::text AS last_failure_stage,
       count(DISTINCT d.recording_id)::bigint AS detected_count,
       count(DISTINCT d.recording_id) FILTER (
           WHERE EXISTS (
               SELECT 1
               FROM media_assets o2
               WHERE o2.recording_id = r.id
                 AND o2.kind = 'original'
                 AND o2.state = 'active'
                 AND NOT EXISTS (
                     SELECT 1 FROM missing_media_assets m2 WHERE m2.media_asset_id = o2.id
                 )
           )
       )::bigint AS redetectable_count,
       l.coded_width AS learned_coded_width,
       l.coded_height AS learned_coded_height,
       a.x, a.y, a.w, a.h, a.coded_width, a.coded_height, a.updated_at AS area_updated_at,
       c.state AS candidate_state,
       c.stage AS candidate_stage,
       c.error AS candidate_error,
       c.preview_png AS candidate_preview_png,
       c.x AS candidate_x,
       c.y AS candidate_y,
       c.w AS candidate_w,
       c.h AS candidate_h,
       c.coded_width AS candidate_coded_width,
       c.coded_height AS candidate_coded_height,
       c.recording_id AS candidate_recording_id,
       c.attempted_at AS candidate_attempted_at,
       -- コマとタイルを取り寄せる録画（原本があり、実体の無いマーカーが付いていない
       -- 最新のもの）。**0 = 無し**（recordings.id は 1 から始まる）で、
       -- 「原本のある録画がありません」を表す。
       COALESCE((SELECT o2.recording_id
          FROM media_assets o2
          JOIN recordings r2 ON r2.id = o2.recording_id
         WHERE r2.network_id = r.network_id
           AND r2.service_id = r.service_id
           AND o2.kind = 'original'
           AND o2.state = 'active'
           AND r2.deleted_at IS NULL
           AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o2.id)
         ORDER BY r2.id DESC
         LIMIT 1), 0)::bigint AS frame_recording_id
FROM recordings r
LEFT JOIN cm_logos l ON l.network_id = r.network_id AND l.service_id = r.service_id
LEFT JOIN cm_logo_areas a ON a.network_id = r.network_id AND a.service_id = r.service_id
LEFT JOIN cm_logo_candidates c ON c.network_id = r.network_id AND c.service_id = r.service_id
LEFT JOIN recording_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN recording_cm_detections d ON d.recording_id = r.id
LEFT JOIN cm_detection_desired desired ON desired.recording_id = r.id
WHERE r.deleted_at IS NULL
-- a の列は主キー (network_id, service_id) の関数従属なので、この 2 列だけで足りる。
GROUP BY r.network_id, r.service_id, l.learned_at, l.preview_png,
         l.coded_width, l.coded_height, a.network_id, a.service_id,
         c.network_id, c.service_id
ORDER BY r.network_id, r.service_id;

-- name: GetCMLogoArea :one
SELECT x, y, w, h, coded_width, coded_height, updated_at
FROM cm_logo_areas
WHERE network_id = sqlc.arg('network_id') AND service_id = sqlc.arg('service_id');

-- name: UpsertCMLogoArea :exec
INSERT INTO cm_logo_areas (network_id, service_id, x, y, w, h, coded_width, coded_height)
VALUES (
    sqlc.arg('network_id'),
    sqlc.arg('service_id'),
    sqlc.arg('x'),
    sqlc.arg('y'),
    sqlc.arg('w'),
    sqlc.arg('h'),
    sqlc.arg('coded_width'),
    sqlc.arg('coded_height')
)
ON CONFLICT (network_id, service_id) DO UPDATE
SET x = EXCLUDED.x,
    y = EXCLUDED.y,
    w = EXCLUDED.w,
    h = EXCLUDED.h,
    coded_width = EXCLUDED.coded_width,
    coded_height = EXCLUDED.coded_height,
    updated_at = now();

-- name: DeleteCMLogoArea :execrows
DELETE FROM cm_logo_areas
WHERE network_id = sqlc.arg('network_id') AND service_id = sqlc.arg('service_id');

-- name: DeleteCMLogo :execrows
DELETE FROM cm_logos
WHERE network_id = sqlc.arg('network_id') AND service_id = sqlc.arg('service_id');

-- name: GetCMDetectionResult :one
SELECT cm_ranges::text AS cm_ranges, detected_at
FROM recording_cm_detections
WHERE recording_id = sqlc.arg('recording_id');

-- name: GetCMLogo :one
SELECT lgd, coded_width, coded_height, learned_at
FROM cm_logos
WHERE network_id = sqlc.arg('network_id') AND service_id = sqlc.arg('service_id');

-- name: ListMissingCMLogoCandidates :many
SELECT desired.network_id, desired.service_id, COALESCE(desired.recording_id, 0)::bigint AS recording_id, desired.area_updated_at,
       COALESCE(GREATEST((EXTRACT(EPOCH FROM (r.ended_at - r.started_at)) * 1000)::bigint, 0), 0)::bigint AS recording_duration_ms
FROM cm_logo_candidate_desired desired
JOIN recordings r ON r.id = desired.recording_id
WHERE (desired.network_id, desired.service_id) > (
    sqlc.arg('after_network_id')::int,
    sqlc.arg('after_service_id')::int
)
ORDER BY desired.network_id, desired.service_id
LIMIT sqlc.arg('row_limit');

-- name: IsCMLogoCandidateDesired :one
SELECT EXISTS (
    SELECT 1
    FROM cm_logo_candidate_desired
    WHERE network_id = sqlc.arg('network_id')
      AND service_id = sqlc.arg('service_id')
      AND area_updated_at = sqlc.arg('area_updated_at')::timestamptz
);

-- name: InsertCMLogoCandidateRunning :one
INSERT INTO cm_logo_candidates (
    network_id, service_id, state, stage, error,
    x, y, w, h, coded_width, coded_height,
    recording_id, observed_area_updated_at, attempted_at
)
SELECT a.network_id, a.service_id, 'running', NULL, NULL,
       a.x, a.y, a.w, a.h, a.coded_width, a.coded_height,
       sqlc.arg('recording_id')::bigint, a.updated_at, clock_timestamp()
FROM cm_logo_areas a
LEFT JOIN cm_logos l
  ON l.network_id = a.network_id AND l.service_id = a.service_id
WHERE a.network_id = sqlc.arg('network_id')
  AND a.service_id = sqlc.arg('service_id')
  AND a.updated_at = sqlc.arg('area_updated_at')::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM cm_logo_candidates c
      WHERE c.network_id = a.network_id AND c.service_id = a.service_id
  )
  AND (l.network_id IS NULL OR l.learned_at < a.updated_at)
ON CONFLICT (network_id, service_id) DO NOTHING
RETURNING attempted_at;

-- name: ListRunningCMLogoCandidates :many
SELECT network_id, service_id, COALESCE(recording_id, 0)::bigint AS recording_id,
       observed_area_updated_at, attempted_at
FROM cm_logo_candidates
WHERE state = 'running'
ORDER BY network_id, service_id;

-- name: FailOrphanCMLogoCandidate :execrows
UPDATE cm_logo_candidates
SET state = 'failed', stage = 'stopped',
    error = 'CM logo candidate job ended without recording a result'
WHERE network_id = sqlc.arg('network_id')
  AND service_id = sqlc.arg('service_id')
  AND recording_id = sqlc.arg('recording_id')
  AND state = 'running'
  AND observed_area_updated_at = sqlc.arg('area_updated_at')::timestamptz
  AND attempted_at = sqlc.arg('attempted_at')::timestamptz;

-- name: DeleteCMLogoCandidateRunningAttempt :execrows
DELETE FROM cm_logo_candidates
WHERE network_id = sqlc.arg('network_id')
  AND service_id = sqlc.arg('service_id')
  AND state = 'running'
  AND observed_area_updated_at = sqlc.arg('area_updated_at')::timestamptz
  AND attempted_at = sqlc.arg('attempted_at')::timestamptz;

-- name: MarkCMLogoCandidateFailure :execrows
UPDATE cm_logo_candidates
SET state = 'failed', stage = sqlc.narg('stage'), error = sqlc.arg('error')
WHERE network_id = sqlc.arg('network_id')
  AND service_id = sqlc.arg('service_id')
  AND state = 'running'
  AND observed_area_updated_at = sqlc.arg('area_updated_at')::timestamptz
  AND attempted_at = sqlc.arg('attempted_at')::timestamptz;

-- name: MarkCMLogoCandidateReady :execrows
UPDATE cm_logo_candidates c
SET state = 'ready', stage = NULL, error = NULL,
    lgd = sqlc.arg('lgd')::bytea,
    preview_png = sqlc.narg('preview_png')::bytea
WHERE c.network_id = sqlc.arg('network_id')
  AND c.service_id = sqlc.arg('service_id')
  AND c.state = 'running'
  AND c.observed_area_updated_at = sqlc.arg('area_updated_at')::timestamptz
  AND c.attempted_at = sqlc.arg('attempted_at')::timestamptz
  AND EXISTS (
      SELECT 1 FROM cm_logo_areas a
      WHERE a.network_id = c.network_id
        AND a.service_id = c.service_id
        AND a.updated_at = sqlc.arg('area_updated_at')::timestamptz
  );

-- name: DeleteCMLogoCandidate :execrows
DELETE FROM cm_logo_candidates
WHERE network_id = sqlc.arg('network_id') AND service_id = sqlc.arg('service_id');

-- name: DeleteCMLogoCandidateForAreaVersion :execrows
DELETE FROM cm_logo_candidates
WHERE network_id = sqlc.arg('network_id')
  AND service_id = sqlc.arg('service_id')
  AND state = 'running'
  AND observed_area_updated_at = sqlc.arg('area_updated_at')::timestamptz
  AND attempted_at = sqlc.arg('attempted_at')::timestamptz;

-- name: GetCMLogoCandidate :one
SELECT state, stage, error, x, y, w, h, coded_width, coded_height,
       recording_id, observed_area_updated_at, lgd, preview_png, attempted_at
FROM cm_logo_candidates
WHERE network_id = sqlc.arg('network_id') AND service_id = sqlc.arg('service_id');

-- name: DeleteCMAdoptAttemptsForStation :exec
DELETE FROM recording_cm_attempts ca
USING recordings r
WHERE ca.recording_id = r.id
  AND r.network_id = sqlc.arg('network_id')
  AND r.service_id = sqlc.arg('service_id')
  AND ca.stage = 'adopt';

-- name: DeleteCMDetectionsForStationWithActiveOriginal :exec
DELETE FROM recording_cm_detections d
USING recordings r
JOIN media_assets o ON o.recording_id = r.id
    AND o.kind = 'original' AND o.state = 'active'
WHERE d.recording_id = r.id
  AND r.network_id = sqlc.arg('network_id')
  AND r.service_id = sqlc.arg('service_id')
  AND r.deleted_at IS NULL
  AND r.purged_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id
  );

-- name: GetCMLogoAnalysisRecording :one
SELECT r.id, a.updated_at
FROM cm_logo_areas a
JOIN recordings r
  ON r.network_id = a.network_id AND r.service_id = a.service_id
JOIN media_assets o ON o.recording_id = r.id
    AND o.kind = 'original' AND o.state = 'active'
WHERE a.network_id = sqlc.arg('network_id')
  AND a.service_id = sqlc.arg('service_id')
  AND r.deleted_at IS NULL
  AND r.purged_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id
  )
ORDER BY r.id DESC
LIMIT 1;

-- name: HasCMRecordingOriginal :one
SELECT EXISTS (
    SELECT 1
    FROM recordings r
    JOIN media_assets o ON o.recording_id = r.id
        AND o.kind = 'original' AND o.state = 'active'
    WHERE r.id = sqlc.arg('recording_id')
      AND r.network_id = sqlc.arg('network_id')
      AND r.service_id = sqlc.arg('service_id')
      AND r.deleted_at IS NULL
      AND r.purged_at IS NULL
      AND NOT EXISTS (
          SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id
      )
);
