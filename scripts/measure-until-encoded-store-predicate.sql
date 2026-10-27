-- until_encoded の削除可否に「派生物の desired 置き場にコピーがある」を足したときの
-- 計画と時間を、本番の migration を当てた DB の上で測る。
--
-- 使い方（専用 DB を作り直して実行する。他の DB には向けないこと）:
--
--   createdb rokuban_pr1636_bench
--   rokuban migrate up --config <DSN を渡せる最小の config>
--   psql -X -v ON_ERROR_STOP=1 -d rokuban_pr1636_bench \
--     -f scripts/measure-until-encoded-store-predicate.sql > out.txt 2>&1
--
-- 本物の view（until_encoded_deletable_originals / unsatisfied_encode_rebuilds /
-- current_cm_attempts ほか）、本物の表と索引、本物の trash_deletable_recordings /
-- removed_encoded_assets をそのまま測る。このスクリプトが足すのは、まだ存在しない
-- media_asset_copies、置き場を引数に取る関数、コピー表の候補索引だけである。
-- 合成データは本番の表の CHECK / FK / 一意索引を満たす形で入れる。
-- 空の DB でだけ動く（recordings に行があれば止まる）。やり直すときは DB を作り直す。
--
-- 各 statement を prepared statement として 5 回（custom plan）実行し、6〜8 回目を
-- EXPLAIN (ANALYZE, BUFFERS) で測る。pg_prepared_statements が
-- custom_plans = 5 / generic_plans = 3 でなければ失敗する。

\set ON_ERROR_STOP on
\pset pager off
SELECT version();
SET plan_cache_mode = auto;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM recordings) THEN
        RAISE EXCEPTION 'recordings is not empty: run against a freshly migrated bench database';
    END IF;
    IF to_regclass('media_asset_copies') IS NOT NULL THEN
        RAISE EXCEPTION 'media_asset_copies already exists: recreate the bench database';
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- まだ存在しない表: media_asset_copies（列は issue の契約）
-- ---------------------------------------------------------------------------
CREATE TABLE media_asset_copies (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    media_asset_id bigint NOT NULL REFERENCES media_assets (id),
    store          text NOT NULL,
    rel_path       text NOT NULL,
    state          text NOT NULL CHECK (state IN ('pending', 'active', 'deleting', 'deleted')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz
);
CREATE UNIQUE INDEX media_asset_copies_live_store_path_idx
    ON media_asset_copies (store, rel_path)
    WHERE state <> 'deleted';

-- ---------------------------------------------------------------------------
-- 合成データ
--   recordings 2 万。1..19500 は生きている録画、19501..20000 はごみ箱（40 日前）。
--   原本: 全録画。deleting は 17501..17750（述語を満たす）、19001..19250（encoded が無く
--     述語を満たさない）、19501..20000（ごみ箱の録画）。
--   encoded(h264): id <= 18000、thumbnail: <= 19400、seek_tiles: <= 18800、
--     TS 計測（現行サイズ）: 原本のうち id <= 19000。
--   CM 検出: id % 5 = 0 が有効。id % 10 = 0 は検出済み、id % 20 = 5 は stage = 'match' の
--     failed（待ちを終える）、id % 20 = 15 は stage = 'adopt' の failed（待つ）、
--     残りは試行なし（待つ）。
--   作り直し要求: id % 50 = 3 かつ id <= 18000。id % 100 = 3 は同じ区間の凍結 cut を持つ
--     encoded があり満たされている。残りは未充足で原本を保持させる。
-- ---------------------------------------------------------------------------
ALTER TABLE recordings DISABLE TRIGGER USER;
ALTER TABLE media_assets DISABLE TRIGGER USER;

INSERT INTO recordings (id, source, site, network_id, service_id, event_id, service_name,
                        channel_type, channel, program_start_at, program_duration_ms,
                        status, deleted_at)
OVERRIDING SYSTEM VALUE
SELECT n, 'rule', 'tokyo', 1, 1, n, 'svc', 'GR', 'ch',
       timestamptz '2026-01-01 00:00:00+00' + n * interval '1 minute',
       1800000, 'finished',
       CASE WHEN n > 19500 THEN now() - interval '40 days' END
FROM generate_series(1, 20000) AS n;

INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
SELECT id, 'always', ARRAY['h264']::text[], id % 5 = 0
FROM recordings;

INSERT INTO media_assets (id, recording_id, kind, profile, state, rel_path, size_bytes)
OVERRIDING SYSTEM VALUE
SELECT id, id, 'original', NULL::text,
       CASE WHEN id BETWEEN 17501 AND 17750 OR id BETWEEN 19001 AND 19250 OR id > 19500
            THEN 'deleting' ELSE 'active' END,
       format('original/%s.ts', id), 100000000
FROM recordings
UNION ALL
SELECT id + 20000, id, 'encoded', 'h264', 'active', format('encoded/%s.mkv', id), 50000000
FROM recordings WHERE id <= 18000
UNION ALL
SELECT id + 40000, id, 'thumbnail', NULL::text, 'active', format('thumbnail/%s.jpg', id), 100000
FROM recordings WHERE id <= 19400
UNION ALL
SELECT id + 60000, id, 'seek_tiles', NULL::text, 'active', format('seek-tiles/%s.jpg', id), 1000000
FROM recordings WHERE id <= 18800;

INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
SELECT id, size_bytes FROM media_assets WHERE kind = 'original' AND recording_id <= 19000;

INSERT INTO recording_cm_detections (recording_id, cm_ranges)
SELECT id, '{[0,1000)}'::int8multirange FROM recordings WHERE id % 10 = 0;

INSERT INTO recording_cm_attempts (recording_id, state, stage, policy_generation)
SELECT id, 'failed', CASE WHEN id % 20 = 5 THEN 'match' ELSE 'adopt' END, 0
FROM recordings WHERE id % 20 IN (5, 15);

INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
SELECT id, 'h264', '{[0,1000)}'::int8multirange
FROM recordings WHERE id % 50 = 3 AND id <= 18000;

INSERT INTO media_asset_cuts (media_asset_id, keep_ranges)
SELECT id + 20000, '{[0,1000)}'::int8multirange
FROM recordings WHERE id % 100 = 3 AND id <= 18000;

-- コピーの内訳（置き場は tokyo / takamatsu の 2 つ）:
--   encoded 1 資産あたり 1〜2 個。偶数 id は両方、id % 4 = 1 は tokyo だけ、id % 4 = 3 は takamatsu だけ。
--   id % 25 = 11 の tokyo のコピーは deleting（desired の置き場に active が無い）。
--   id % 3 = 0 の encoded には、過去に消した deleted のコピーが両方の置き場に 1 行ずつある。
--   encoded 以外の資産は tokyo に active のコピーが 1 つ。
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at, deleted_at)
SELECT e.id, 'tokyo', format('tokyo/encoded/%s.mkv', e.recording_id),
       CASE WHEN e.recording_id % 25 = 11 THEN 'deleting' ELSE 'active' END,
       timestamptz '2026-01-01+00', NULL::timestamptz
