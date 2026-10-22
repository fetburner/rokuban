-- +goose Up

CREATE TABLE ruler_input_revisions (
    site text PRIMARY KEY,
    revision bigint NOT NULL CHECK (revision >= 0)
);

-- +goose Down

DROP TABLE ruler_input_revisions;
