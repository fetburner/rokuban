-- +goose Up
-- EPG の一覧・詳細・次回一覧が参照する表。
CREATE TABLE program_snapshots (
    site VARCHAR(255) NOT NULL,
    program_id BIGINT NOT NULL,
    title LONGTEXT NOT NULL DEFAULT (''),
    start_at DATETIME(6) NOT NULL,
    duration_ms BIGINT NOT NULL,
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    channel_type VARCHAR(3) NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    channel LONGTEXT NOT NULL,
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    event_id INT NOT NULL,
    service_name LONGTEXT NOT NULL,
    PRIMARY KEY (site, program_id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE program_intents (
    site VARCHAR(255) NOT NULL,
    program_id BIGINT NOT NULL,
    action VARCHAR(6) NOT NULL CHECK (action IN ('record', 'skip')),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (site, program_id),
    FOREIGN KEY (site, program_id) REFERENCES program_snapshots(site, program_id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE recordings (
    id BIGINT NOT NULL AUTO_INCREMENT,
    rule_id BIGINT,
    source VARCHAR(12) NOT NULL CHECK (source IN ('rule', 'manual', 'unattributed')),
    site VARCHAR(255) NOT NULL,
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    event_id INT NOT NULL,
    service_name LONGTEXT NOT NULL,
    channel_type VARCHAR(3) NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    channel LONGTEXT NOT NULL,
    title LONGTEXT NOT NULL DEFAULT (''),
    description LONGTEXT,
    extended JSON,
    genres JSON,
    is_free BOOLEAN NOT NULL DEFAULT TRUE,
    program_start_at DATETIME(6) NOT NULL,
    program_duration_ms BIGINT NOT NULL,
    status VARCHAR(9) NOT NULL CHECK (status IN ('recording', 'finished', 'canceled', 'failed')),
    started_at DATETIME(6),
    ended_at DATETIME(6),
    quality_events JSON NOT NULL DEFAULT (JSON_ARRAY()),
    deleted_at DATETIME(6),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    superseded_at DATETIME(6),
    purged_at DATETIME(6),
    genre_lv1 JSON NOT NULL DEFAULT (JSON_ARRAY()),
    series_key LONGTEXT,
    failure_reported BOOLEAN NOT NULL DEFAULT FALSE,
    active_program_start_at DATETIME(6) GENERATED ALWAYS AS (
        CASE WHEN deleted_at IS NULL AND superseded_at IS NULL THEN program_start_at END
    ) STORED,
    PRIMARY KEY (id),
    UNIQUE KEY recordings_unique_active_event (site, network_id, service_id, event_id, active_program_start_at),
    FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE SET NULL,
    CHECK (purged_at IS NULL OR deleted_at IS NOT NULL)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE label_rules (
    id BIGINT NOT NULL AUTO_INCREMENT,
    `key` VARCHAR(6) NOT NULL CHECK (`key` = 'series'),
    value LONGTEXT NOT NULL,
    keyword LONGTEXT NOT NULL CHECK (TRIM(keyword) <> ''),
    priority INT NOT NULL DEFAULT 0,
    value_key LONGTEXT NOT NULL,
    keyword_key LONGTEXT GENERATED ALWAYS AS (LOWER(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(keyword,
        'Ａ', 'A'), 'Ｂ', 'B'), 'Ｃ', 'C'), 'Ｄ', 'D'), 'Ｅ', 'E'), 'Ｆ', 'F'), 'Ｇ', 'G'),
        'Ｈ', 'H'), 'Ｉ', 'I'), 'Ｊ', 'J'), 'Ｋ', 'K'), 'Ｌ', 'L'), 'Ｍ', 'M'), 'Ｎ', 'N'),
        'Ｏ', 'O'), 'Ｐ', 'P'), 'Ｑ', 'Q'), 'Ｒ', 'R'), 'Ｓ', 'S'), 'Ｔ', 'T'), 'Ｕ', 'U'),
        'Ｖ', 'V'), 'Ｗ', 'W'), 'Ｘ', 'X'), 'Ｙ', 'Y'), 'Ｚ', 'Z'), 'ａ', 'a'), 'ｂ', 'b'),
        'ｃ', 'c'), 'ｄ', 'd'), 'ｅ', 'e'), 'ｆ', 'f'), 'ｇ', 'g'), 'ｈ', 'h'), 'ｉ', 'i'),
        'ｊ', 'j'), 'ｋ', 'k'), 'ｌ', 'l'), 'ｍ', 'm'), 'ｎ', 'n'), 'ｏ', 'o'), 'ｐ', 'p'),
        'ｑ', 'q'), 'ｒ', 'r'), 'ｓ', 's'), 'ｔ', 't'), 'ｕ', 'u'), 'ｖ', 'v'), 'ｗ', 'w'),
        'ｘ', 'x'), 'ｙ', 'y'), 'ｚ', 'z'), '０', '0'), '１', '1'), '２', '2'), '３', '3'),
        '４', '4'), '５', '5'), '６', '6'), '７', '7'), '８', '8'), '９', '9'), '　', ' '), '!', '!!'), '%', '!%'), '_', '!_'))) VIRTUAL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE label_rule_hits (
    recording_id BIGINT NOT NULL,
    label_rule_id BIGINT NOT NULL,
    PRIMARY KEY (recording_id),
    FOREIGN KEY (recording_id) REFERENCES recordings(id),
    FOREIGN KEY (label_rule_id) REFERENCES label_rules(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TRIGGER program_intents_notify_insert AFTER INSERT ON program_intents FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('reservations');
CREATE TRIGGER program_intents_notify_update AFTER UPDATE ON program_intents FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('reservations');
CREATE TRIGGER program_intents_notify_delete AFTER DELETE ON program_intents FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('reservations');

CREATE TRIGGER recordings_notify_insert AFTER INSERT ON recordings FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('recordings');
CREATE TRIGGER recordings_notify_update AFTER UPDATE ON recordings FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('recordings');
CREATE TRIGGER recordings_notify_delete AFTER DELETE ON recordings FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('recordings');

CREATE TRIGGER label_rules_notify_insert AFTER INSERT ON label_rules FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('recordings');
CREATE TRIGGER label_rules_notify_update AFTER UPDATE ON label_rules FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('recordings');
CREATE TRIGGER label_rules_notify_delete AFTER DELETE ON label_rules FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('recordings');


CREATE VIEW recording_series AS
SELECT r.id AS recording_id, COALESCE(lr.value_key, r.series_key) COLLATE utf8mb4_0900_bin AS value
FROM recordings r
LEFT JOIN label_rule_hits h ON h.recording_id = r.id
LEFT JOIN label_rules lr ON lr.id = h.label_rule_id;

CREATE VIEW epg_program_series AS
SELECT p.site, p.program_id,
       COALESCE((
           SELECT lr.value_key
           FROM label_rules lr
           WHERE p.search_name COLLATE utf8mb4_0900_bin LIKE CONCAT('%', lr.keyword_key, '%') ESCAPE '!'
           ORDER BY lr.priority DESC, lr.id
           LIMIT 1
       ), p.series_key) COLLATE utf8mb4_0900_bin AS value
FROM epg_programs p;

-- +goose Down
DROP VIEW epg_program_series;
DROP VIEW recording_series;
DROP TABLE label_rule_hits;
DROP TABLE label_rules;
DROP TABLE recordings;
DROP TABLE program_intents;
DROP TABLE program_snapshots;