FROM media_assets e
WHERE e.kind = 'encoded' AND (e.recording_id % 2 = 0 OR e.recording_id % 4 = 1)
UNION ALL
SELECT e.id, 'takamatsu', format('takamatsu/encoded/%s.mkv', e.recording_id),
       'active', timestamptz '2026-01-01+00', NULL::timestamptz
FROM media_assets e
WHERE e.kind = 'encoded' AND (e.recording_id % 2 = 0 OR e.recording_id % 4 = 3)
UNION ALL
SELECT e.id, s.store, format('%s/encoded/%s-old.mkv', s.store, e.recording_id),
       'deleted', timestamptz '2025-06-01+00', timestamptz '2025-07-01+00'
FROM media_assets e CROSS JOIN (VALUES ('tokyo'), ('takamatsu')) AS s(store)
WHERE e.kind = 'encoded' AND e.recording_id % 3 = 0
UNION ALL
SELECT a.id, 'tokyo', format('tokyo/%s', a.rel_path), 'active', timestamptz '2026-01-01+00', NULL::timestamptz
FROM media_assets a
WHERE a.kind <> 'encoded';

ALTER TABLE recordings ENABLE TRIGGER USER;
ALTER TABLE media_assets ENABLE TRIGGER USER;
VACUUM (ANALYZE);

\echo '== 合成データの内訳'
SELECT
    (SELECT count(*) FROM recordings) AS recordings,
    (SELECT count(*) FROM media_assets WHERE kind = 'original') AS originals,
    (SELECT count(*) FROM media_assets WHERE kind = 'original' AND state = 'deleting') AS deleting_originals,
    (SELECT count(*) FROM media_assets WHERE kind = 'encoded') AS encoded,
    (SELECT count(*) FROM media_asset_ts_scans) AS ts_scans,
    (SELECT count(*) FROM encoded_asset_rebuild_requests) AS rebuild_requests,
    (SELECT count(*) FROM unsatisfied_encode_rebuilds) AS unsatisfied_rebuilds;
SELECT a.kind, c.store, c.state, count(*) AS copies
FROM media_asset_copies c JOIN media_assets a ON a.id = c.media_asset_id
GROUP BY 1, 2, 3 ORDER BY 1, 2, 3;

