-- recording_encode_attempts は encode のドメイン試行状態を持つ。attempt_count は
-- River の attempt ではなく、開始ごとに進む fencing token である。

-- name: CreateRecordingEncodeAttemptRunning :execrows
-- 新しい試行を開始する。競合した場合は、既存行をロックして状態を判定する呼び出し側が続ける。
INSERT INTO recording_encode_attempts (
    recording_id, profile, state, error, attempted_at, attempt_count
) VALUES ($1, $2, 'running', NULL, now(), 1)
ON CONFLICT (recording_id, profile) DO NOTHING;

-- name: GetRecordingEncodeAttemptForUpdate :one
SELECT state, error, attempted_at, attempt_count
FROM recording_encode_attempts
WHERE recording_id = $1 AND profile = $2
FOR UPDATE;

-- name: UpdateRecordingEncodeAttemptRunning :execrows
UPDATE recording_encode_attempts
SET state = 'running', error = NULL, attempted_at = now(),
    attempt_count = sqlc.arg('next_count')
WHERE recording_id = $1 AND profile = $2;

-- name: UpdateRecordingEncodeAttemptFailed :execrows
UPDATE recording_encode_attempts
SET state = 'failed', error = $3, attempted_at = now()
WHERE recording_id = $1 AND profile = $2
  AND state = 'running' AND attempt_count = sqlc.arg('attempt_count');

-- name: RestoreRecordingEncodeAttempt :execrows
UPDATE recording_encode_attempts
SET state = $3, error = sqlc.narg('error')::text,
    attempted_at = sqlc.arg('attempted_at'),
    attempt_count = sqlc.arg('restore_count')
WHERE recording_id = $1 AND profile = $2
  AND state = 'running' AND attempt_count = sqlc.arg('current_count');

-- name: DeleteRecordingEncodeAttemptForAttempt :execrows
DELETE FROM recording_encode_attempts
WHERE recording_id = $1 AND profile = $2
  AND state = 'running' AND attempt_count = $3;

-- name: DeleteFailedRecordingEncodeAttempts :execrows
-- 利用者の再要求で、指定プロファイルの failed 行を消して試行予算を戻す。
-- running の行は生きた試行の fencing token なので消さない。
DELETE FROM recording_encode_attempts
WHERE recording_id = $1 AND profile = ANY(sqlc.arg('profiles')::text[])
  AND state = 'failed';
