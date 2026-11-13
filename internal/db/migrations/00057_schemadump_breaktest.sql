-- +goose Up
CREATE TABLE schemadump_breaktest (id bigint PRIMARY KEY);

-- +goose Down
DROP TABLE schemadump_breaktest;