-- ---------------------------------------------------------------------------
-- 置き場を引数に取る関数。本物の view の本体のうち、encoded の判定だけを
-- 「desired の置き場に active なコピーがある」に置き換える。それ以外の条件は view と同じ。
--
-- 関数の形は 4 つ。結果の集合は同じで（後で EXCEPT で確かめる）、計画が違う。
--   合成形（until_encoded_deletable_originals）: view と同じ unencoded_desired_profiles の
--       参照を残し、「desired の active な encoded のうち、desired の置き場に active な
--       コピーが無いものが無い」とだけ足す。不足の判定は共有 view に任せ、写さない。
--   view 参照形: unencoded_desired_profiles を残し、コピーの確認を profile ごとの
--       NOT EXISTS の中で encoded とコピーを JOIN して足す（不足の判定を二重に持つ）。
--   join 形: unencoded_desired_profiles を参照せず、profile ごとの NOT EXISTS の中で
--       encoded とコピーを JOIN する（不足の判定を写す）。
--   count 形: desired の profile の数と、desired の置き場に active なコピーがある
--       active な encoded の profile の数を、候補ごとの相関スカラー副問い合わせで比べる。
-- ---------------------------------------------------------------------------
CREATE FUNCTION pg_temp.until_encoded_count_form(desired_store text)
RETURNS TABLE (asset_id bigint, recording_id bigint, rel_path text, size_bytes bigint, state text)
LANGUAGE sql STABLE
AS $fn$
    SELECT a.id AS asset_id, a.recording_id, a.rel_path, a.size_bytes, a.state
    FROM media_assets a
    JOIN recordings r ON r.id = a.recording_id
    JOIN recording_encode_policy p ON p.recording_id = r.id
    WHERE a.kind = 'original'
      AND p.keep_original = 'until_encoded'
      AND r.deleted_at IS NULL
      AND cardinality(p.encode_profiles) > 0
      AND NOT EXISTS (
          SELECT 1 FROM unsatisfied_encode_rebuilds q WHERE q.recording_id = r.id
      )
      AND EXISTS (
          SELECT 1 FROM current_ts_scanned_originals s WHERE s.media_asset_id = a.id
      )
      AND (
          SELECT count(DISTINCT e.profile)
          FROM media_assets e
          JOIN media_asset_copies c ON c.media_asset_id = e.id
          WHERE e.recording_id = r.id
            AND e.kind = 'encoded'
            AND e.state = 'active'
            AND e.profile = ANY (p.encode_profiles)
            AND c.store = desired_store
            AND c.state = 'active'
      ) = (SELECT count(DISTINCT w.profile) FROM unnest(p.encode_profiles) AS w(profile))
      AND EXISTS (
          SELECT 1 FROM media_assets t
          WHERE t.recording_id = r.id AND t.kind = 'thumbnail' AND t.state = 'active'
      )
      AND EXISTS (
          SELECT 1 FROM media_assets s
          WHERE s.recording_id = r.id AND s.kind = 'seek_tiles' AND s.state = 'active'
      )
      AND (
          NOT p.cm_detect
          OR EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
          OR EXISTS (
              SELECT 1 FROM current_cm_attempts ca
              WHERE ca.recording_id = r.id
                AND ca.state = 'failed'
                AND ca.stage IS DISTINCT FROM 'adopt'
          )
      )
$fn$;

CREATE FUNCTION until_encoded_deletable_originals(desired_store text)
RETURNS TABLE (asset_id bigint, recording_id bigint, rel_path text, size_bytes bigint, state text)
LANGUAGE sql STABLE
AS $fn$
    SELECT a.id AS asset_id, a.recording_id, a.rel_path, a.size_bytes, a.state
    FROM media_assets a
    JOIN recordings r ON r.id = a.recording_id
    JOIN recording_encode_policy p ON p.recording_id = r.id
    WHERE a.kind = 'original'
      AND p.keep_original = 'until_encoded'
      AND r.deleted_at IS NULL
      AND cardinality(p.encode_profiles) > 0
      AND NOT EXISTS (
          SELECT 1 FROM unsatisfied_encode_rebuilds q WHERE q.recording_id = r.id
      )
      AND EXISTS (
          SELECT 1 FROM current_ts_scanned_originals s WHERE s.media_asset_id = a.id
      )
      AND NOT EXISTS (
          SELECT 1 FROM unencoded_desired_profiles u WHERE u.recording_id = r.id
      )
      AND NOT EXISTS (
          SELECT 1
          FROM media_assets e
          WHERE e.recording_id = r.id
            AND e.kind = 'encoded'
            AND e.state = 'active'
            AND e.profile = ANY (p.encode_profiles)
            AND NOT EXISTS (
                SELECT 1 FROM media_asset_copies c
                WHERE c.media_asset_id = e.id
                  AND c.store = desired_store
                  AND c.state = 'active'
            )
      )
      AND EXISTS (
          SELECT 1 FROM media_assets t
          WHERE t.recording_id = r.id AND t.kind = 'thumbnail' AND t.state = 'active'
      )
      AND EXISTS (
          SELECT 1 FROM media_assets s
          WHERE s.recording_id = r.id AND s.kind = 'seek_tiles' AND s.state = 'active'
      )
      AND (
          NOT p.cm_detect
          OR EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
          OR EXISTS (
              SELECT 1 FROM current_cm_attempts ca
              WHERE ca.recording_id = r.id
                AND ca.state = 'failed'
                AND ca.stage IS DISTINCT FROM 'adopt'
          )
      )
