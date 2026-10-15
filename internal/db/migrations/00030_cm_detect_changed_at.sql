-- +goose Up

ALTER TABLE recording_encode_policy
    ADD COLUMN cm_detect_changed_at timestamptz;

-- Existing disabled policies may already have an attempt row. Treat the migration
-- as their last state change so a rapid re-enable before the first reconcile clears it.
UPDATE recording_encode_policy
SET cm_detect_changed_at = now()
WHERE NOT cm_detect;

-- +goose Down

ALTER TABLE recording_encode_policy
    DROP COLUMN cm_detect_changed_at;
