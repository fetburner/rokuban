-- +goose Up

DROP INDEX public.recordings_unique_active_event;

CREATE UNIQUE INDEX recordings_unique_active_event
    ON public.recordings USING btree
        (site, network_id, service_id, event_id, program_start_at)
    WHERE deleted_at IS NULL AND superseded_at IS NULL;

-- +goose Down

DROP INDEX public.recordings_unique_active_event;

CREATE UNIQUE INDEX recordings_unique_active_event
    ON public.recordings USING btree
        (site, network_id, service_id, event_id)
    WHERE deleted_at IS NULL AND superseded_at IS NULL;