$fn$;

CREATE FUNCTION pg_temp.until_encoded_join_form(desired_store text)
RETURNS TABLE (asset_id bigint, recording_id bigint, rel_path text, size_bytes bigint, state text)
LANGUAGE sql STABLE
AS $fn$
    SELECT a.id AS asset_id, a.recording_id, a.rel_path, a.size_bytes, a.state
    FROM media_assets a
    JOIN recordings r ON r.id = a.recording_id
    JOIN recording_encode_policy p ON p.recording_id = r.id
    WHERE a.kind = 'original'
      AND p.keep_original = 'until_encoded'
      AND r.deleted_at IS NULL
      AND cardinality(p.encode_profiles) > 0
      AND NOT EXISTS (
          SELECT 1 FROM unsatisfied_encode_rebuilds q WHERE q.recording_id = r.id
      )
      AND EXISTS (
          SELECT 1 FROM current_ts_scanned_originals s WHERE s.media_asset_id = a.id
      )
      AND NOT EXISTS (
          SELECT 1
          FROM unnest(p.encode_profiles) AS wanted(profile)
          WHERE NOT EXISTS (
              SELECT 1
              FROM media_assets e
              JOIN media_asset_copies c ON c.media_asset_id = e.id
              WHERE e.recording_id = r.id
                AND e.kind = 'encoded'
                AND e.state = 'active'
                AND e.profile = wanted.profile
                AND c.store = desired_store
                AND c.state = 'active'
          )
      )
      AND EXISTS (
          SELECT 1 FROM media_assets t
          WHERE t.recording_id = r.id AND t.kind = 'thumbnail' AND t.state = 'active'
      )
      AND EXISTS (
          SELECT 1 FROM media_assets s
          WHERE s.recording_id = r.id AND s.kind = 'seek_tiles' AND s.state = 'active'
      )
      AND (
          NOT p.cm_detect
          OR EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
          OR EXISTS (
              SELECT 1 FROM current_cm_attempts ca
              WHERE ca.recording_id = r.id
                AND ca.state = 'failed'
                AND ca.stage IS DISTINCT FROM 'adopt'
          )
      )
$fn$;

CREATE FUNCTION pg_temp.until_encoded_view_ref_form(desired_store text)
RETURNS TABLE (asset_id bigint, recording_id bigint, rel_path text, size_bytes bigint, state text)
LANGUAGE sql STABLE
AS $fn$
    SELECT a.id AS asset_id, a.recording_id, a.rel_path, a.size_bytes, a.state
    FROM media_assets a
    JOIN recordings r ON r.id = a.recording_id
    JOIN recording_encode_policy p ON p.recording_id = r.id
    WHERE a.kind = 'original'
      AND p.keep_original = 'until_encoded'
      AND r.deleted_at IS NULL
      AND cardinality(p.encode_profiles) > 0
      AND NOT EXISTS (
          SELECT 1 FROM unsatisfied_encode_rebuilds q WHERE q.recording_id = r.id
      )
      AND EXISTS (
          SELECT 1 FROM current_ts_scanned_originals s WHERE s.media_asset_id = a.id
      )
      AND NOT EXISTS (
          SELECT 1 FROM unencoded_desired_profiles u WHERE u.recording_id = r.id
      )
      AND NOT EXISTS (
          SELECT 1
          FROM unnest(p.encode_profiles) AS wanted(profile)
          WHERE NOT EXISTS (
              SELECT 1
              FROM media_assets e
              JOIN media_asset_copies c ON c.media_asset_id = e.id
              WHERE e.recording_id = r.id
                AND e.kind = 'encoded'
                AND e.state = 'active'
                AND e.profile = wanted.profile
                AND c.store = desired_store
                AND c.state = 'active'
          )
      )
      AND EXISTS (
          SELECT 1 FROM media_assets t
          WHERE t.recording_id = r.id AND t.kind = 'thumbnail' AND t.state = 'active'
      )
      AND EXISTS (
          SELECT 1 FROM media_assets s
          WHERE s.recording_id = r.id AND s.kind = 'seek_tiles' AND s.state = 'active'
      )
      AND (
          NOT p.cm_detect
          OR EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
          OR EXISTS (
              SELECT 1 FROM current_cm_attempts ca
              WHERE ca.recording_id = r.id
                AND ca.state = 'failed'
                AND ca.stage IS DISTINCT FROM 'adopt'
          )
      )
$fn$;

-- ---------------------------------------------------------------------------
-- 測定の道具
-- ---------------------------------------------------------------------------
CREATE TEMP TABLE results (
    seq serial, pct int, variant text, query text,
    ms_median numeric, ms_runs text, est_rows bigint, act_rows bigint,
    custom_plans int, generic_plans int
);
CREATE TEMP TABLE plans (seq int, line_no serial, line text);
CREATE TEMP TABLE idx_sizes (idx text PRIMARY KEY, bytes bigint);

