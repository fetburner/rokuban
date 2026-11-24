-- +goose Up
CREATE TABLE program_overrides (
    site VARCHAR(255) NOT NULL,
    program_id BIGINT NOT NULL,
    overrides JSON NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (site, program_id),
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots(site, program_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE reservations (
    site VARCHAR(255) NOT NULL,
    program_id BIGINT NOT NULL,
    rule_id BIGINT,
    base JSON,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    dedup_match_recording_id BIGINT,
    dedup_similarity FLOAT,
    PRIMARY KEY (site, program_id),
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots(site, program_id) ON DELETE CASCADE,
    FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE SET NULL,
    CHECK ((dedup_match_recording_id IS NULL) = (dedup_similarity IS NULL))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE ruler_input_revisions (
    site VARCHAR(255) NOT NULL,
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    PRIMARY KEY (site)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE media_assets (
    id BIGINT NOT NULL AUTO_INCREMENT,
    recording_id BIGINT NOT NULL,
    kind VARCHAR(10) NOT NULL CHECK (kind IN ('original', 'encoded', 'thumbnail', 'seek_tiles')),
    profile VARCHAR(255) COLLATE utf8mb4_0900_bin,
    size_bytes BIGINT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    ingest_generation BIGINT,
    derived_generation BIGINT NOT NULL DEFAULT 0,
    sha256 VARBINARY(32),
    content_revision BIGINT NOT NULL DEFAULT 0,
    profile_key VARCHAR(255) COLLATE utf8mb4_0900_bin GENERATED ALWAYS AS (COALESCE(profile, '')) STORED,
    PRIMARY KEY (id),
    UNIQUE KEY media_assets_identity (recording_id, kind, profile_key),
    FOREIGN KEY (recording_id) REFERENCES recordings(id),
    CHECK ((kind = 'encoded') = (profile IS NOT NULL)),
    CHECK (derived_generation >= 0 AND (kind <> 'original' OR derived_generation = 0)),
    CHECK (ingest_generation IS NULL OR (kind = 'original' AND ingest_generation > 0)),
    CHECK (sha256 IS NULL OR OCTET_LENGTH(sha256) = 32)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

ALTER TABLE recordings ADD INDEX recordings_status_rule_start (status, rule_id, program_start_at);

CREATE VIEW program_investments AS
SELECT site, program_id FROM program_intents WHERE action = 'record'
UNION
SELECT site, program_id FROM program_overrides;

CREATE TRIGGER reservations_notify_insert AFTER INSERT ON reservations FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('reservations');
CREATE TRIGGER reservations_notify_update AFTER UPDATE ON reservations FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('reservations');
CREATE TRIGGER reservations_notify_delete AFTER DELETE ON reservations FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('reservations');

CREATE TRIGGER program_overrides_notify_insert AFTER INSERT ON program_overrides FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('reservations');
CREATE TRIGGER program_overrides_notify_update AFTER UPDATE ON program_overrides FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('reservations');
CREATE TRIGGER program_overrides_notify_delete AFTER DELETE ON program_overrides FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('reservations');

CREATE TRIGGER media_assets_notify_insert AFTER INSERT ON media_assets FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('recordings');
CREATE TRIGGER media_assets_notify_update AFTER UPDATE ON media_assets FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('recordings');
CREATE TRIGGER media_assets_notify_delete AFTER DELETE ON media_assets FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('recordings');

-- CASCADE で消える子の DELETE トリガーは発火しないため、親の削除で同じ通知を記録する。
CREATE TRIGGER program_snapshots_notify_cascade BEFORE DELETE ON program_snapshots FOR EACH ROW
    INSERT INTO notifications (payload)
    SELECT 'reservations' FROM reservations WHERE site = OLD.site AND program_id = OLD.program_id
    UNION ALL
    SELECT 'reservations' FROM program_intents WHERE site = OLD.site AND program_id = OLD.program_id
    UNION ALL
    SELECT 'reservations' FROM program_overrides WHERE site = OLD.site AND program_id = OLD.program_id;

-- +goose Down
DROP TRIGGER program_snapshots_notify_cascade;
DROP VIEW program_investments;
ALTER TABLE recordings DROP INDEX recordings_status_rule_start;
DROP TABLE media_assets;
DROP TABLE ruler_input_revisions;
DROP TABLE reservations;
DROP TABLE program_overrides;
