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
