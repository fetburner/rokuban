-- +goose Up
CREATE TABLE epg_services (
    site VARCHAR(255) NOT NULL,
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    type INT NOT NULL,
    logo_id INT NOT NULL,
    remote_control_key_id INT NOT NULL,
    name LONGTEXT NOT NULL,
    channel_type VARCHAR(3) NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    channel LONGTEXT NOT NULL,
    has_logo_data BOOLEAN NOT NULL DEFAULT FALSE,
    observed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (site, network_id, service_id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 検索用の列は、全角英数字と全角空白を半角に写してから小文字化する。
CREATE TABLE epg_programs (
    site VARCHAR(255) NOT NULL,
    program_id BIGINT NOT NULL,
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    event_id INT NOT NULL,
    start_at DATETIME(6) NOT NULL,
    duration_ms BIGINT NOT NULL,
    end_at DATETIME(6) NOT NULL,
    is_free BOOLEAN NOT NULL DEFAULT TRUE,
    name LONGTEXT NOT NULL DEFAULT (''),
    description LONGTEXT NOT NULL DEFAULT (''),
    extended JSON,
    genres JSON,
    video JSON,
    audios JSON,
    observed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    genre_lv1 JSON NOT NULL DEFAULT (JSON_ARRAY()),
    series_key LONGTEXT,
    search_name LONGTEXT GENERATED ALWAYS AS (LOWER(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(name,
        'Ａ', 'A'), 'Ｂ', 'B'), 'Ｃ', 'C'), 'Ｄ', 'D'), 'Ｅ', 'E'), 'Ｆ', 'F'), 'Ｇ', 'G'),
        'Ｈ', 'H'), 'Ｉ', 'I'), 'Ｊ', 'J'), 'Ｋ', 'K'), 'Ｌ', 'L'), 'Ｍ', 'M'), 'Ｎ', 'N'),
        'Ｏ', 'O'), 'Ｐ', 'P'), 'Ｑ', 'Q'), 'Ｒ', 'R'), 'Ｓ', 'S'), 'Ｔ', 'T'), 'Ｕ', 'U'),
        'Ｖ', 'V'), 'Ｗ', 'W'), 'Ｘ', 'X'), 'Ｙ', 'Y'), 'Ｚ', 'Z'), 'ａ', 'a'), 'ｂ', 'b'),
        'ｃ', 'c'), 'ｄ', 'd'), 'ｅ', 'e'), 'ｆ', 'f'), 'ｇ', 'g'), 'ｈ', 'h'), 'ｉ', 'i'),
        'ｊ', 'j'), 'ｋ', 'k'), 'ｌ', 'l'), 'ｍ', 'm'), 'ｎ', 'n'), 'ｏ', 'o'), 'ｐ', 'p'),
        'ｑ', 'q'), 'ｒ', 'r'), 'ｓ', 's'), 'ｔ', 't'), 'ｕ', 'u'), 'ｖ', 'v'), 'ｗ', 'w'),
        'ｘ', 'x'), 'ｙ', 'y'), 'ｚ', 'z'), '０', '0'), '１', '1'), '２', '2'), '３', '3'),
        '４', '4'), '５', '5'), '６', '6'), '７', '7'), '８', '8'), '９', '9'), '　', ' '))) VIRTUAL,
    search_description LONGTEXT GENERATED ALWAYS AS (LOWER(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(description,
        'Ａ', 'A'), 'Ｂ', 'B'), 'Ｃ', 'C'), 'Ｄ', 'D'), 'Ｅ', 'E'), 'Ｆ', 'F'), 'Ｇ', 'G'),
        'Ｈ', 'H'), 'Ｉ', 'I'), 'Ｊ', 'J'), 'Ｋ', 'K'), 'Ｌ', 'L'), 'Ｍ', 'M'), 'Ｎ', 'N'),
        'Ｏ', 'O'), 'Ｐ', 'P'), 'Ｑ', 'Q'), 'Ｒ', 'R'), 'Ｓ', 'S'), 'Ｔ', 'T'), 'Ｕ', 'U'),
        'Ｖ', 'V'), 'Ｗ', 'W'), 'Ｘ', 'X'), 'Ｙ', 'Y'), 'Ｚ', 'Z'), 'ａ', 'a'), 'ｂ', 'b'),
        'ｃ', 'c'), 'ｄ', 'd'), 'ｅ', 'e'), 'ｆ', 'f'), 'ｇ', 'g'), 'ｈ', 'h'), 'ｉ', 'i'),
        'ｊ', 'j'), 'ｋ', 'k'), 'ｌ', 'l'), 'ｍ', 'm'), 'ｎ', 'n'), 'ｏ', 'o'), 'ｐ', 'p'),
        'ｑ', 'q'), 'ｒ', 'r'), 'ｓ', 's'), 'ｔ', 't'), 'ｕ', 'u'), 'ｖ', 'v'), 'ｗ', 'w'),
        'ｘ', 'x'), 'ｙ', 'y'), 'ｚ', 'z'), '０', '0'), '１', '1'), '２', '2'), '３', '3'),
        '４', '4'), '５', '5'), '６', '6'), '７', '7'), '８', '8'), '９', '9'), '　', ' '))) VIRTUAL,
    search_extended LONGTEXT GENERATED ALWAYS AS (LOWER(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
        REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(COALESCE(CAST(extended AS CHAR CHARACTER SET utf8mb4), ''),
        'Ａ', 'A'), 'Ｂ', 'B'), 'Ｃ', 'C'), 'Ｄ', 'D'), 'Ｅ', 'E'), 'Ｆ', 'F'), 'Ｇ', 'G'),
        'Ｈ', 'H'), 'Ｉ', 'I'), 'Ｊ', 'J'), 'Ｋ', 'K'), 'Ｌ', 'L'), 'Ｍ', 'M'), 'Ｎ', 'N'),
        'Ｏ', 'O'), 'Ｐ', 'P'), 'Ｑ', 'Q'), 'Ｒ', 'R'), 'Ｓ', 'S'), 'Ｔ', 'T'), 'Ｕ', 'U'),
        'Ｖ', 'V'), 'Ｗ', 'W'), 'Ｘ', 'X'), 'Ｙ', 'Y'), 'Ｚ', 'Z'), 'ａ', 'a'), 'ｂ', 'b'),
        'ｃ', 'c'), 'ｄ', 'd'), 'ｅ', 'e'), 'ｆ', 'f'), 'ｇ', 'g'), 'ｈ', 'h'), 'ｉ', 'i'),
        'ｊ', 'j'), 'ｋ', 'k'), 'ｌ', 'l'), 'ｍ', 'm'), 'ｎ', 'n'), 'ｏ', 'o'), 'ｐ', 'p'),
        'ｑ', 'q'), 'ｒ', 'r'), 'ｓ', 's'), 'ｔ', 't'), 'ｕ', 'u'), 'ｖ', 'v'), 'ｗ', 'w'),
        'ｘ', 'x'), 'ｙ', 'y'), 'ｚ', 'z'), '０', '0'), '１', '1'), '２', '2'), '３', '3'),
        '４', '4'), '５', '5'), '６', '6'), '７', '7'), '８', '8'), '９', '9'), '　', ' '))) VIRTUAL,
    PRIMARY KEY (site, program_id),
    INDEX epg_programs_service_start (site, network_id, service_id, start_at),
    INDEX epg_programs_site_start (site, start_at),
    INDEX epg_programs_site_end (site, end_at),
    INDEX epg_programs_series_key (series_key(191))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE rules (
    id BIGINT NOT NULL AUTO_INCREMENT,
    name LONGTEXT NOT NULL,
    description LONGTEXT NOT NULL DEFAULT (''),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    priority INT NOT NULL DEFAULT 10,
    is_free BOOLEAN,
    duration_min_ms BIGINT,
    duration_max_ms BIGINT,
    period_start_at DATETIME(6),
    period_end_at DATETIME(6),
    dedupe_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    dedupe_threshold FLOAT,
    dedupe_window BIGINT CHECK (dedupe_window > 0),
    keep_original VARCHAR(13) NOT NULL DEFAULT 'always' CHECK (keep_original IN ('always', 'until_encoded')),
    encode_profiles JSON NOT NULL DEFAULT (JSON_ARRAY()),
    filename_template LONGTEXT NOT NULL DEFAULT (''),
    metadata JSON NOT NULL DEFAULT (JSON_OBJECT()),
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    CHECK (duration_min_ms <= duration_max_ms),
    CHECK (NOT dedupe_enabled OR dedupe_threshold IS NOT NULL),
    CHECK (dedupe_threshold > 0 AND dedupe_threshold <= 1),
    CHECK (JSON_TYPE(encode_profiles) = 'ARRAY'),
    CHECK (keep_original <> 'until_encoded' OR JSON_LENGTH(encode_profiles) > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE rule_channel_types (
    rule_id BIGINT NOT NULL,
    channel_type VARCHAR(3) NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    PRIMARY KEY (rule_id, channel_type),
    FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE rule_genres (
    rule_id BIGINT NOT NULL,
    genre_lv1 SMALLINT NOT NULL CHECK (genre_lv1 BETWEEN 0 AND 15),
    PRIMARY KEY (rule_id, genre_lv1),
    FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE rule_services (
    rule_id BIGINT NOT NULL,
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    PRIMARY KEY (rule_id, network_id, service_id),
    FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE rule_sites (
    rule_id BIGINT NOT NULL,
    site VARCHAR(255) NOT NULL CHECK (site <> ''),
    PRIMARY KEY (rule_id, site),
    FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE rule_text_matches (
    rule_id BIGINT NOT NULL,
    seq INT NOT NULL CHECK (seq >= 0),
    target VARCHAR(11) NOT NULL CHECK (target IN ('name', 'description', 'extended')),
    mode VARCHAR(7) NOT NULL CHECK (mode IN ('keyword', 'regex')),
    value LONGTEXT NOT NULL CHECK (value <> ''),
    case_sensitive BOOLEAN NOT NULL DEFAULT FALSE,
    negate BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (rule_id, seq),
    FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TABLE rule_times (
    rule_id BIGINT NOT NULL,
    seq INT NOT NULL CHECK (seq >= 0),
    weekdays INT NOT NULL CHECK (weekdays BETWEEN 1 AND 127),
    start_sec INT NOT NULL CHECK (start_sec BETWEEN 0 AND 86400),
    end_sec INT NOT NULL CHECK (end_sec BETWEEN 0 AND 86400),
    PRIMARY KEY (rule_id, seq),
    FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

CREATE TRIGGER rules_notify_insert AFTER INSERT ON rules FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('rules');
CREATE TRIGGER rules_notify_update AFTER UPDATE ON rules FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('rules');
CREATE TRIGGER rules_notify_delete AFTER DELETE ON rules FOR EACH ROW
    INSERT INTO notifications (payload) VALUES ('rules');

-- +goose Down
DROP TABLE rule_times;
DROP TABLE rule_text_matches;
DROP TABLE rule_sites;
DROP TABLE rule_services;
DROP TABLE rule_genres;
DROP TABLE rule_channel_types;
DROP TABLE rules;
DROP TABLE epg_programs;
DROP TABLE epg_services;
