-- +goose Up

-- A row claims that one pending transfer created this destination key. This
-- lets delete reconcile remove its own partial writes without deleting a
-- pre-existing object that rejected Put with ErrAlreadyExists.
CREATE TABLE media_asset_transfer_objects (
    copy_id    bigint NOT NULL REFERENCES media_asset_copies (id) ON DELETE CASCADE,
    rel_path   text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (copy_id, rel_path)
);

COMMENT ON TABLE media_asset_transfer_objects IS
    'Ownership evidence for destination object keys created by an in-flight media asset transfer.';

-- +goose Down
DROP TABLE media_asset_transfer_objects;