-- 6〜8 回目を EXPLAIN (ANALYZE, BUFFERS) で測る。引数は ($1 store text, $2 grace_cutoff
-- timestamptz, $3 row_limit integer) に固定する（使わない引数があっても generic plan の
-- 形は変わらない）。est_rows は Limit の直下の最初のノード（最上位の結合）の見積もり行数、act_rows は Limit が返した行数。
CREATE FUNCTION pg_temp.bench(p_pct int, p_variant text, p_query text, p_sql text) RETURNS void
LANGUAGE plpgsql AS $b$
DECLARE
    nm text := 'b_' || md5(p_pct || p_variant || p_query);
    args text := $a$'tokyo', now() - interval '30 days', 5000$a$;
    ln text; i int; ms numeric[] := '{}'; est bigint; act bigint; n int;
    cp int; gp int; sq int;
BEGIN
    EXECUTE format('PREPARE %I(text, timestamptz, integer) AS %s', nm, p_sql);
    FOR i IN 1..5 LOOP
        EXECUTE format('EXECUTE %I(%s)', nm, args);
    END LOOP;
    INSERT INTO results (pct, variant, query) VALUES (p_pct, p_variant, p_query) RETURNING seq INTO sq;
    FOR i IN 1..3 LOOP
        n := 0;
        FOR ln IN EXECUTE format('EXPLAIN (ANALYZE, BUFFERS) EXECUTE %I(%s)', nm, args) LOOP
            n := n + 1;
            IF ln LIKE 'Execution Time: %' THEN
                ms := ms || substring(ln from 'Execution Time: ([0-9.]+)')::numeric;
            END IF;
            IF i = 3 THEN INSERT INTO plans (seq, line) VALUES (sq, ln); END IF;
            IF i = 3 AND n = 1 THEN
                act := substring(ln from 'actual time=[0-9.]+\.\.[0-9.]+ rows=([0-9]+)')::bigint;
            END IF;
            IF i = 3 AND est IS NULL AND ln LIKE '%->%' THEN
                est := substring(ln from 'cost=[0-9.]+\.\.[0-9.]+ rows=([0-9]+)')::bigint;
            END IF;
        END LOOP;
    END LOOP;
    SELECT custom_plans, generic_plans INTO cp, gp FROM pg_prepared_statements WHERE name = nm;
    IF cp <> 5 OR gp <> 3 THEN
        RAISE EXCEPTION '% % % pct=%: custom_plans=% generic_plans=% (expected 5 / 3)',
            p_variant, p_query, nm, p_pct, cp, gp;
    END IF;
    UPDATE results SET ms_median = (SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY m) FROM unnest(ms) m),
                       ms_runs = (SELECT string_agg(m::text, ' / ') FROM unnest(ms) m),
                       est_rows = est, act_rows = act, custom_plans = cp, generic_plans = gp
    WHERE seq = sq;
    EXECUTE format('DEALLOCATE %I', nm);
END
$b$;

-- src は「until_encoded の述語」を v で参照する FROM 句の断片。
-- 消費側の形は internal/db/queries/delete_reconcile.sql の同名のクエリ。
CREATE FUNCTION pg_temp.bench_set(p_pct int, p_variant text, src text, with_consumers boolean) RETURNS void
LANGUAGE plpgsql AS $b$
BEGIN
    PERFORM pg_temp.bench(p_pct, p_variant, 'producer',
        replace('SELECT v.* FROM {src} LIMIT $3', '{src}', src));
    IF NOT with_consumers THEN RETURN; END IF;
    PERFORM pg_temp.bench(p_pct, p_variant, 'ListUntilEncodedOriginalsToDelete',
        replace($q$SELECT v.asset_id AS id, v.recording_id, v.rel_path, v.size_bytes, 'original'::text AS kind
            FROM {src} WHERE v.state = 'active' ORDER BY v.asset_id LIMIT $3$q$, '{src}', src));
    PERFORM pg_temp.bench_neg(p_pct, p_variant, src);
END
$b$;

