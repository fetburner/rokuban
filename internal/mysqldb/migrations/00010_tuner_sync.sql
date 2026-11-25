-- +goose Up
-- チューナー射影。使い捨ての射影で、真実は mirakc 側にある。
-- types は JSON 配列で、CHECK が 4 種別の部分集合であることを検査する。
-- SET にしないのは、配列の順序と重複を正規化してしまい、PostgreSQL の text[] と読み出し結果が変わるためである。
CREATE TABLE tuner_sync (
    site VARCHAR(255) NOT NULL,
    tuner_index INT NOT NULL,
    name LONGTEXT NOT NULL,
    types JSON NOT NULL,
    is_available BOOLEAN NOT NULL,
    is_fault BOOLEAN NOT NULL,
    observed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (site, tuner_index),
    CHECK (JSON_TYPE(types) = 'ARRAY'),
    CHECK (JSON_CONTAINS('["GR","BS","CS","SKY"]', types))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE tuner_sync;
