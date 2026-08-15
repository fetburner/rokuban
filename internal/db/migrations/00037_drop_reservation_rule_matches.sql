-- +goose Up

-- 読者がいない導出表を落とす。
--
-- reservation_rule_matches は enabled ルール全件のマッチをトレーサビリティ
-- 目的で毎パス書き換えていたが、読む側が一度も来なかった（internal/api・
-- web に読者ゼロ）。ルール削除の影響プレビュー用途を想定していたが、それは
-- そもそも入力が違う ---
-- DeleteReservationsByRuleWithoutIntent / CountReservationsByRuleWithIntent は
-- どちらも reservations.rule_id（勝者ルール）で引くので、負けたルールを
-- 消しても無効化しても予約は 1 行も変わらない。全マッチを数えると起きない
-- 変化を影響件数として報告してしまう。
--
-- 必要になれば enabled ルールを rulequery.MatchProgramIDsForRule で回せば
-- 同じ集合が作り直せる（ruler が毎パスやっている計算そのもの）。
DROP TABLE reservation_rule_matches;

-- +goose Down

-- 00006_rules.sql の CREATE TABLE / CREATE INDEX をそのまま復元する。
-- 導出表なので中身の復元は不要 --- down 後も書き手は存在しないため空のまま。
CREATE TABLE reservation_rule_matches (
    reservation_id bigint NOT NULL REFERENCES reservations (id) ON DELETE CASCADE,
    rule_id        bigint NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    PRIMARY KEY (reservation_id, rule_id)
);

CREATE INDEX ON reservation_rule_matches (rule_id);
