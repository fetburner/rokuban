-- +goose Up
-- MySQL は 1 トリガーに 1 イベントしか書けず、AFTER UPDATE OF <列> と WHEN 句も書けないため、
-- 表ごとに INSERT / UPDATE / DELETE の 3 本になる。トピックは Postgres の rokuban_notify と同じ "breakers"。
-- +goose StatementBegin
CREATE TRIGGER circuit_breakers_notify_insert AFTER INSERT ON circuit_breakers FOR EACH ROW
BEGIN
    INSERT INTO notifications (payload) VALUES ('breakers');
END
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER circuit_breakers_notify_update AFTER UPDATE ON circuit_breakers FOR EACH ROW
BEGIN
    INSERT INTO notifications (payload) VALUES ('breakers');
END
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER circuit_breakers_notify_delete AFTER DELETE ON circuit_breakers FOR EACH ROW
BEGIN
    INSERT INTO notifications (payload) VALUES ('breakers');
END
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER circuit_breakers_notify_delete;
DROP TRIGGER circuit_breakers_notify_update;
DROP TRIGGER circuit_breakers_notify_insert;
