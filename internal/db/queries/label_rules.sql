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
-- 棚 1 件 = 実効シリーズの値 1 つ。現行の母集団は生きていて再生できる録画
-- （原本の media_asset がある、または encoded の派生物がある）。
--
-- 代表は program_start_at の新しい順で先頭の 1 件。title は代表の生のタイトルで、
-- 値（棚のキー）そのものではない --- 値は正規化の産物なので表示名にならない。
--
-- 値が NULL の棚も返す。棚一覧の UI は NULL を表示対象から外すが、API では
-- 欠落と「分類されていない」を区別できるように残す。
--
-- 値が NULL の棚の行は `GROUP BY value` が 1 つのグループにまとめる（SQL の
-- GROUP BY は NULL を等しいものとして扱う）。
--
-- **この形はプランの形に依存する。** 73,000 行がすべて再生可能な状態での過去の実測
-- （sqlc / pgx の prepared statement 経由）:
--
--   - この形: 141 ms
--   - 代表と件数を別々の CTE に割る: 231 ms（playable をもう 1 度走査する）
--   - playable を MATERIALIZED にしない: 617 ms
--
-- 617 ms の仕組みは、MATERIALIZED を外すと部分一意索引 recordings_unique_active_event
-- が選ばれ、その行数見積もりが 1 になって下流が全部 1 行の計画になり、代表を求める
-- ソートが外側の行数ぶん繰り返されること、だった。**現スキーマ・合成 seed（下記）では
-- この 617 ms は再現しない**（MATERIALIZED を外した形は現行形の 0.92〜0.96 倍で、EXPLAIN でも
-- recordings は Seq Scan のまま部分一意索引を使わない）。再現条件は未検証なので、MATERIALIZED は外さない。
--
-- 実効シリーズは recording_series ビューが唯一の定義で、ここでも JOIN で読む
-- （COALESCE(lr.value_key, r.series_key) を書き下すと定義が 2 箇所になる）。
-- ビュー経由は書き下しより約 8% 遅かった（合成データ 73,000 行・141 棚・分類ルール
-- 50 本で約 223 ms 対 約 206 ms）。
--
-- 母集団を「生きている録画」へ広げた形の測定は `internal/api/shelves_bench_test.go`
-- （`ROKUBAN_BENCH_DATABASE_URL` が無ければスキップ）が専用 DB で再現する。録画 73,000
-- 行（再生可能 65,000・録画中 3,000・ingest 待ち 2,000・failed 1,000・ごみ箱 1,000・
-- superseded 1,000）・141 棚・分類ルール 50 本で、各形を形ごとに交互に 10 ラウンド回した
-- 中央値（Apple M3 Max・PostgreSQL 16.2、3 回実行）:
--
--   - (a) この形: 269〜280 ms
--   - (a') この形から playable の MATERIALIZED を外す: 248〜260 ms（(a) の 0.92〜0.96 倍）
--   - (b) 生きている録画 + playable_assets の LEFT JOIN + count FILTER + max(program_start_at):
--     282〜289 ms（(a) の 1.03〜1.05 倍）
--   - (b') (b) の live を MATERIALIZED にする: 294〜310 ms（(a) の 1.09〜1.12 倍。(b) より遅い）
--
-- 結論: 母集団を広げる形は (b) を採る。同じ環境で現行形の約 1.05 倍で、live の
-- MATERIALIZED は改善にならない。(a) の recording_count と (b) の playable_count は全棚で一致する
-- （ハーネスが検査する）。
-- **絶対値の 200 ms 予算の確認は未測定**（元の測定環境・実データ。この環境は現行形が
-- 予算を越える）。
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
