-- +goose Up
-- 録画の検索・一覧・棚が読む表と view。列は Postgres の最終形（internal/db/schema.sql）に揃える。
-- 書き手は ingest・CM 検出・視聴状態・エンコード・チャプターで、録画の一覧・検索はこれらの表から読む。
-- 表の寿命は録画の観測や試行と同じく、書き手ごとに分けてある。

CREATE TABLE recording_playback_positions (
    recording_id BIGINT NOT NULL,
    position_ms BIGINT NOT NULL CHECK (position_ms >= 2000),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE recording_encode_attempts (
    recording_id BIGINT NOT NULL,
    profile VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL,
    state VARCHAR(7) NOT NULL CHECK (state IN ('running', 'failed')),
    error LONGTEXT,
    attempted_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    attempt_count INT NOT NULL DEFAULT 1 CHECK (attempt_count >= 1),
    PRIMARY KEY (recording_id, profile),
    FOREIGN KEY (recording_id) REFERENCES recordings(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE recording_ingest_progress (
    recording_id BIGINT NOT NULL,
    written_bytes BIGINT NOT NULL CHECK (written_bytes >= 0),
    expected_bytes BIGINT CHECK (expected_bytes IS NULL OR expected_bytes >= 0),
    observed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    temp_rel_path LONGTEXT NOT NULL,
    temp_generation BIGINT NOT NULL CHECK (temp_generation > 0),
    temp_store VARCHAR(64) NOT NULL CHECK (temp_store REGEXP '^[a-z0-9-]+$'),
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 区間は {"startMs", "endMs"} の配列（[start, end)）。Postgres の int8multirange の代わり。
-- 並びは書き手が決め、読み手は読み出し後に startMs で並べ直す（JSON_ARRAYAGG は ORDER BY を受けない）。
-- logo_ranges と structure_elements は審査用の材料で、Postgres と同じく両方あるか両方無いかのどちらか。
CREATE TABLE recording_cm_detections (
    recording_id BIGINT NOT NULL,
    cm_ranges JSON NOT NULL,
    detected_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    logo_ranges JSON,
    structure_elements JSON,
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id),
    CHECK (JSON_TYPE(cm_ranges) = 'ARRAY'),
    CHECK ((logo_ranges IS NULL) = (structure_elements IS NULL))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE recording_cm_attempts (
    recording_id BIGINT NOT NULL,
    state VARCHAR(8) NOT NULL CHECK (state IN ('running', 'retrying', 'failed')),
    error LONGTEXT,
    attempted_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    stage VARCHAR(10) CHECK (stage IS NULL OR stage IN ('setup', 'probe', 'area', 'logo', 'chapter', 'join', 'parse', 'save', 'stopped', 'resolution', 'match', 'adopt')),
    attempt_count INT NOT NULL DEFAULT 1 CHECK (attempt_count >= 1),
    policy_generation BIGINT NOT NULL,
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 保持区間の JSON 配列。行が無いことが「カット版ではない」を表す（null の値を作らない）。
CREATE TABLE media_asset_cuts (
    media_asset_id BIGINT NOT NULL,
    keep_ranges JSON NOT NULL,
    PRIMARY KEY (media_asset_id),
    FOREIGN KEY (media_asset_id) REFERENCES media_assets(id),
    CHECK (JSON_TYPE(keep_ranges) = 'ARRAY' AND JSON_LENGTH(keep_ranges) > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE encoded_asset_removal_requests (
    recording_id BIGINT NOT NULL,
    profile VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL,
    requested_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (recording_id, profile),
    FOREIGN KEY (recording_id) REFERENCES recordings(id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE missing_media_assets (
    media_asset_copy_id BIGINT NOT NULL,
    first_seen DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (media_asset_copy_id),
    FOREIGN KEY (media_asset_copy_id) REFERENCES media_asset_copies(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 置き場名を引数に取る関数の代わりに、置き場の列を持つ view を 1 つだけ定義し、読み手が列で絞る。
CREATE VIEW active_media_asset_copies AS
SELECT copy.id, copy.media_asset_id, copy.store
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.state = 'active' AND copy.generation = asset.derived_generation;

CREATE VIEW active_present_media_asset_copies AS
SELECT c.id, c.media_asset_id, c.store
FROM active_media_asset_copies c
WHERE NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = c.id);

CREATE VIEW current_cm_attempts AS
SELECT ca.recording_id, ca.state, ca.stage, ca.error, ca.attempted_at, ca.attempt_count, ca.policy_generation
FROM recording_cm_attempts ca
JOIN recording_processing_policy p ON p.recording_id = ca.recording_id
WHERE p.cm_detect AND ca.policy_generation = p.cm_detect_generation;

CREATE VIEW current_ts_scanned_originals AS
SELECT a.id AS media_asset_id
FROM media_assets a
WHERE a.kind = 'original' AND a.size_bytes = (
    SELECT s.scanned_size_bytes
    FROM media_asset_ts_scans s
    WHERE s.media_asset_id = a.id AND s.content_revision = a.content_revision
);

-- ユーザーが外した encoded 版のうち、他の版に active・実体ありのコピーが残るもの。
CREATE VIEW removed_encoded_assets AS
SELECT a.id AS asset_id, a.recording_id
FROM media_assets a
JOIN encoded_asset_removal_requests q ON q.recording_id = a.recording_id AND q.profile = a.profile
JOIN recording_processing_policy p ON p.recording_id = a.recording_id
WHERE a.kind = 'encoded'
  AND JSON_CONTAINS(p.encode_profiles, JSON_QUOTE(a.profile)) = 0
  AND EXISTS (
      SELECT 1
      FROM media_assets other
      JOIN active_present_media_asset_copies c ON c.media_asset_id = other.id
      WHERE other.recording_id = a.recording_id
        AND (other.kind = 'original'
             OR (other.kind = 'encoded' AND NOT EXISTS (
                 SELECT 1 FROM encoded_asset_removal_requests oq
                 WHERE oq.recording_id = other.recording_id AND oq.profile = other.profile)))
  );

-- 録画の進捗は通知のトリガーを持つ。Postgres の recording_ingest_progress_notify（INSERT・DELETE）と
-- recording_ingest_progress_generation_notify（temp_generation が変わる UPDATE）に揃える。
-- +goose StatementBegin
CREATE TRIGGER recording_ingest_progress_notify_insert AFTER INSERT ON recording_ingest_progress FOR EACH ROW
BEGIN
    INSERT INTO notifications (payload) VALUES ('recordings');
END
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER recording_ingest_progress_notify_update AFTER UPDATE ON recording_ingest_progress FOR EACH ROW
BEGIN
    IF NOT (OLD.temp_generation <=> NEW.temp_generation) THEN
        INSERT INTO notifications (payload) VALUES ('recordings');
    END IF;
END
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER recording_ingest_progress_notify_delete AFTER DELETE ON recording_ingest_progress FOR EACH ROW
BEGIN
    INSERT INTO notifications (payload) VALUES ('recordings');
END
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER recording_ingest_progress_notify_delete;
DROP TRIGGER recording_ingest_progress_notify_update;
DROP TRIGGER recording_ingest_progress_notify_insert;
DROP VIEW removed_encoded_assets;
DROP VIEW current_ts_scanned_originals;
DROP VIEW current_cm_attempts;
DROP VIEW active_present_media_asset_copies;
DROP VIEW active_media_asset_copies;
DROP TABLE missing_media_assets;
DROP TABLE encoded_asset_removal_requests;
DROP TABLE media_asset_cuts;
DROP TABLE recording_cm_attempts;
DROP TABLE recording_cm_detections;
DROP TABLE recording_ingest_progress;
DROP TABLE recording_encode_attempts;
DROP TABLE recording_playback_positions;
