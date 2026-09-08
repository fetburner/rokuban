-- +goose Up

CREATE TABLE drop_positions (
    media_asset_id bigint NOT NULL REFERENCES media_assets(id),
    byte_offset    bigint NOT NULL,
    pid            integer NOT NULL,
    elapsed_ms     bigint,
    PRIMARY KEY (media_asset_id, byte_offset)
);

-- +goose Down

DROP TABLE drop_positions;
