-- Run from the repository root with a PostgreSQL connection:
--   psql -X -v ON_ERROR_STOP=1 "postgres://localhost:5432/postgres?sslmode=disable" \
--     -f scripts/measure-until-encoded-store-predicate.sql
--
-- All tables, views, indexes, and functions are temporary and disappear when
-- the psql session ends. The fixture leaves CM detection disabled and rebuild
-- requests empty. It reports calls 6-8 and fails unless they use generic plans.

\set ON_ERROR_STOP on
\echo 'PostgreSQL version and plan cache mode'
SELECT version();
SET plan_cache_mode = auto;

CREATE TEMP TABLE recordings (
    id bigint PRIMARY KEY,
    deleted_at timestamptz
);

CREATE TEMP TABLE recording_encode_policy (
    recording_id bigint PRIMARY KEY,
    keep_original text NOT NULL,
    encode_profiles text[] NOT NULL,
    cm_detect boolean NOT NULL
);

CREATE TEMP TABLE media_assets (
    id bigint PRIMARY KEY,
    recording_id bigint NOT NULL,
    kind text NOT NULL,
    profile text NOT NULL,
    state text NOT NULL,
    rel_path text NOT NULL,
    size_bytes bigint NOT NULL
);

CREATE TEMP TABLE media_asset_ts_scans (
    media_asset_id bigint PRIMARY KEY,
    scanned_size_bytes bigint NOT NULL
);

CREATE TEMP TABLE media_asset_copies (
    media_asset_id bigint NOT NULL,
    store text NOT NULL,
    rel_path text NOT NULL,
    state text NOT NULL,
    created_at timestamptz NOT NULL,
    deleted_at timestamptz
);

INSERT INTO recordings (id, deleted_at)
SELECT n, NULL
FROM generate_series(1, 20000) AS n;

INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
SELECT id, 'keep', ARRAY['h264']::text[], false
FROM recordings;

INSERT INTO media_assets (id, recording_id, kind, profile, state, rel_path, size_bytes)
SELECT id, id, 'original', '', 'active', format('original/%s.ts', id), 100000000
FROM recordings
UNION ALL
SELECT id + 20000, id, 'encoded', 'h264', 'active', format('encoded/%s.mkv', id), 50000000
FROM recordings
WHERE id <= 18000
UNION ALL
SELECT id + 40000, id, 'thumbnail', '', 'active', format('thumbnail/%s.jpg', id), 100000
FROM recordings
WHERE id <= 19400
UNION ALL
SELECT id + 60000, id, 'seek_tiles', '', 'active', format('seek-tiles/%s.jpg', id), 1000000
FROM recordings
WHERE id <= 18800;

INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
SELECT id, size_bytes
FROM media_assets
WHERE kind = 'original'
  AND recording_id <= 19000;

-- Every encoded asset has one active copy. Half have a second copy in the
-- other store, giving 1.5 copies per encoded asset and two store locations.
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, state, created_at, deleted_at)
SELECT e.id,
       CASE WHEN e.recording_id % 2 = 0 THEN 'tokyo'
            WHEN e.recording_id % 4 = 1 THEN 'tokyo'
            ELSE 'takamatsu'
       END,
       format('encoded/%s-%s.mkv', e.recording_id,
              CASE WHEN e.recording_id % 2 = 0 OR e.recording_id % 4 = 1
                   THEN 'tokyo' ELSE 'takamatsu' END),
       'active',
       timestamptz '2025-01-01 00:00:00+00',
       NULL::timestamptz
FROM media_assets e
WHERE e.kind = 'encoded'
UNION ALL
SELECT e.id,
       'takamatsu',
       format('encoded/%s-takamatsu.mkv', e.recording_id),
       'active',
       timestamptz '2025-01-01 00:00:00+00',
       NULL::timestamptz
FROM media_assets e
WHERE e.kind = 'encoded'
  AND e.recording_id % 2 = 0;

-- Match the media_assets indexes in the current migrations.
CREATE INDEX media_assets_kind_state_idx
    ON media_assets (kind, state);
CREATE INDEX media_assets_recording_id_idx
    ON media_assets (recording_id);
CREATE UNIQUE INDEX media_assets_rel_path_idx
    ON media_assets (rel_path)
    WHERE state <> 'deleted';
CREATE UNIQUE INDEX media_asset_copies_live_store_path_idx
    ON media_asset_copies (store, rel_path)
    WHERE state <> 'deleted';

ANALYZE recordings;
ANALYZE recording_encode_policy;
ANALYZE media_assets;
ANALYZE media_asset_ts_scans;
ANALYZE media_asset_copies;

