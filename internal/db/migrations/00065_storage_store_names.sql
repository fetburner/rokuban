-- +goose Up

-- Do not assign or discard legacy observations here. Adopt with the preceding
-- release on its current schema before upgrading; CHECK validation must fail
-- and roll back this migration if any unassigned or invalid name remains.
ALTER TABLE orphan_files
    ALTER COLUMN store DROP DEFAULT,
    ADD CONSTRAINT orphan_files_store_check CHECK (store ~ '^[a-z0-9-]+$');
ALTER TABLE storage_sync
    ALTER COLUMN store DROP DEFAULT,
    ADD CONSTRAINT storage_sync_store_check CHECK (store ~ '^[a-z0-9-]+$');
ALTER TABLE recording_ingest_progress
    ALTER COLUMN temp_store DROP DEFAULT,
    ADD CONSTRAINT recording_ingest_progress_temp_store_check CHECK (temp_store ~ '^[a-z0-9-]+$');

COMMENT ON COLUMN orphan_files.store IS 'Store where the orphan path was observed';
COMMENT ON COLUMN storage_sync.store IS 'Store whose filesystem root was observed';

-- +goose Down

ALTER TABLE recording_ingest_progress
    DROP CONSTRAINT recording_ingest_progress_temp_store_check,
    ALTER COLUMN temp_store SET DEFAULT '';
ALTER TABLE storage_sync
    DROP CONSTRAINT storage_sync_store_check,
    ALTER COLUMN store SET DEFAULT '';
ALTER TABLE orphan_files
    DROP CONSTRAINT orphan_files_store_check,
    ALTER COLUMN store SET DEFAULT '';

COMMENT ON COLUMN orphan_files.store IS 'Store where the orphan path was observed; empty until storage adopt';
COMMENT ON COLUMN storage_sync.store IS 'Store whose filesystem root was observed; empty until storage adopt';
