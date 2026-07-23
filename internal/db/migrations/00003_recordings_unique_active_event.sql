-- +goose Up
CREATE UNIQUE INDEX recordings_unique_active_event
    ON recordings (site, network_id, service_id, event_id)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS recordings_unique_active_event;
