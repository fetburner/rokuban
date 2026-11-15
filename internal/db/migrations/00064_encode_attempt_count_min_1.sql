-- +goose Up
UPDATE recording_encode_attempts SET attempt_count = 1 WHERE attempt_count = 0;

ALTER TABLE recording_encode_attempts
    ALTER COLUMN attempt_count SET DEFAULT 1,
    DROP CONSTRAINT recording_encode_attempts_attempt_count_check,
    ADD CONSTRAINT recording_encode_attempts_attempt_count_check CHECK (attempt_count >= 1);

CREATE OR REPLACE FUNCTION encode_attempts_exhausted(rec_id bigint, profile_name text, attempt_limit integer)
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
          AND a.attempt_count >= attempt_limit
    )
$$;

COMMENT ON FUNCTION encode_attempts_exhausted(bigint, text, integer) IS
    'encode_attempts_exhausted は (recording_id, profile) の failed 試行が attempt_limit に達したかを返す。running 行は除外しない。';

-- +goose Down
ALTER TABLE recording_encode_attempts
    ALTER COLUMN attempt_count SET DEFAULT 0,
    DROP CONSTRAINT recording_encode_attempts_attempt_count_check,
    ADD CONSTRAINT recording_encode_attempts_attempt_count_check CHECK (attempt_count >= 0);

CREATE OR REPLACE FUNCTION encode_attempts_exhausted(rec_id bigint, profile_name text, attempt_limit integer)
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
