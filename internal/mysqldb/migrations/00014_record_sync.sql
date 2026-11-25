-- +goose Up
-- mirakc の record の観測（(site, record_id) ごとに 1 行）。Postgres の record_sync と列を揃える。
-- 通知トリガーは Postgres 側に無いので張らない。
-- tags は text[] の代わりに JSON 配列。failed_reason は JSON。
CREATE TABLE record_sync (
    site VARCHAR(255) NOT NULL,
    record_id VARCHAR(255) NOT NULL,
    recording_id BIGINT,
    program_id BIGINT NOT NULL,
    status VARCHAR(255) NOT NULL,
    content_path LONGTEXT,
    content_length BIGINT,
    tags JSON NOT NULL DEFAULT (JSON_ARRAY()),
    failed_reason JSON,
    observed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (site, record_id),
    KEY record_sync_recording_id (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id),
    CHECK (JSON_TYPE(tags) = 'ARRAY')
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE record_sync;
