-- +goose Up

-- コードレビューで発覚: rules.dedupe_threshold / dedupe_window に値域の検査が
-- どこにもなかった。00006_rules.sql の CHECK は
-- "dedupe_enabled = false OR dedupe_threshold IS NOT NULL" しか見ておらず、
-- 値そのものの範囲は API 層にも DB 層にも無かった。
--
-- dedupe_threshold は internal/ruler/dedupe.go で
-- similarity(rec.title, c.title) >= c.dedupe_threshold として使われる。
-- similarity() の値域は [0, 1] なので:
--   * 0 を入れると恒真になる。そのルールに finished の録画が 1 本でもあれば、
--     以降マッチする全番組に base.skip が立ち、録画が黙って止まる
--     （M2-5 のサーキットブレーカーは削除しか守らないのでこの経路は
--     何にも止められない）
--   * 1 を超えると恒偽になり、重複排除が黙って無効化される
-- dedupe_window は "rec.program_start_at >= now() - c.dedupe_window" の
-- 右辺に使われるため、負値を入れると右辺が未来の時刻になり恒偽になる
-- （重複排除が黙って無効化される）。
--
-- 不変条件 10「あってはいけない組み合わせは CHECK で禁止するより表現不可能に
-- する方が強い」の理想からは、real / interval という一般の列型ではこの範囲を
-- 表現不可能にはできない（専用ドメイン型を導入しない限り）。したがって
-- ここでは CHECK を最後の砦として足す。API 層（internal/api/rules.go の
-- validateRuleInput）が人間可読なエラーを返す一次防御、この CHECK は
-- マイグレーション・手作業など API を経由しない書き込み経路に対する防御。
--
-- 既存行がこの CHECK に違反しうる（本番にまだデータがある想定）。
-- 違反する値をそれらしい数値に丸めて推測するのではなく、危険な機能を
-- 安全側（無効化）に倒す: 意図が分からない値を保持したまま重複排除を
-- 有効のままにしておく方が、黙って録画を止め続ける・黙って無効化され続ける
-- という既存の症状を CHECK 追加のタイミングでも継続させてしまう。
-- 無効化しておけば、管理者が改めて妥当な値を入れ直すまでの間は
-- 「重複排除なし」という安全に倒れた状態になる。
UPDATE rules
SET dedupe_enabled = false,
    dedupe_threshold = NULL
WHERE dedupe_threshold IS NOT NULL
  AND NOT (dedupe_threshold > 0 AND dedupe_threshold <= 1);

UPDATE rules
SET dedupe_enabled = false,
    dedupe_window = NULL
WHERE dedupe_window IS NOT NULL
  AND dedupe_window < interval '0';

ALTER TABLE rules
    ADD CONSTRAINT rules_dedupe_threshold_range
        CHECK (dedupe_threshold IS NULL
               OR (dedupe_threshold > 0 AND dedupe_threshold <= 1)),
    ADD CONSTRAINT rules_dedupe_window_nonnegative
        CHECK (dedupe_window IS NULL OR dedupe_window >= interval '0');

-- +goose Down

ALTER TABLE rules
    DROP CONSTRAINT IF EXISTS rules_dedupe_window_nonnegative,
    DROP CONSTRAINT IF EXISTS rules_dedupe_threshold_range;
