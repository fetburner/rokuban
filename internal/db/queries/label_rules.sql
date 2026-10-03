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
-- 未視聴件数は別の ListRecordingShelvesUnwatchedCount で数える。棚クエリと並列に
-- 実行して、棚の一覧集計へ DISTINCT event 集計の費用を加えない。
--
-- **この形はプランの形に依存する。** 旧母集団（再生できる録画だけ。73,000 行がすべて
-- 再生可能）での過去の実測（別の環境、sqlc / pgx の prepared statement 経由）:
--
--   - その形: 141 ms
--   - 代表と件数を別々の CTE に割る: 231 ms（playable をもう 1 度走査する）
--   - playable（recordings × playable_assets × recording_series の CTE）を
--     MATERIALIZED にしない: 617 ms
--
-- 617 ms の仕組みは、MATERIALIZED を外すと部分一意索引 recordings_unique_active_event
-- が選ばれ、その行数見積もりが 1 になって下流が全部 1 行の計画になり、代表を求める
-- ソートが外側の行数ぶん繰り返されること、だった。
--
-- 下の live は旧 playable に当たる（recordings を走査する CTE）が、MATERIALIZED にしない。
-- 現スキーマ・合成 seed（下記）では 617 ms は再現せず、live を MATERIALIZED にした形は
-- 統合集計より遅い。617 ms の再現条件は未検証なので、再発したら EXPLAIN で計画を調べる。
--
-- playable_assets は参照が 1 回なので MATERIALIZED にしない。合成 seed で指定を外すと
-- 棚集計が 193.6 ms から 157.4 ms になり、結果も一致した。
--
-- 実効シリーズは recording_series ビューが唯一の定義で、ここでも JOIN で読む
-- （COALESCE(lr.value_key, r.series_key) を書き下すと定義が 2 箇所になる）。
-- ビュー経由は書き下しより約 8% 遅かった（旧母集団の形、合成データ 73,000 行・141 棚・
-- 分類ルール 50 本で約 223 ms 対 約 206 ms）。
--
-- 棚の集計に未視聴数を統合すると DISTINCT event の負荷で 200 ms 予算を越えるため、
-- 未視聴集計を別クエリに分ける。`internal/api/shelves_bench_test.go` は
-- `ROKUBAN_BENCH_DATABASE_URL` がなければスキップし、専用 DB で各形を交互に 10 ラウンド
-- 計測する（Apple M3 Max・PostgreSQL 16.2、中央値）:
--
--   - 棚集計だけ（未視聴数なし、playable_assets は非 MATERIALIZED）: 157.4 ms
--   - 棚集計 + 未視聴集計を別接続で並列実行（本番）: 161.1 ms
--   - 同じ 2 クエリを同じ接続で逐次実行: 203.0 ms
--   - 未視聴数を棚集計へ統合: 242.5 ms
--   - playable_assets を MATERIALIZED にする: 193.6 ms
--   - 旧母集団（再生できる録画だけを INNER JOIN、playable は MATERIALIZED）: 191.0 ms
--   - 旧母集団から playable の MATERIALIZED を外す: 180.8 ms
--   - live を MATERIALIZED にする: 252.6 ms
--
-- seed は生きている録画 71,000 行を含む全 73,000 行、141 棚、分類ルール 50 本で、
-- 放送イベントを 2 拠点の録画で作り 10 行に 1 行を視聴済みにする。本番の並列分割形は
-- この合成 seed で 200 ms 予算内だが、実データでの絶対値は未測定である。
-- 予約一覧の EPG JOIN は同じハーネスで 500 件が 16.4 ms、2,000 件が 59.6 ms。
-- 本番の playable_count は旧形の recording_count と全棚で一致する（ハーネスが検査する）。
WITH playable_assets AS (
    SELECT DISTINCT ma.recording_id
    FROM media_assets ma
    WHERE (ma.kind = 'original' AND ma.state <> 'deleted')
       OR (ma.kind = 'encoded' AND ma.state = 'active')
),
live AS (
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

-- name: ListRecordingShelvesUnwatchedCount :many
-- 生きている棚母集団の放送イベントを数える。視聴済み印は録画 id に付くが、印の検索は
-- 全 recordings を見るため、ごみ箱・supersede 済み録画の印も同じイベントへ反映される。
-- イベント単位に先に束ね、既存の recordings_broadcast_event_idx で watched を除外する。
WITH live_events AS (
    SELECT DISTINCT rs.value, r.network_id, r.service_id, r.program_start_at
    FROM recordings r
    JOIN recording_series rs ON rs.recording_id = r.id
    WHERE r.deleted_at IS NULL
      AND r.superseded_at IS NULL
)
SELECT le.value,
       count(*)::bigint AS unwatched_count
FROM live_events le
WHERE NOT EXISTS (
    SELECT 1
    FROM recordings watched_recording
    JOIN recording_watched w ON w.recording_id = watched_recording.id
    WHERE watched_recording.network_id = le.network_id
      AND watched_recording.service_id = le.service_id
      AND watched_recording.program_start_at = le.program_start_at
)
GROUP BY le.value
ORDER BY le.value ASC NULLS LAST;
