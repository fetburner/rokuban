-- +goose Up

-- ListMissingEncodeProfiles と ListEncodeQueue が共有する、failed 行の試行上限述語。
-- failed 以外（特に live な running 行）は上限に達した扱いにしない。
CREATE FUNCTION encode_attempts_exhausted(rec_id bigint, profile_name text, attempt_limit integer)
RETURNS boolean
LANGUAGE sql
STABLE
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM recording_encode_attempts a
        WHERE a.recording_id = rec_id
          AND a.profile = profile_name
          AND a.state = 'failed'
          AND GREATEST(a.attempt_count, 1) >= attempt_limit
    )
$$;

COMMENT ON FUNCTION encode_attempts_exhausted(bigint, text, integer) IS
    'encode_attempts_exhausted は (recording_id, profile) の failed 試行が attempt_limit に達したかを返す。recording_encode_attempts.attempt_count が 0 の旧行は 1 回と数え、running 行は除外しない。';

-- +goose Down

DROP FUNCTION encode_attempts_exhausted(bigint, text, integer);
