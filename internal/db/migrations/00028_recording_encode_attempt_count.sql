-- +goose Up

ALTER TABLE recording_encode_attempts
    ADD COLUMN attempt_count integer NOT NULL DEFAULT 0
        CHECK (attempt_count >= 0);

-- Existing rows contain at least one observed encode attempt, but the old schema
-- did not retain a retry count. Keep those rows useful without inventing history.
UPDATE recording_encode_attempts SET attempt_count = 1;

-- +goose Down

ALTER TABLE recording_encode_attempts DROP COLUMN attempt_count;