-- deleting の原本を引く否定形の 2 クエリ。
CREATE FUNCTION pg_temp.bench_neg(p_pct int, p_variant text, src text) RETURNS void
LANGUAGE plpgsql AS $b$
BEGIN
    PERFORM pg_temp.bench(p_pct, p_variant, 'ListMediaAssetsPendingDelete',
        replace($q$SELECT a.id, a.recording_id, a.rel_path, a.size_bytes, a.kind
            FROM media_assets a
            WHERE a.state = 'deleting'
              AND (
                EXISTS (SELECT 1 FROM trash_deletable_recordings($2) t WHERE t.recording_id = a.recording_id)
                OR EXISTS (SELECT 1 FROM {src} WHERE v.asset_id = a.id)
                OR EXISTS (SELECT 1 FROM removed_encoded_assets x WHERE x.asset_id = a.id)
              )
            ORDER BY a.id LIMIT $3$q$, '{src}', src));
    PERFORM pg_temp.bench(p_pct, p_variant, 'ListUnqualifiedDeletingAssets',
        replace($q$SELECT a.id, a.recording_id, a.rel_path
            FROM media_assets a
            WHERE a.state = 'deleting'
              AND NOT EXISTS (SELECT 1 FROM trash_deletable_recordings($2) t WHERE t.recording_id = a.recording_id)
              AND NOT EXISTS (SELECT 1 FROM {src} WHERE v.asset_id = a.id)
              AND NOT EXISTS (SELECT 1 FROM removed_encoded_assets x WHERE x.asset_id = a.id)
            ORDER BY a.id LIMIT $3$q$, '{src}', src));
END
$b$;

CREATE FUNCTION pg_temp.set_pct(p int) RETURNS void LANGUAGE plpgsql AS $b$
BEGIN
    UPDATE recording_encode_policy
    SET keep_original = CASE WHEN abs(hashtextextended(recording_id::text, 1616) % 100) < p
                             THEN 'until_encoded' ELSE 'always' END;
    ANALYZE recording_encode_policy;
END
$b$;

-- 候補索引。none は索引なし（(store, rel_path) の一意索引と主キーだけ）。
CREATE FUNCTION pg_temp.set_idx(variant text) RETURNS void LANGUAGE plpgsql AS $b$
BEGIN
    DROP INDEX IF EXISTS cand_partial_idx, cand_full_idx, cand_storefirst_idx;
    IF variant = 'partial' THEN
        CREATE INDEX cand_partial_idx ON media_asset_copies (media_asset_id, store) WHERE state = 'active';
    ELSIF variant = 'full' THEN
        CREATE INDEX cand_full_idx ON media_asset_copies (media_asset_id, store, state);
    ELSIF variant = 'storefirst' THEN
        CREATE INDEX cand_storefirst_idx ON media_asset_copies (store, media_asset_id) WHERE state = 'active';
    END IF;
    ANALYZE media_asset_copies;
    INSERT INTO idx_sizes SELECT c.relname, pg_relation_size(c.oid) FROM pg_class c
    WHERE c.relname IN ('cand_partial_idx', 'cand_full_idx', 'cand_storefirst_idx') ON CONFLICT DO NOTHING;
END
$b$;

-- 関数が view と矛盾しないこと: desired の置き場に全 profile の active なコピーがあるという
-- 条件を足しているので、関数の結果は view の結果の部分集合になる。
-- 比較用の 3 形は、合成形と同じ集合を返す。
DO $$
DECLARE extra int; diff int; f text;
BEGIN
    PERFORM pg_temp.set_pct(100);
    PERFORM pg_temp.set_idx('partial'); -- 索引なしだと全件の評価に数分かかる
    SELECT count(*) INTO extra FROM (
        SELECT asset_id FROM until_encoded_deletable_originals('tokyo')
        EXCEPT SELECT asset_id FROM until_encoded_deletable_originals) q;
    IF extra <> 0 THEN RAISE EXCEPTION 'function returns % rows outside the view', extra; END IF;
    FOREACH f IN ARRAY ARRAY['pg_temp.until_encoded_join_form', 'pg_temp.until_encoded_view_ref_form', 'pg_temp.until_encoded_count_form'] LOOP
        EXECUTE format($q$SELECT count(*) FROM (
            (SELECT asset_id FROM until_encoded_deletable_originals('tokyo') EXCEPT SELECT asset_id FROM %1$s('tokyo'))
            UNION ALL
            (SELECT asset_id FROM %1$s('tokyo') EXCEPT SELECT asset_id FROM until_encoded_deletable_originals('tokyo'))) q$q$, f)
            INTO diff;
        IF diff <> 0 THEN RAISE EXCEPTION '% differs from the composed form in % rows', f, diff; END IF;
    END LOOP;
END $$;

\echo '== 適格行数（原本が active のもの。view / 関数 tokyo / 関数 takamatsu。until_encoded 100%）'
SELECT (SELECT count(*) FROM until_encoded_deletable_originals WHERE state = 'active') AS view_rows,
       (SELECT count(*) FROM until_encoded_deletable_originals('tokyo') WHERE state = 'active') AS tokyo_rows,
       (SELECT count(*) FROM until_encoded_deletable_originals('takamatsu') WHERE state = 'active') AS takamatsu_rows;

