-- name: ListLabelRules :many
-- priority DESC, id ASC はラベルの勝者を決める順と同じ（label_rule_winner）。
-- 並びを 2 箇所に書き下すと、一覧の見た目と実効の順が食い違う。
SELECT * FROM label_rules
ORDER BY priority DESC, id ASC;

-- name: GetLabelRule :one
SELECT * FROM label_rules WHERE id = $1;

-- name: CreateLabelRule :one
INSERT INTO label_rules (key, value, keyword, priority)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpdateLabelRule :one
-- value_key は value の生成列なので、value を変えれば一緒に変わる。
UPDATE label_rules SET
    value      = sqlc.arg(value),
    keyword    = sqlc.arg(keyword),
    priority   = sqlc.arg(priority),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteLabelRule :execrows
DELETE FROM label_rules WHERE id = $1;

-- name: LockLabelRuleReevaluation :exec
-- 全件再評価（worker のジョブと catalog rescue）を直列化する tx スコープの
-- advisory lock。キーはここ 1 箇所で定義し、両方がこのクエリを呼ぶ。並行した 2 本の
-- 古い方が後から新しい方の結果を上書きしうるので、評価する tx の先頭で取る。
SELECT pg_advisory_xact_lock(hashtextextended('rokuban:label-rule-reconcile', 0));

-- name: ApplyLabelRuleReevaluation :one
-- label_rules の変更後に、全録画の当たりを差分だけ適用する。全件を DELETE
-- してから INSERT し直すと、変化が無くてもデッドタプルが全行ぶん出る。
--
--   - upserted: 勝者がいる録画。ON CONFLICT ... WHERE IS DISTINCT FROM で、
--     値が同じ行は UPDATE 自体を起こさない（新しいタプルを作らない）
--   - removed: 勝者が居なくなった録画の行。当たりは勝者しか持たないので、
--     ルールの削除はここでしか落ちない（CASCADE だけだと次点に移らない）
--
-- winners を MATERIALIZED にするのは、2 つのデータ変更 CTE が同じ評価結果を
-- 見るようにするため（評価は label_rule_winner の呼び出しで、行数ぶん走る）。
WITH winners AS MATERIALIZED (
    SELECT r.id AS recording_id, public.label_rule_winner(r.title) AS label_rule_id
    FROM recordings r
),
upserted AS (
    INSERT INTO label_rule_hits (recording_id, label_rule_id)
    SELECT w.recording_id, w.label_rule_id
    FROM winners w
    WHERE w.label_rule_id IS NOT NULL
    ON CONFLICT (recording_id) DO UPDATE
        SET label_rule_id = EXCLUDED.label_rule_id
        WHERE label_rule_hits.label_rule_id IS DISTINCT FROM EXCLUDED.label_rule_id
    RETURNING 1
),
removed AS (
    DELETE FROM label_rule_hits h
    WHERE NOT EXISTS (
        SELECT 1
        FROM winners w
        WHERE w.recording_id = h.recording_id
          AND w.label_rule_id IS NOT NULL
    )
    RETURNING 1
)
SELECT (SELECT count(*) FROM upserted) + (SELECT count(*) FROM removed);

-- name: ListRecordingShelves :many
-- 棚 1 件 = 実効シリーズの値 1 つ。母集団は生きていて再生できる録画
-- （原本の media_asset がある、または encoded の派生物がある）。
--
-- 代表は program_start_at の新しい順で先頭の 1 件。title は代表の生のタイトルで、
-- 値（棚のキー）そのものではない --- 値は正規化の産物なので表示名にならない。
--
-- 値が NULL の棚も返す（UI が「その他」にまとめる材料にする）。値が NULL の行を
-- 落とすと、まとめ先の件数が API からは分からなくなる。
--
-- 値が NULL の棚の行は `GROUP BY value` が 1 つのグループにまとめる（SQL の
-- GROUP BY は NULL を等しいものとして扱う）。
--
-- **この形はプランの形に依存するので、崩すと 4 倍以上遅くなる。** 73,000 行が
-- すべて再生可能な状態での実測（sqlc / pgx の prepared statement 経由）:
--
--   - この形: 141 ms
--   - 代表と件数を別々の CTE に割る: 231 ms（playable をもう 1 度走査する）
--   - playable を MATERIALIZED にしない: 617 ms（下記）
--
-- MATERIALIZED を外すと、プランナは recordings_unique_active_event（部分一意
-- 索引）を選ぶ。一意索引の行数を 1 と見積もるので下流が全部 1 行の計画になり、
-- 代表を求めるソートが外側の行数ぶん繰り返される。**psql で単発実行すると
-- prepared statement ではないのでこの計画を踏まず、146 ms に見える**（アプリは
-- 必ず踏む）。docs/data/series.md §8 の予算はこの経路の値である。
--
-- 実効シリーズは recording_series ビューが唯一の定義で、ここでも JOIN で読む
-- （COALESCE(lr.value_key, r.series_key) を書き下すと定義が 2 箇所になる）。
-- ビュー経由の追加コストは、合成データ（73,000 行・すべて再生可能・141 棚・
-- 分類ルール 50 本、prepared statement 経由 10 回）でこの環境の書き下し 約 206 ms
-- に対し約 223 ms（+8%）。**この環境は書き下しの側が元の測定（141 ms）より遅く、
-- 絶対値の 200 ms 予算はここでは確認できていない**（未測定: 元の測定環境・実データ）。
WITH playable_assets AS MATERIALIZED (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
playable AS MATERIALIZED (
    SELECT r.id,
           r.title,
           r.program_start_at,
           rs.value
    FROM recordings r
    JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
)
SELECT p.value,
       (array_agg(p.title ORDER BY p.program_start_at DESC, p.id DESC))[1]::text AS title,
       count(*) AS recording_count,
       (array_agg(p.id ORDER BY p.program_start_at DESC, p.id DESC))[1]::bigint AS representative_id
FROM playable p
GROUP BY p.value
ORDER BY recording_count DESC, p.value ASC NULLS LAST;
