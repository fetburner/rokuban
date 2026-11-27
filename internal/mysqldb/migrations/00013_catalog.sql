-- +goose Up
-- catalog（export / restore）が読み書きする永続資産の残りの表。Postgres の最終形（internal/db/schema.sql）に列を揃える。

-- 物理コピー。(store, rel_path) は墓石も含めて一意（Postgres の media_asset_copies_store_rel_path_idx と同じ）。
-- rel_path は長さの上限を超えうる（墓石の合成パスは 1024 字以上）ので、一意キーは SHA-256 の生成列で持つ。
-- 部分一意索引（state <> 'deleted' の行だけ一意）は、生きている間だけ値を持つ生成列 live_generation で代える。
CREATE TABLE media_asset_copies (
    id BIGINT NOT NULL AUTO_INCREMENT,
    media_asset_id BIGINT NOT NULL,
    store VARCHAR(64) NOT NULL CHECK (store REGEXP '^[a-z0-9-]+$'),
    rel_path LONGTEXT NOT NULL,
    rel_path_key BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(rel_path, 256))) STORED,
    state VARCHAR(8) NOT NULL CHECK (state IN ('pending', 'active', 'deleting', 'deleted')),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    deleted_at DATETIME(6),
    generation BIGINT NOT NULL,
    live_generation BIGINT GENERATED ALWAYS AS (CASE WHEN state <> 'deleted' THEN generation END) STORED,
    PRIMARY KEY (id),
    UNIQUE KEY media_asset_copies_store_rel_path (store, rel_path_key),
    UNIQUE KEY media_asset_copies_asset_store_generation (media_asset_id, store, live_generation),
    KEY media_asset_copies_media_asset_id (media_asset_id),
    FOREIGN KEY (media_asset_id) REFERENCES media_assets(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE media_asset_ts_scans (
    media_asset_id BIGINT NOT NULL,
    scanned_size_bytes BIGINT NOT NULL CHECK (scanned_size_bytes >= 0),
    content_revision BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (media_asset_id),
    FOREIGN KEY (media_asset_id) REFERENCES media_assets(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE drop_stats (
    media_asset_id BIGINT NOT NULL,
    pid INT NOT NULL,
    packets BIGINT NOT NULL,
    drops BIGINT NOT NULL,
    errors BIGINT NOT NULL,
    scrambled BIGINT NOT NULL,
    pid_type LONGTEXT,
    PRIMARY KEY (media_asset_id, pid),
    FOREIGN KEY (media_asset_id) REFERENCES media_assets(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE drop_positions (
    media_asset_id BIGINT NOT NULL,
    byte_offset BIGINT NOT NULL,
    pid INT NOT NULL,
    elapsed_ms BIGINT,
    PRIMARY KEY (media_asset_id, byte_offset),
    FOREIGN KEY (media_asset_id) REFERENCES media_assets(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE recording_processing_policy (
    recording_id BIGINT NOT NULL,
    keep_original VARCHAR(13) NOT NULL CHECK (keep_original IN ('always', 'until_encoded')),
    encode_profiles JSON NOT NULL DEFAULT (JSON_ARRAY()),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    cm_detect BOOLEAN NOT NULL DEFAULT FALSE,
    cm_detect_generation BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id),
    CHECK (JSON_TYPE(encode_profiles) = 'ARRAY'),
    CHECK (keep_original <> 'until_encoded' OR JSON_LENGTH(encode_profiles) > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE recording_purge_requests (
    recording_id BIGINT NOT NULL,
    requested_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE recording_watched (
    recording_id BIGINT NOT NULL,
    watched_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE recording_chapter_ownership (
    recording_id BIGINT NOT NULL,
    adopted_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 区間は [start_ms, end_ms)。Postgres の区間の重なりの EXCLUDE は写さず、書き手が重なりを確かめる。
-- 開始位置が同じ区間は、この索引を主キーに上げた後の migration が拒む。
CREATE TABLE recording_chapter_spans (
    recording_id BIGINT NOT NULL,
    start_ms BIGINT NOT NULL,
    end_ms BIGINT NOT NULL,
    label LONGTEXT,
    cut BOOLEAN NOT NULL,
    KEY recording_chapter_spans_recording (recording_id, start_ms),
    FOREIGN KEY (recording_id) REFERENCES recording_chapter_ownership(recording_id) ON DELETE CASCADE,
    CHECK (label IS NOT NULL OR cut),
    CHECK (start_ms < end_ms)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE recording_chapter_spans;
DROP TABLE recording_chapter_ownership;
DROP TABLE recording_watched;
DROP TABLE recording_purge_requests;
DROP TABLE recording_processing_policy;
DROP TABLE drop_positions;
DROP TABLE drop_stats;
DROP TABLE media_asset_ts_scans;
DROP TABLE media_asset_copies;
