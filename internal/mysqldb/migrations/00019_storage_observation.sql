-- +goose Up
-- ストレージ観測（置き場の容量）と孤児候補の表。Postgres の storage_sync・orphan_files の列に揃える。

-- 置き場ごと・root ごとの最新観測値（使い捨ての射影）。
CREATE TABLE storage_sync (
    store VARCHAR(64) NOT NULL CHECK (store REGEXP '^[a-z0-9-]+$'),
    root VARCHAR(16) NOT NULL CHECK (root IN ('media', 'scratch')),
    path LONGTEXT NOT NULL,
    total_bytes BIGINT NOT NULL,
    used_bytes BIGINT NOT NULL,
    available_bytes BIGINT NOT NULL,
    observed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (store, root)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 置き場にあるがコピーの無いパスの候補。first_seen が孤児の回収の起点になる。
-- rel_path は長さの上限を超えうるので、主キーは SHA-256 の生成列で持つ（media_asset_copies と同じ）。
CREATE TABLE orphan_files (
    store VARCHAR(64) NOT NULL CHECK (store REGEXP '^[a-z0-9-]+$'),
    rel_path LONGTEXT NOT NULL,
    rel_path_key BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(rel_path, 256))) STORED,
    first_seen DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (store, rel_path_key)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE orphan_files;
DROP TABLE storage_sync;
