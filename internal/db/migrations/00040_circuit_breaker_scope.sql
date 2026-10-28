-- +goose Up

ALTER TABLE circuit_breakers RENAME COLUMN site TO scope;

-- +goose Down

ALTER TABLE circuit_breakers RENAME COLUMN scope TO site;
