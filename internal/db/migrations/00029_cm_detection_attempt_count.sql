-- +goose Up

ALTER TABLE recording_cm_attempts
    ADD COLUMN attempt_count integer NOT NULL DEFAULT 1,
    ADD CONSTRAINT recording_cm_attempts_attempt_count_check CHECK (attempt_count >= 1);

-- +goose Down

ALTER TABLE recording_cm_attempts
    DROP CONSTRAINT recording_cm_attempts_attempt_count_check,
    DROP COLUMN attempt_count;