-- ---------------------------------------------------------------------------
-- 測定。until_encoded の割合ごとに、view と、関数（索引・書き方の候補）を測る。
-- 消費側（delete_reconcile.sql の 3 クエリ）は view と、関数の 4 形（部分索引つき）で測る。
-- ---------------------------------------------------------------------------
VACUUM (ANALYZE) recording_encode_policy;
DO $m$
DECLARE pct int := 100;
BEGIN
    PERFORM pg_temp.set_pct(pct);
    PERFORM pg_temp.set_idx('none');
    PERFORM pg_temp.bench_set(pct, 'view', 'until_encoded_deletable_originals v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_compose_no_index', 'until_encoded_deletable_originals($1) v', true);
    PERFORM pg_temp.set_idx('partial');
    PERFORM pg_temp.bench_set(pct, 'fn_compose_partial', 'until_encoded_deletable_originals($1) v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_join_partial', 'pg_temp.until_encoded_join_form($1) v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_view_ref_partial', 'pg_temp.until_encoded_view_ref_form($1) v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_count_partial', 'pg_temp.until_encoded_count_form($1) v', true);
    PERFORM pg_temp.set_idx('full');
    PERFORM pg_temp.bench_set(pct, 'fn_compose_full', 'until_encoded_deletable_originals($1) v', false);
    PERFORM pg_temp.set_idx('storefirst');
    PERFORM pg_temp.bench_set(pct, 'fn_compose_storefirst', 'until_encoded_deletable_originals($1) v', false);
END $m$;

VACUUM (ANALYZE) recording_encode_policy;
DO $m$
DECLARE pct int := 50;
BEGIN
    PERFORM pg_temp.set_pct(pct);
    PERFORM pg_temp.set_idx('none');
    PERFORM pg_temp.bench_set(pct, 'view', 'until_encoded_deletable_originals v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_compose_no_index', 'until_encoded_deletable_originals($1) v', true);
    -- join 形の索引なしは 1 回が数十秒かかるので、50% の producer だけ測る。
    PERFORM pg_temp.bench_set(pct, 'fn_join_no_index', 'pg_temp.until_encoded_join_form($1) v', false);
    PERFORM pg_temp.set_idx('partial');
    PERFORM pg_temp.bench_set(pct, 'fn_compose_partial', 'until_encoded_deletable_originals($1) v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_join_partial', 'pg_temp.until_encoded_join_form($1) v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_view_ref_partial', 'pg_temp.until_encoded_view_ref_form($1) v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_count_partial', 'pg_temp.until_encoded_count_form($1) v', true);
    PERFORM pg_temp.set_idx('full');
    PERFORM pg_temp.bench_set(pct, 'fn_compose_full', 'until_encoded_deletable_originals($1) v', false);
    PERFORM pg_temp.set_idx('storefirst');
    PERFORM pg_temp.bench_set(pct, 'fn_compose_storefirst', 'until_encoded_deletable_originals($1) v', false);
END $m$;

VACUUM (ANALYZE) recording_encode_policy;
DO $m$
DECLARE pct int := 20;
BEGIN
    PERFORM pg_temp.set_pct(pct);
    PERFORM pg_temp.set_idx('none');
    PERFORM pg_temp.bench_set(pct, 'view', 'until_encoded_deletable_originals v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_compose_no_index', 'until_encoded_deletable_originals($1) v', true);
    PERFORM pg_temp.set_idx('partial');
    PERFORM pg_temp.bench_set(pct, 'fn_compose_partial', 'until_encoded_deletable_originals($1) v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_join_partial', 'pg_temp.until_encoded_join_form($1) v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_view_ref_partial', 'pg_temp.until_encoded_view_ref_form($1) v', true);
    PERFORM pg_temp.bench_set(pct, 'fn_count_partial', 'pg_temp.until_encoded_count_form($1) v', true);
    PERFORM pg_temp.set_idx('full');
    PERFORM pg_temp.bench_set(pct, 'fn_compose_full', 'until_encoded_deletable_originals($1) v', false);
    PERFORM pg_temp.set_idx('storefirst');
    PERFORM pg_temp.bench_set(pct, 'fn_compose_storefirst', 'until_encoded_deletable_originals($1) v', false);
END $m$;

-- 否定形の計画は deleting の件数で変わる。deleting の原本を 20 行（述語を満たす 10 行と
-- 満たさない 10 行）に絞り、否定形の 2 クエリだけ測る。名前の末尾の _del20 がこの条件。
UPDATE media_assets SET state = 'active'
WHERE kind = 'original' AND state = 'deleting'
  AND id NOT IN (SELECT id FROM media_assets WHERE kind = 'original' AND state = 'deleting' AND recording_id <= 17750 ORDER BY id LIMIT 10)
  AND id NOT IN (SELECT id FROM media_assets WHERE kind = 'original' AND state = 'deleting' AND recording_id BETWEEN 19001 AND 19250 ORDER BY id LIMIT 10);
