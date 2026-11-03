-- +goose Up

-- Current writers target the composite keys. These legacy single-column indexes
-- incorrectly make roots and relative paths unique across all stores.
DROP INDEX orphan_files_rel_path_legacy_key;
DROP INDEX storage_sync_root_legacy_key;

-- +goose Down

-- Recreating the old global keys fails when stores share a root or relative
-- path. That failure is intentional: the old writers cannot represent those rows.
CREATE UNIQUE INDEX orphan_files_rel_path_legacy_key ON orphan_files (rel_path);
CREATE UNIQUE INDEX storage_sync_root_legacy_key ON storage_sync (root);
