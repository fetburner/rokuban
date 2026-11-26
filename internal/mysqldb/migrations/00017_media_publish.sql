-- +goose Up
-- メディアの公開・配信・reconcile・転送の表と view。列と CHECK は Postgres の最終形（internal/db/schema.sql）に揃える。
-- 時刻は DATETIME(6)、バイト列は LONGBLOB、UUID は BINARY(16)、区間は JSON 配列（{"startMs","endMs"}）。

-- 録画の writer lease と clean handoff の checkpoint。行の存在が「writer が居た」ことを表す。
CREATE TABLE recording_ingest_checkpoints (
    recording_id BIGINT NOT NULL,
    lease_owner BINARY(16) NOT NULL,
    lease_expires_at DATETIME(6) NOT NULL,
    writer_generation BIGINT NOT NULL CHECK (writer_generation > 0),
    handed_off BOOLEAN NOT NULL DEFAULT FALSE,
    offset_bytes BIGINT NOT NULL DEFAULT 0 CHECK (offset_bytes >= 0),
    hash_state LONGBLOB,
    canonical_rel_path LONGTEXT,
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id),
    CHECK ((NOT handed_off) OR (hash_state IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- temp ファイルの世代の採番（Postgres の sequence の代わり）。1 行のカウンタで、採番は
-- UPDATE ... SET value = LAST_INSERT_ID(value + 1) の後に同じ接続で LAST_INSERT_ID() を読む。
CREATE TABLE ingest_temp_generation_counter (
    id TINYINT NOT NULL CHECK (id = 1),
    value BIGINT NOT NULL DEFAULT 0 CHECK (value >= 0),
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;
INSERT INTO ingest_temp_generation_counter (id, value) VALUES (1, 0);

-- 利用者が押した時点の keep 区間。満たされても残る desired で、消すのは api だけ。
CREATE TABLE encoded_asset_rebuild_requests (
    recording_id BIGINT NOT NULL,
    profile VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL,
    requested_keep JSON NOT NULL,
    requested_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (recording_id, profile),
    FOREIGN KEY (recording_id) REFERENCES recordings(id),
    CHECK (JSON_TYPE(requested_keep) = 'ARRAY' AND JSON_LENGTH(requested_keep) > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 置き場への転送の進捗（lease・再開位置・アップロード中の状態）。
CREATE TABLE media_asset_copy_transfer_progress (
    copy_id BIGINT NOT NULL,
    lease_owner BINARY(16) NOT NULL,
    lease_expires_at DATETIME(6) NOT NULL,
    writer_generation BIGINT NOT NULL CHECK (writer_generation > 0),
    handed_off BOOLEAN NOT NULL DEFAULT FALSE,
    offset_bytes BIGINT NOT NULL DEFAULT 0 CHECK (offset_bytes >= 0),
    hash_state LONGBLOB,
    upload_store VARCHAR(64) CHECK (upload_store IS NULL OR upload_store REGEXP '^[a-z0-9-]+$'),
    upload_state LONGBLOB,
    verifying_source BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (copy_id),
    FOREIGN KEY (copy_id) REFERENCES media_asset_copies(id),
    CHECK ((upload_store IS NULL) = (upload_state IS NULL)),
    CHECK (NOT (verifying_source AND upload_state IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE media_asset_thumbnail_seeks (
    media_asset_id BIGINT NOT NULL,
    seek_ms BIGINT NOT NULL CHECK (seek_ms >= 0),
    PRIMARY KEY (media_asset_id),
    FOREIGN KEY (media_asset_id) REFERENCES media_assets(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- コピーへの転送の失敗の観測。成功した転送は行を消す。
CREATE TABLE media_asset_transfer_attempts (
    copy_id BIGINT NOT NULL,
    attempt_count INT NOT NULL CHECK (attempt_count > 0),
    last_error LONGTEXT NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (copy_id),
    FOREIGN KEY (copy_id) REFERENCES media_asset_copies(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 転送中に作った置き場のキーの所有の証拠。回収は所有の記録があるキーだけを消す。
CREATE TABLE media_asset_transfer_objects (
    copy_id BIGINT NOT NULL,
    rel_path LONGTEXT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    KEY media_asset_transfer_objects_copy_id (copy_id),
    FOREIGN KEY (copy_id) REFERENCES media_asset_copies(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 以下の view は Postgres の同名の view・関数の定義に従う。MySQL には表を返す関数が無いので、
-- 複数の読み手が使う述語は view に 1 つだけ置く。知らない profile の判定など、呼び出しごとに
-- 変わる引数は読み手のクエリ側に書く。

-- 録画の原本が active（ごみ箱の録画を除く）な録画。
CREATE VIEW encodable_recordings AS
SELECT r.id AS recording_id
FROM recordings r
WHERE r.deleted_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM media_assets o
      JOIN media_asset_copies c ON c.media_asset_id = o.id
      WHERE o.recording_id = r.id AND o.kind = 'original' AND c.state = 'active'
  );

-- 設定の desired profile のうち、まだ active な encoded が無いもの。ordinality は設定の並び。
CREATE VIEW unencoded_desired_profiles AS
SELECT p.recording_id, want.profile, want.ord AS ordinality
FROM recording_processing_policy p
JOIN JSON_TABLE(p.encode_profiles, '$[*]' COLUMNS (
    ord FOR ORDINALITY,
    profile VARCHAR(255) COLLATE utf8mb4_0900_bin PATH '$'
)) want
WHERE NOT EXISTS (
    SELECT 1
    FROM media_assets e
    JOIN active_media_asset_copies ac ON ac.media_asset_id = e.id
    WHERE e.recording_id = p.recording_id AND e.kind = 'encoded' AND e.profile = want.profile
);

-- requested_keep と一致する凍結区間の active な encoded がまだ無い作り直し要求。
CREATE VIEW unsatisfied_encode_rebuilds AS
SELECT q.recording_id, q.profile, q.requested_keep
FROM encoded_asset_rebuild_requests q
JOIN encodable_recordings er ON er.recording_id = q.recording_id
WHERE NOT EXISTS (
    SELECT 1
    FROM media_assets e
    JOIN media_asset_cuts c ON c.media_asset_id = e.id
    JOIN active_media_asset_copies ac ON ac.media_asset_id = e.id
    WHERE e.recording_id = q.recording_id AND e.kind = 'encoded' AND e.profile = q.profile
      AND c.keep_ranges = q.requested_keep
);

-- 確認待ちの候補（cut の profile の絞り込みは読み手が書く）。Postgres の awaiting_review_profiles に対応。
CREATE VIEW awaiting_review_candidates AS
SELECT u.recording_id, u.profile
FROM unencoded_desired_profiles u
JOIN encodable_recordings er ON er.recording_id = u.recording_id
WHERE NOT EXISTS (SELECT 1 FROM recording_chapter_ownership o WHERE o.recording_id = u.recording_id)
  AND NOT EXISTS (SELECT 1 FROM recording_encode_attempts a WHERE a.recording_id = u.recording_id AND a.profile = u.profile)
  AND NOT EXISTS (SELECT 1 FROM unsatisfied_encode_rebuilds q WHERE q.recording_id = u.recording_id AND q.profile = u.profile);

-- +goose Down
DROP VIEW awaiting_review_candidates;
DROP VIEW unsatisfied_encode_rebuilds;
DROP VIEW unencoded_desired_profiles;
DROP VIEW encodable_recordings;
DROP TABLE media_asset_transfer_objects;
DROP TABLE media_asset_transfer_attempts;
DROP TABLE media_asset_thumbnail_seeks;
DROP TABLE media_asset_copy_transfer_progress;
DROP TABLE encoded_asset_rebuild_requests;
DROP TABLE ingest_temp_generation_counter;
DROP TABLE recording_ingest_checkpoints;
