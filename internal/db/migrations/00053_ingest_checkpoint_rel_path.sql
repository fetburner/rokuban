-- +goose Up
ALTER TABLE recording_ingest_checkpoints
    ADD COLUMN canonical_rel_path text;

-- +goose Down
ALTER TABLE recording_ingest_checkpoints
    DROP COLUMN canonical_rel_path;
