-- +goose Up

-- Existing progress rows cannot identify a temp file generation or its path.
-- Drop them so the next ingest retries from the temp file it can actually find.
DELETE FROM recording_ingest_progress;

CREATE SEQUENCE ingest_temp_generation_seq AS bigint;

ALTER TABLE recording_ingest_progress
    ADD COLUMN temp_rel_path text NOT NULL,
    ADD COLUMN temp_generation bigint NOT NULL CHECK (temp_generation > 0);

ALTER TABLE media_assets
    ADD COLUMN ingest_generation bigint,
    ADD CONSTRAINT media_assets_ingest_generation_positive_check
        CHECK (ingest_generation IS NULL OR ingest_generation > 0),
    ADD CONSTRAINT media_assets_ingest_generation_kind_check
        CHECK (kind = 'original' OR ingest_generation IS NULL);

COMMENT ON COLUMN recording_ingest_progress.temp_rel_path IS
    'Media-root-relative path of the ingest temp associated with this transfer';
COMMENT ON COLUMN recording_ingest_progress.temp_generation IS
    'Generation of the current ingest temp file';
COMMENT ON COLUMN media_assets.ingest_generation IS
    'Generation of the ingest temp whose bytes were committed as this original';

-- +goose Down

ALTER TABLE media_assets
    DROP CONSTRAINT media_assets_ingest_generation_kind_check,
    DROP CONSTRAINT media_assets_ingest_generation_positive_check,
    DROP COLUMN ingest_generation;

ALTER TABLE recording_ingest_progress
    DROP COLUMN temp_generation,
    DROP COLUMN temp_rel_path;

DROP SEQUENCE ingest_temp_generation_seq;
