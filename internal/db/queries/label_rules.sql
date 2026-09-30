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
-- 棚 1 件 = 実効シリーズの値 1 つ。母集団は生きている録画
-- （`deleted_at IS NULL AND superseded_at IS NULL`）で、録画中・取り込み待ち・失敗も含む。
--
-- 代表は program_start_at の新しい順で先頭の 1 件。value は画面のシリーズ名に使い、
-- title は代表録画の生タイトルを補助表示する。キーが過剰併合を隠さないよう、両方返す。
--
-- 値が NULL の棚も返す。棚一覧の UI は NULL を表示対象から外すが、API では
-- 欠落と「分類されていない」を区別できるように残す。
--
-- 値が NULL の棚の行は `GROUP BY value` が 1 つのグループにまとめる（SQL の
-- GROUP BY は NULL を等しいものとして扱う）。
--
-- **この形はプランの形に依存する。** 73,000 行を 141 棚に分けた測定は、
-- `internal/api/shelves_bench_test.go` が専用 DB 上で再現する。テストは
-- `ROKUBAN_BENCH_DATABASE_URL` が無い環境ではスキップし、同じ接続の pgx
-- prepared statement 経由で各形を 10 回実行して中央値を出す。データには
-- finished / recording / failed、再生可能な行、再生資産の無い行、deleted / superseded
-- 行を混ぜ、分類ルールを 50 本置く。
--
-- 採用形は、生きている録画を母集団にして `playable_assets` を LEFT JOIN し、
-- `FILTER` で再生可能件数、`max(program_start_at)` で最新開始時刻を同じ集計から返す。
-- `live` を MATERIALIZED にする。測定は `internal/api/shelves_bench_test.go` の
-- 73,000 行・141 棚・50 ルール・混在ステータスを、pgx の prepared statement で
-- 各形 10 回実行した中央値を基準にする（専用 DB URL が無い環境では skip）。
--
-- `playable_assets` の MATERIALIZED を外す候補も測定する。prepared statement と単発の
-- psql ではプランが変わるため、アプリと同じ pgx 経路の中央値を基準にする。
--
-- 実効シリーズは recording_series ビューが唯一の定義で、ここでも JOIN で読む
-- （COALESCE(lr.value_key, r.series_key) を書き下すと定義が 2 箇所になる）。
-- `recording_series` ビュー経由の定義は維持する。測定値は環境依存なので、過去の
-- all-playable データの値を受け入れ条件に固定せず、ハーネスの混在データと実行ログを
-- 変更判断の根拠にする。
WITH playable_assets AS MATERIALIZED (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
live AS MATERIALIZED (
    SELECT r.id,
           r.title,
           r.program_start_at,
           rs.value,
           pa.recording_id AS playable_recording_id
    FROM recordings r
    LEFT JOIN playable_assets pa ON pa.recording_id = r.id
    JOIN recording_series rs ON rs.recording_id = r.id
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
)
SELECT l.value,
       (array_agg(l.title ORDER BY l.program_start_at DESC, l.id DESC))[1]::text AS title,
       count(*) AS recording_count,
       count(*) FILTER (WHERE l.playable_recording_id IS NOT NULL) AS playable_count,
       max(l.program_start_at)::timestamptz AS latest_start_at,
       (array_agg(l.id ORDER BY l.program_start_at DESC, l.id DESC))[1]::bigint AS representative_id
FROM live l
GROUP BY l.value
ORDER BY recording_count DESC, l.value ASC NULLS LAST;
