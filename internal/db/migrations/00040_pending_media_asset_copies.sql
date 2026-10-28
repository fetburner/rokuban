-- +goose Up

-- Version 39 added this non-unique lookup index. Replace it with a partial unique
-- index so a new pending copy can coexist with the current active copy.
DROP INDEX media_asset_copies_active_asset_store_idx;
ALTER TABLE media_asset_copies
    DROP CONSTRAINT media_asset_copies_asset_store_key;

CREATE UNIQUE INDEX media_asset_copies_active_asset_store_idx
    ON media_asset_copies (media_asset_id, store) WHERE state = 'active';

-- +goose Down

-- Replacement history legitimately keeps multiple physical rows for one logical
-- asset and store. Do not discard those rows to recreate the old constraint.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM media_asset_copies
        GROUP BY media_asset_id, store
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot downgrade media asset copies while multiple physical copy rows exist for an asset and store';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX media_asset_copies_active_asset_store_idx;
ALTER TABLE media_asset_copies
    ADD CONSTRAINT media_asset_copies_asset_store_key UNIQUE (media_asset_id, store);
CREATE INDEX media_asset_copies_active_asset_store_idx
    ON media_asset_copies (media_asset_id, store) WHERE state = 'active';
