-- +goose Up

-- Tombstones reserve their physical paths just like live copies. A reused path
-- could otherwise let a delayed delete unlink a newer file or let catalog
-- restore resurrect a copy that was already deleted.
DROP INDEX media_asset_copies_store_rel_path_idx;
CREATE UNIQUE INDEX media_asset_copies_store_rel_path_idx
    ON media_asset_copies (store, rel_path);

-- +goose Down

DROP INDEX media_asset_copies_store_rel_path_idx;
CREATE UNIQUE INDEX media_asset_copies_store_rel_path_idx
    ON media_asset_copies (store, rel_path) WHERE state <> 'deleted';
