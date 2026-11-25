-- +goose Up
-- schedule 同期の観測と snapshot marker。Postgres 版（internal/db/schema.sql）の最終形に揃える。
-- tags は JSON 配列で、CHECK が配列であることを検査する（text[] の代わり）。
-- 観測の置き換えは site 単位の全削除と全挿入なので、observed_at は読み出しには使わない。
CREATE TABLE schedule_sync (
    site VARCHAR(255) NOT NULL,
    program_id BIGINT NOT NULL,
    state VARCHAR(64) NOT NULL,
    options JSON NOT NULL,
    tags JSON NOT NULL DEFAULT (JSON_ARRAY()),
    failed_reason JSON,
    observed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (site, program_id),
    CHECK (JSON_TYPE(tags) = 'ARRAY')
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE schedule_sync_snapshots (
    site VARCHAR(255) NOT NULL,
    snapshot_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (site)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE schedule_sync_snapshots;
DROP TABLE schedule_sync;
