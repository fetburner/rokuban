-- +goose Up
-- 名前で引くロック用の行の表。tx 単位の直列化（Postgres の pg_advisory_xact_lock）を、
-- この表の行の SELECT ... FOR UPDATE で代える。GET_LOCK は commit をまたいで保持され、
-- 接続が切れるまで解放されないので使わない。
-- 1 表にするのは、後のタスク（catalog・CM 検出の局・転送のバックログ）が行を足すだけで
-- 同じ機構を使えるようにするため。行は migration で置き、アプリは行を作らない。
CREATE TABLE named_locks (
    name VARCHAR(64) NOT NULL,
    PRIMARY KEY (name)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

INSERT INTO named_locks (name) VALUES ('label_rule_reconcile');

-- 録画のタイトルの勝者。Postgres の label_rule_winner(recordings.title) と同じ式にする。
-- 正規化（全角英数と全角空白を半角へ写し、LOWER する）は epg_programs.search_name と
-- label_rules.keyword_key と同じ写像で、DB 側の式を 3 箇所に持つ。
-- 勝者は priority DESC, id ASC の先頭（label_rule_winner の ORDER BY と同じ）。
CREATE VIEW label_rule_winners AS
SELECT r.id AS recording_id,
       (SELECT lr.id
        FROM label_rules lr
        WHERE LOWER(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE( REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE( REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE( REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE( REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE( REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE( REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE( REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE( REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(r.title, 'Ａ', 'A'), 'Ｂ', 'B'), 'Ｃ', 'C'), 'Ｄ', 'D'), 'Ｅ', 'E'), 'Ｆ', 'F'), 'Ｇ', 'G'), 'Ｈ', 'H'), 'Ｉ', 'I'), 'Ｊ', 'J'), 'Ｋ', 'K'), 'Ｌ', 'L'), 'Ｍ', 'M'), 'Ｎ', 'N'), 'Ｏ', 'O'), 'Ｐ', 'P'), 'Ｑ', 'Q'), 'Ｒ', 'R'), 'Ｓ', 'S'), 'Ｔ', 'T'), 'Ｕ', 'U'), 'Ｖ', 'V'), 'Ｗ', 'W'), 'Ｘ', 'X'), 'Ｙ', 'Y'), 'Ｚ', 'Z'), 'ａ', 'a'), 'ｂ', 'b'), 'ｃ', 'c'), 'ｄ', 'd'), 'ｅ', 'e'), 'ｆ', 'f'), 'ｇ', 'g'), 'ｈ', 'h'), 'ｉ', 'i'), 'ｊ', 'j'), 'ｋ', 'k'), 'ｌ', 'l'), 'ｍ', 'm'), 'ｎ', 'n'), 'ｏ', 'o'), 'ｐ', 'p'), 'ｑ', 'q'), 'ｒ', 'r'), 'ｓ', 's'), 'ｔ', 't'), 'ｕ', 'u'), 'ｖ', 'v'), 'ｗ', 'w'), 'ｘ', 'x'), 'ｙ', 'y'), 'ｚ', 'z'), '０', '0'), '１', '1'), '２', '2'), '３', '3'), '４', '4'), '５', '5'), '６', '6'), '７', '7'), '８', '8'), '９', '9'), '　', ' '))
            COLLATE utf8mb4_0900_bin LIKE CONCAT('%', lr.keyword_key, '%') ESCAPE '!'
        ORDER BY lr.priority DESC, lr.id ASC
        LIMIT 1) AS label_rule_id
FROM recordings r;

-- 録画の INSERT と、タイトルが変わる UPDATE で当たりを作り直す。MySQL は 1 トリガーに 1 イベントしか
-- 書けず WHEN 句も無いので、UPDATE 側は <=> で変化を見てから書く。
-- +goose StatementBegin
CREATE TRIGGER recordings_label_rule_sync_insert AFTER INSERT ON recordings FOR EACH ROW
BEGIN
    INSERT INTO label_rule_hits (recording_id, label_rule_id)
    SELECT w.recording_id, w.label_rule_id
    FROM label_rule_winners w
    WHERE w.recording_id = NEW.id AND w.label_rule_id IS NOT NULL;
END
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER recordings_label_rule_sync_update AFTER UPDATE ON recordings FOR EACH ROW
BEGIN
    IF NOT (OLD.title <=> NEW.title) THEN
        DELETE FROM label_rule_hits WHERE recording_id = NEW.id;
        INSERT INTO label_rule_hits (recording_id, label_rule_id)
        SELECT w.recording_id, w.label_rule_id
        FROM label_rule_winners w
        WHERE w.recording_id = NEW.id AND w.label_rule_id IS NOT NULL;
    END IF;
END
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER recordings_label_rule_sync_update;
DROP TRIGGER recordings_label_rule_sync_insert;
DROP VIEW label_rule_winners;
DROP TABLE named_locks;
