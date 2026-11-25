-- +goose Up
-- EPGStation の rule ID から録画ルールへの対応。1 ルールに 1 キー（rule_id が主キー）、
-- 1 つの外部 ID は高々 1 ルールに対応する（一意キー）。
CREATE TABLE rule_import_keys (
    rule_id BIGINT NOT NULL,
    source VARCHAR(32) NOT NULL CHECK (source = 'epgstation'),
    external_id VARCHAR(255) NOT NULL CHECK (external_id <> ''),
    PRIMARY KEY (rule_id),
    UNIQUE KEY rule_import_keys_source_external_id (source, external_id),
    FOREIGN KEY (rule_id) REFERENCES rules(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE rule_import_keys;