CREATE TEMP VIEW current_ts_scanned_originals AS
SELECT a.id AS media_asset_id
FROM media_assets a
WHERE a.kind = 'original'
  AND (
      SELECT s.scanned_size_bytes
      FROM media_asset_ts_scans s
      WHERE s.media_asset_id = a.id
  ) = a.size_bytes;

CREATE TEMP VIEW unsatisfied_encode_rebuilds AS
SELECT NULL::bigint AS recording_id
WHERE false;

CREATE TEMP VIEW unencoded_desired_profiles AS
SELECT p.recording_id,
       wanted.profile::text AS profile,
       wanted.ordinality::bigint AS ordinality
FROM recording_encode_policy p
CROSS JOIN LATERAL unnest(p.encode_profiles) WITH ORDINALITY AS wanted(profile, ordinality)
WHERE NOT EXISTS (
    SELECT 1
    FROM media_assets e
    WHERE e.recording_id = p.recording_id
      AND e.kind = 'encoded'
      AND e.state = 'active'
      AND e.profile = wanted.profile
);

CREATE TEMP VIEW until_encoded_deletable_originals AS
SELECT a.id AS asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.state
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN recording_encode_policy p ON p.recording_id = r.id
WHERE a.kind = 'original'
  AND p.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
  AND cardinality(p.encode_profiles) > 0
  AND NOT EXISTS (
      SELECT 1
      FROM unsatisfied_encode_rebuilds q
      WHERE q.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1
      FROM current_ts_scanned_originals s
      WHERE s.media_asset_id = a.id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM unencoded_desired_profiles u
      WHERE u.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1
      FROM media_assets t
      WHERE t.recording_id = r.id
        AND t.kind = 'thumbnail'
        AND t.state = 'active'
  )
  AND EXISTS (
      SELECT 1
      FROM media_assets s
      WHERE s.recording_id = r.id
        AND s.kind = 'seek_tiles'
        AND s.state = 'active'
  )
  AND NOT p.cm_detect;

CREATE FUNCTION pg_temp.until_encoded_deletable_originals(desired_store text)
RETURNS TABLE (
    asset_id bigint,
    recording_id bigint,
    rel_path text,
    size_bytes bigint,
    state text
)
LANGUAGE sql STABLE
AS $function$
    SELECT a.id AS asset_id,
           a.recording_id,
           a.rel_path,
           a.size_bytes,
           a.state
    FROM media_assets a
    JOIN recordings r ON r.id = a.recording_id
    JOIN recording_encode_policy p ON p.recording_id = r.id
    WHERE a.kind = 'original'
      AND p.keep_original = 'until_encoded'
      AND r.deleted_at IS NULL
      AND cardinality(p.encode_profiles) > 0
      AND NOT EXISTS (
          SELECT 1
          FROM unsatisfied_encode_rebuilds q
          WHERE q.recording_id = r.id
      )
      AND EXISTS (
          SELECT 1
          FROM current_ts_scanned_originals s
          WHERE s.media_asset_id = a.id
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
          SELECT 1
          FROM media_assets t
          WHERE t.recording_id = r.id
            AND t.kind = 'thumbnail'
            AND t.state = 'active'
      )
      AND EXISTS (
          SELECT 1
          FROM media_assets s
          WHERE s.recording_id = r.id
            AND s.kind = 'seek_tiles'
            AND s.state = 'active'
      )
      AND NOT p.cm_detect
$function$;

\echo 'Synthetic population'
SELECT
    (SELECT count(*) FROM media_assets WHERE kind = 'original') AS originals,
    (SELECT count(*) FROM media_assets WHERE kind = 'encoded') AS encoded,
    (SELECT count(*) FROM media_asset_ts_scans) AS current_size_ts_scans,
    (SELECT count(*) FROM media_assets WHERE kind = 'thumbnail') AS thumbnails,
    (SELECT count(*) FROM media_assets WHERE kind = 'seek_tiles') AS seek_tiles,
    (SELECT count(*) FROM media_asset_copies) AS encoded_copies,
    (SELECT count(DISTINCT store) FROM media_asset_copies) AS stores;

