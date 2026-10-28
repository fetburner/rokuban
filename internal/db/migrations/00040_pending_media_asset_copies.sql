-- +goose Up

-- Version 39 added this non-unique lookup index. Replace it with a partial unique
-- index so a new pending copy can coexist with the current active copy.
DROP INDEX media_asset_copies_active_asset_store_idx;
ALTER TABLE media_asset_copies
    DROP CONSTRAINT media_asset_copies_asset_store_key;

CREATE UNIQUE INDEX media_asset_copies_active_asset_store_idx
    ON media_asset_copies (media_asset_id, store) WHERE state = 'active';

-- +goose Down

DROP INDEX media_asset_copies_active_asset_store_idx;
ALTER TABLE media_asset_copies
    ADD CONSTRAINT media_asset_copies_asset_store_key UNIQUE (media_asset_id, store);
CREATE INDEX media_asset_copies_active_asset_store_idx
    ON media_asset_copies (media_asset_id, store) WHERE state = 'active';
