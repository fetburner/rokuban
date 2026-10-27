-- +goose Up

ALTER TABLE recording_ingest_progress
    ADD COLUMN temp_finalized boolean NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN recording_ingest_progress.temp_finalized IS
    'True after the ingest writer has stopped appending to this temp generation';

-- +goose Down

ALTER TABLE recording_ingest_progress DROP COLUMN temp_finalized;
