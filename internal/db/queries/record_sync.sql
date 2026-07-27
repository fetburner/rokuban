-- name: GetRecordSyncRecordingID :one
SELECT recording_id FROM record_sync
WHERE site = $1 AND record_id = $2;

-- name: AcquireRecordSync :one
-- (site, record_id) の record_sync 行を確保し、processRecord を直列化するための
-- 行ロックを取る。行がなければ recording_id = NULL で新規作成し、あれば既存の
-- recording_id を返すだけで内容には触れない（PK の ON CONFLICT DO UPDATE が
-- 行ロックを取るために DO UPDATE を使うが、更新対象は自分自身への no-op）。
-- 呼び出し側は返ってきた recording_id が NULL のときだけ recordings 行を作る。
-- 2 つの processRecord が同じ record を同時に処理しても、後発はここで先発の
-- コミットまで待たされ、待った後は recording_id が埋まった状態を見る。
INSERT INTO record_sync (
    site, record_id, recording_id, program_id, status, tags, observed_at
) VALUES ($1, $2, NULL, $3, $4, '{}', now())
ON CONFLICT (site, record_id) DO UPDATE SET
    observed_at = record_sync.observed_at
RETURNING recording_id;

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