VACUUM (ANALYZE) media_assets;
VACUUM (ANALYZE) recording_encode_policy;
DO $m$
DECLARE pct int := 100;
BEGIN
    PERFORM pg_temp.set_pct(pct);
    PERFORM pg_temp.set_idx('partial');
    PERFORM pg_temp.bench_neg(pct, 'view_del20', 'until_encoded_deletable_originals v');
    PERFORM pg_temp.bench_neg(pct, 'fn_compose_del20', 'until_encoded_deletable_originals($1) v');
    PERFORM pg_temp.bench_neg(pct, 'fn_join_del20', 'pg_temp.until_encoded_join_form($1) v');
    PERFORM pg_temp.bench_neg(pct, 'fn_view_ref_del20', 'pg_temp.until_encoded_view_ref_form($1) v');
    PERFORM pg_temp.bench_neg(pct, 'fn_count_del20', 'pg_temp.until_encoded_count_form($1) v');
END $m$;

VACUUM (ANALYZE) recording_encode_policy;
DO $m$
DECLARE pct int := 50;
BEGIN
    PERFORM pg_temp.set_pct(pct);
    PERFORM pg_temp.set_idx('partial');
    PERFORM pg_temp.bench_neg(pct, 'view_del20', 'until_encoded_deletable_originals v');
    PERFORM pg_temp.bench_neg(pct, 'fn_compose_del20', 'until_encoded_deletable_originals($1) v');
    PERFORM pg_temp.bench_neg(pct, 'fn_join_del20', 'pg_temp.until_encoded_join_form($1) v');
    PERFORM pg_temp.bench_neg(pct, 'fn_view_ref_del20', 'pg_temp.until_encoded_view_ref_form($1) v');
    PERFORM pg_temp.bench_neg(pct, 'fn_count_del20', 'pg_temp.until_encoded_count_form($1) v');
END $m$;

-- encode_profiles が 2 要素（h264 と h265）。h265 の encoded は id <= 17000 にだけある。
SELECT pg_temp.set_idx('partial');
INSERT INTO media_assets (id, recording_id, kind, profile, state, rel_path, size_bytes)
OVERRIDING SYSTEM VALUE
SELECT id + 80000, id, 'encoded', 'h265', 'active', format('encoded/%s-h265.mkv', id), 40000000
FROM recordings WHERE id <= 17000;
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at)
SELECT id, 'tokyo', format('tokyo/%s', rel_path), 'active', timestamptz '2026-01-01+00'
FROM media_assets WHERE profile = 'h265';
UPDATE recording_encode_policy SET encode_profiles = ARRAY['h264', 'h265']::text[];
VACUUM (ANALYZE) media_assets;
VACUUM (ANALYZE) media_asset_copies;
VACUUM (ANALYZE) recording_encode_policy;
DO $m$
BEGIN
    PERFORM pg_temp.set_pct(50);
    PERFORM pg_temp.bench_set(50, 'view_2profiles', 'until_encoded_deletable_originals v', false);
    PERFORM pg_temp.bench_set(50, 'fn_compose_partial_2profiles', 'until_encoded_deletable_originals($1) v', false);
END $m$;

\echo '== 結果（6〜8 回目の Execution Time の中央値）'
SELECT pct, variant, query, ms_median, ms_runs, est_rows, act_rows, custom_plans, generic_plans
FROM results ORDER BY seq;

\echo '== 計画の要点（最上位の結合 / コピー表の引き方）'
SELECT r.pct, r.variant, r.query,
       (SELECT regexp_replace(trim(p.line), '\s+\(cost=.*$', '') FROM plans p
        WHERE p.seq = r.seq AND p.line LIKE '%->%' ORDER BY p.line_no LIMIT 1) AS top_node,
       (SELECT string_agg(regexp_replace(trim(p.line), '\s+\(cost=.*$', '') || ' ' || substring(p.line from 'loops=[0-9]+'), '; ' ORDER BY p.line_no)
        FROM plans p WHERE p.seq = r.seq AND p.line LIKE '%->%' AND p.line LIKE '%media_asset_copies%') AS copies_access,
       (SELECT count(*) FROM plans p WHERE p.seq = r.seq AND p.line LIKE '%encoded_asset_rebuild_requests%') AS rebuild_requests_lines,
       (SELECT count(*) FROM plans p WHERE p.seq = r.seq AND p.line LIKE '%recording_cm_attempts%') AS cm_attempts_lines,
       (SELECT max(substring(p.line from 'Rows Removed by Join Filter: ([0-9]+)')::bigint) FROM plans p
        WHERE p.seq = r.seq AND p.line LIKE '%Rows Removed by Join Filter%') AS max_removed_by_join_filter,
       (SELECT bool_or(p.line LIKE '%Materialize%') FROM plans p WHERE p.seq = r.seq) AS has_materialize
FROM results r ORDER BY r.seq;

\echo '== 候補索引の大きさ（bytes。コピー 9.7 万行の時点）'
SELECT idx, bytes FROM idx_sizes ORDER BY idx;

\echo '== 最後の回の計画（Limit の直下から）'
SELECT r.pct, r.variant, r.query, p.line
FROM results r JOIN plans p USING (seq)
ORDER BY r.seq, p.line_no;
