-- +goose Up

-- reservations.source を落とす（issue #26）。
--
-- source は 2 つの独立した事実を 1 列に載せていた。
--   ①ユーザーが手動で予約したか --- 不可逆な歴史的事実
--   ②いまルールが base を供給しているか --- 毎パス変わる導出状態
-- internal/ruler/sql.go の resolved CTE は
-- `CASE WHEN d.rule_id IS NOT NULL THEN 'rule' ELSE COALESCE(r.source, 'manual') END`
-- で②が①を不可逆に上書きしていた（手動予約にルールが一度でもマッチすると
-- 二度と 'manual' に戻らない）。この歪みは M2-4（00010_program_overrides.sql）で
-- 「録れ / 録るな」（program_intents.action）と「パラメータの上書き」
-- （program_overrides）が分離されたことで解消できるようになった --- ①は
-- 「program_intents に action='record' の行があるか」から、②は
-- 「rule_id IS NOT NULL」から、それぞれ別々に読めるので、1 列に押し込む理由が
-- なくなった（docs/recording.md §4.4「manual 予約との統一」）。
--
-- 落とすのは reservations.source だけ。recordings.source は残す --- 履歴における
-- provenance（この録画が録られた時点でユーザーが録れと言っていたか）は正当な
-- 関心事で、こちらは不変であるべき（録画後に予約側の状態がどう変わろうと
-- 録画の由来は変わらない）。internal/watcher が recordings.source を書く際、
-- reservations.source をコピーするのをやめ、録画時点の program_intents の
-- 有無から都度导出するように変更する（intent{record} があれば manual、
-- 無ければ rule。intent は放送終了まで生きているので録画時点では必ず参照できる）。
--
-- 既存の recordings 行の backfill はしない。誤って 'rule' に化けた
-- recordings.source は本マイグレーション以前に確定した値で、どちらが真の
-- 由来だったか（手動予約 → ルールマッチ → 昇格前の履歴 vs 実際にルール由来）を
-- recordings 行だけから復元する手段がないため。

ALTER TABLE reservations DROP COLUMN source;

-- +goose Down

-- 列を戻す。CHECK は 00002_schema_v1.sql の元の定義に揃える。
ALTER TABLE reservations ADD COLUMN source text;

-- 値の復元は可能な範囲でのみ行う。
--   rule_id IS NOT NULL       -> 'rule'（今まさにルールが base を供給している）
--   それ以外（program_intents に action='record' の行がある） -> 'manual'
--   それ以外                  -> 'rule'（detached/override-only 行。M2-4 以降、
--                                手動予約は必ず program_intents{record} を伴う
--                                ため、intent が無ければルール由来と判断する）
--
-- これは本来の値を保証しない片道の推定に過ぎない。落ちていた列の値自体が
-- 「ルールが一度でもマッチしたら二度と戻らない」という壊れた意味論の産物
-- だったため、そもそも復元すべき「正しい値」が存在しない行がありうる
-- （00010_program_overrides.sql の Down コメントと同じ性質の非対称性）。
UPDATE reservations r
SET source = CASE
    WHEN r.rule_id IS NOT NULL THEN 'rule'
    WHEN EXISTS (
        SELECT 1 FROM program_intents i
        WHERE i.site = r.site AND i.program_id = r.program_id AND i.action = 'record'
    ) THEN 'manual'
    ELSE 'rule'
END;

ALTER TABLE reservations ALTER COLUMN source SET NOT NULL;
ALTER TABLE reservations ADD CONSTRAINT reservations_source_check CHECK (source IN ('rule', 'manual'));
