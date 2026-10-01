-- +goose Up

-- Household playback facts are written by the API, not by the recording spine.
CREATE TABLE recording_playback_positions (
    recording_id bigint PRIMARY KEY REFERENCES recordings (id),
    position_ms bigint NOT NULL CHECK (position_ms >= 2000),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX recording_playback_positions_updated_at_idx
    ON recording_playback_positions (updated_at DESC, recording_id DESC);

CREATE TABLE recording_watched (
    recording_id bigint PRIMARY KEY REFERENCES recordings (id),
    watched_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX recordings_broadcast_event_idx
    ON recordings (network_id, service_id, program_start_at);

-- +goose Down

DROP TABLE recording_watched;
DROP TABLE recording_playback_positions;
DROP INDEX recordings_broadcast_event_idx;
