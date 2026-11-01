-- +goose Up
CREATE TABLE recording_ingest_checkpoints (
    recording_id bigint PRIMARY KEY REFERENCES recordings (id) ON DELETE CASCADE,
    lease_owner uuid NOT NULL,
    lease_expires_at timestamptz NOT NULL,
    writer_generation bigint NOT NULL CHECK (writer_generation > 0),
    handed_off boolean NOT NULL DEFAULT false,
    offset_bytes bigint NOT NULL DEFAULT 0 CHECK (offset_bytes >= 0),
    hash_state bytea,
    CHECK (NOT handed_off OR hash_state IS NOT NULL)
);

-- +goose Down
DROP TABLE recording_ingest_checkpoints;