\echo 'Current view, until_encoded 100%, generic plan calls 6-8'
UPDATE recording_encode_policy SET keep_original = 'until_encoded';
ANALYZE recording_encode_policy;
SELECT count(*) AS current_view_rows_100 FROM until_encoded_deletable_originals;
PREPARE current_100(integer) AS SELECT * FROM until_encoded_deletable_originals LIMIT $1;
\o /dev/null
EXECUTE current_100(5000);
EXECUTE current_100(5000);
EXECUTE current_100(5000);
EXECUTE current_100(5000);
EXECUTE current_100(5000);
\o
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE current_100(5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE current_100(5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE current_100(5000);
DO $$ BEGIN
    IF (SELECT generic_plans FROM pg_prepared_statements WHERE name = 'current_100') < 3 THEN
        RAISE EXCEPTION 'current_100 did not use a generic plan for all three measured calls';
    END IF;
END $$;
SELECT name, custom_plans, generic_plans FROM pg_prepared_statements WHERE name = 'current_100';
DEALLOCATE current_100;

\echo 'Current view, until_encoded 50%, generic plan calls 6-8'
UPDATE recording_encode_policy p
SET keep_original = CASE WHEN p.recording_id IN (
    SELECT candidate.recording_id
    FROM recording_encode_policy candidate
    ORDER BY hashtextextended(candidate.recording_id::text, 1616), candidate.recording_id
    LIMIT 10000
) THEN 'until_encoded' ELSE 'keep' END;
ANALYZE recording_encode_policy;
SELECT count(*) AS current_view_rows_50 FROM until_encoded_deletable_originals;
PREPARE current_50(integer) AS SELECT * FROM until_encoded_deletable_originals LIMIT $1;
\o /dev/null
EXECUTE current_50(5000);
EXECUTE current_50(5000);
EXECUTE current_50(5000);
EXECUTE current_50(5000);
EXECUTE current_50(5000);
\o
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE current_50(5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE current_50(5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE current_50(5000);
DO $$ BEGIN
    IF (SELECT generic_plans FROM pg_prepared_statements WHERE name = 'current_50') < 3 THEN
        RAISE EXCEPTION 'current_50 did not use a generic plan for all three measured calls';
    END IF;
END $$;
SELECT name, custom_plans, generic_plans FROM pg_prepared_statements WHERE name = 'current_50';
DEALLOCATE current_50;

\echo 'Current view, until_encoded 20%, generic plan calls 6-8'
UPDATE recording_encode_policy p
SET keep_original = CASE WHEN p.recording_id IN (
    SELECT candidate.recording_id
    FROM recording_encode_policy candidate
    ORDER BY hashtextextended(candidate.recording_id::text, 1616), candidate.recording_id
    LIMIT 4000
) THEN 'until_encoded' ELSE 'keep' END;
ANALYZE recording_encode_policy;
SELECT count(*) AS current_view_rows_20 FROM until_encoded_deletable_originals;
PREPARE current_20(integer) AS SELECT * FROM until_encoded_deletable_originals LIMIT $1;
\o /dev/null
EXECUTE current_20(5000);
EXECUTE current_20(5000);
EXECUTE current_20(5000);
EXECUTE current_20(5000);
EXECUTE current_20(5000);
\o
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE current_20(5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE current_20(5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE current_20(5000);
DO $$ BEGIN
    IF (SELECT generic_plans FROM pg_prepared_statements WHERE name = 'current_20') < 3 THEN
        RAISE EXCEPTION 'current_20 did not use a generic plan for all three measured calls';
    END IF;
END $$;
SELECT name, custom_plans, generic_plans FROM pg_prepared_statements WHERE name = 'current_20';
DEALLOCATE current_20;

\echo 'Store function without media_asset_id/store active index, until_encoded 50%, generic plan calls 6-8'
UPDATE recording_encode_policy p
SET keep_original = CASE WHEN p.recording_id IN (
    SELECT candidate.recording_id
    FROM recording_encode_policy candidate
    ORDER BY hashtextextended(candidate.recording_id::text, 1616), candidate.recording_id
    LIMIT 10000
) THEN 'until_encoded' ELSE 'keep' END;
ANALYZE recording_encode_policy;
SELECT count(*) AS store_function_rows_50_without_index
FROM pg_temp.until_encoded_deletable_originals('tokyo');
PREPARE store_without_index_50(text, integer) AS
    SELECT * FROM pg_temp.until_encoded_deletable_originals($1) LIMIT $2;
\o /dev/null
EXECUTE store_without_index_50('tokyo', 5000);
EXECUTE store_without_index_50('tokyo', 5000);
EXECUTE store_without_index_50('tokyo', 5000);
EXECUTE store_without_index_50('tokyo', 5000);
EXECUTE store_without_index_50('tokyo', 5000);
\o
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_without_index_50('tokyo', 5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_without_index_50('tokyo', 5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_without_index_50('tokyo', 5000);
DO $$ BEGIN
    IF (SELECT generic_plans FROM pg_prepared_statements WHERE name = 'store_without_index_50') < 3 THEN
        RAISE EXCEPTION 'store_without_index_50 did not use a generic plan for all three measured calls';
    END IF;
END $$;
SELECT name, custom_plans, generic_plans FROM pg_prepared_statements WHERE name = 'store_without_index_50';
DEALLOCATE store_without_index_50;

\echo 'Add the candidate index: active copies by logical asset and store'
CREATE INDEX media_asset_copies_active_asset_store_idx
    ON media_asset_copies (media_asset_id, store)
    WHERE state = 'active';
ANALYZE media_asset_copies;

\echo 'Store function with candidate index, until_encoded 100%, generic plan calls 6-8'
UPDATE recording_encode_policy SET keep_original = 'until_encoded';
ANALYZE recording_encode_policy;
SELECT count(*) AS store_function_rows_100
FROM pg_temp.until_encoded_deletable_originals('tokyo');
PREPARE store_indexed_100(text, integer) AS
    SELECT * FROM pg_temp.until_encoded_deletable_originals($1) LIMIT $2;
\o /dev/null
EXECUTE store_indexed_100('tokyo', 5000);
EXECUTE store_indexed_100('tokyo', 5000);
EXECUTE store_indexed_100('tokyo', 5000);
EXECUTE store_indexed_100('tokyo', 5000);
EXECUTE store_indexed_100('tokyo', 5000);
\o
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_indexed_100('tokyo', 5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_indexed_100('tokyo', 5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_indexed_100('tokyo', 5000);
DO $$ BEGIN
    IF (SELECT generic_plans FROM pg_prepared_statements WHERE name = 'store_indexed_100') < 3 THEN
        RAISE EXCEPTION 'store_indexed_100 did not use a generic plan for all three measured calls';
    END IF;
END $$;
SELECT name, custom_plans, generic_plans FROM pg_prepared_statements WHERE name = 'store_indexed_100';
DEALLOCATE store_indexed_100;

\echo 'Store function with candidate index, until_encoded 50%, generic plan calls 6-8'
UPDATE recording_encode_policy p
SET keep_original = CASE WHEN p.recording_id IN (
    SELECT candidate.recording_id
    FROM recording_encode_policy candidate
    ORDER BY hashtextextended(candidate.recording_id::text, 1616), candidate.recording_id
    LIMIT 10000
) THEN 'until_encoded' ELSE 'keep' END;
ANALYZE recording_encode_policy;
SELECT count(*) AS store_function_rows_50
FROM pg_temp.until_encoded_deletable_originals('tokyo');
PREPARE store_indexed_50(text, integer) AS
    SELECT * FROM pg_temp.until_encoded_deletable_originals($1) LIMIT $2;
\o /dev/null
EXECUTE store_indexed_50('tokyo', 5000);
EXECUTE store_indexed_50('tokyo', 5000);
EXECUTE store_indexed_50('tokyo', 5000);
EXECUTE store_indexed_50('tokyo', 5000);
EXECUTE store_indexed_50('tokyo', 5000);
\o
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_indexed_50('tokyo', 5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_indexed_50('tokyo', 5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_indexed_50('tokyo', 5000);
DO $$ BEGIN
    IF (SELECT generic_plans FROM pg_prepared_statements WHERE name = 'store_indexed_50') < 3 THEN
        RAISE EXCEPTION 'store_indexed_50 did not use a generic plan for all three measured calls';
    END IF;
END $$;
SELECT name, custom_plans, generic_plans FROM pg_prepared_statements WHERE name = 'store_indexed_50';
DEALLOCATE store_indexed_50;

\echo 'Store function with candidate index, until_encoded 20%, generic plan calls 6-8'
UPDATE recording_encode_policy p
SET keep_original = CASE WHEN p.recording_id IN (
    SELECT candidate.recording_id
    FROM recording_encode_policy candidate
    ORDER BY hashtextextended(candidate.recording_id::text, 1616), candidate.recording_id
    LIMIT 4000
) THEN 'until_encoded' ELSE 'keep' END;
ANALYZE recording_encode_policy;
SELECT count(*) AS store_function_rows_20
FROM pg_temp.until_encoded_deletable_originals('tokyo');
PREPARE store_indexed_20(text, integer) AS
    SELECT * FROM pg_temp.until_encoded_deletable_originals($1) LIMIT $2;
\o /dev/null
EXECUTE store_indexed_20('tokyo', 5000);
EXECUTE store_indexed_20('tokyo', 5000);
EXECUTE store_indexed_20('tokyo', 5000);
EXECUTE store_indexed_20('tokyo', 5000);
EXECUTE store_indexed_20('tokyo', 5000);
\o
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_indexed_20('tokyo', 5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_indexed_20('tokyo', 5000);
EXPLAIN (ANALYZE, BUFFERS, SETTINGS) EXECUTE store_indexed_20('tokyo', 5000);
DO $$ BEGIN
    IF (SELECT generic_plans FROM pg_prepared_statements WHERE name = 'store_indexed_20') < 3 THEN
        RAISE EXCEPTION 'store_indexed_20 did not use a generic plan for all three measured calls';
    END IF;
END $$;
SELECT name, custom_plans, generic_plans FROM pg_prepared_statements WHERE name = 'store_indexed_20';
DEALLOCATE store_indexed_20;
