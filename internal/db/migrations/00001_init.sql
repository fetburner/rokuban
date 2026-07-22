-- +goose Up
CREATE TABLE schema_info (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT INTO schema_info (key, value) VALUES ('version', '1');

-- +goose Down
DROP TABLE schema_info;
