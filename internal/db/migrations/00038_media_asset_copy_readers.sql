-- +goose Up

-- The named delete predicates and the rest of the current readers use the physical
-- copy table. Keep this partial index for the asset/store active-copy lookups.
CREATE INDEX media_asset_copies_active_asset_store_idx
    ON media_asset_copies (media_asset_id, store) WHERE state = 'active';

-- 「論理資産 + 置き場 → active コピー」の唯一の定義。p_store が NULL なら全置き場。
-- 存在判定に順序は要らない。ORDER BY を持たないので、planner が呼び出し側へ平坦化できる。
CREATE FUNCTION active_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT c.*
FROM media_asset_copies c
WHERE c.media_asset_id = p_asset_id
  AND c.state = 'active'
  AND (p_store IS NULL OR c.store = p_store)
$$;

COMMENT ON FUNCTION active_media_asset_copies(bigint, text) IS
    'active_media_asset_copies は、論理資産の active コピーを返す。p_store が NULL なら全置き場、指定すればその置き場だけを対象にする。順序は持たない。';

-- 1 件だけ選ぶ読み手が同じコピーを選べるよう、store 順に並べる唯一の定義。
-- 呼び出し側が LIMIT 1 を付ける（関数に LIMIT を入れると planner がインライン展開できない）。
CREATE FUNCTION ordered_active_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT c.* FROM active_media_asset_copies(p_asset_id, p_store) c ORDER BY c.store
$$;

COMMENT ON FUNCTION ordered_active_media_asset_copies(bigint, text) IS
    'ordered_active_media_asset_copies は、active_media_asset_copies を store 順に並べて返す。1 件選ぶ読み手が先頭を使う。';

CREATE OR REPLACE VIEW encodable_recordings AS
SELECT r.id AS recording_id
FROM recordings r
WHERE r.deleted_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM media_assets o
      JOIN media_asset_copies c ON c.media_asset_id = o.id
      WHERE o.recording_id = r.id
        AND o.kind = 'original'
        AND c.state = 'active'
  );

COMMENT ON VIEW encodable_recordings IS
    'encodable_recordings は、ごみ箱になく active な原本コピーがある録画を表す。encode の不足の補完・待機列・作り直し要求の列挙が共有する。';

CREATE OR REPLACE VIEW unencoded_desired_profiles AS
SELECT p.recording_id,
       want.profile::text AS profile,
       want.ordinality::bigint AS ordinality
FROM recording_encode_policy p
CROSS JOIN LATERAL unnest(p.encode_profiles) WITH ORDINALITY AS want(profile, ordinality)
WHERE NOT EXISTS (
    SELECT 1
    FROM media_assets e
    JOIN media_asset_copies c ON c.media_asset_id = e.id
    WHERE e.recording_id = p.recording_id
      AND e.kind = 'encoded'
      AND c.state = 'active'
      AND e.profile = want.profile
);

COMMENT ON VIEW unencoded_desired_profiles IS
    'unencoded_desired_profiles は、凍結済みの desired のうち active な encoded コピーが無い (recording_id, profile) を表す。現在の設定では絞らない。encode の不足の補完・待機列と、until_encoded の削除判定が共有する。';

CREATE OR REPLACE VIEW unsatisfied_encode_rebuilds AS
SELECT q.recording_id,
       q.profile,
       q.requested_keep
FROM encoded_asset_rebuild_requests q
JOIN encodable_recordings er ON er.recording_id = q.recording_id
WHERE NOT EXISTS (
    SELECT 1
    FROM media_assets e
    JOIN media_asset_copies copy ON copy.media_asset_id = e.id
    JOIN media_asset_cuts c ON c.media_asset_id = e.id
    WHERE e.recording_id = q.recording_id
      AND e.kind = 'encoded'
      AND copy.state = 'active'
      AND e.profile = q.profile
      AND c.keep_ranges = q.requested_keep
);

CREATE OR REPLACE VIEW cm_logo_candidate_desired AS
SELECT a.network_id,
       a.service_id,
       a.updated_at AS area_updated_at,
       (SELECT r2.id
        FROM recordings r2
        JOIN media_assets o2 ON o2.recording_id = r2.id AND o2.kind = 'original'
        WHERE r2.network_id = a.network_id
          AND r2.service_id = a.service_id
          AND r2.deleted_at IS NULL
          AND r2.purged_at IS NULL
          AND EXISTS (
              SELECT 1 FROM media_asset_copies c2
              WHERE c2.media_asset_id = o2.id AND c2.state = 'active'
          )
          AND NOT EXISTS (
              SELECT 1 FROM missing_media_assets m2 WHERE m2.media_asset_id = o2.id
          )
        ORDER BY r2.id DESC
        LIMIT 1) AS recording_id
FROM cm_logo_areas a
LEFT JOIN cm_logo_candidates c
  ON c.network_id = a.network_id AND c.service_id = a.service_id
LEFT JOIN cm_logos l
  ON l.network_id = a.network_id AND l.service_id = a.service_id
WHERE c.network_id IS NULL
  AND (l.network_id IS NULL OR l.learned_at < a.updated_at)
  AND EXISTS (
      SELECT 1
      FROM recordings r
      JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original'
      WHERE r.network_id = a.network_id
        AND r.service_id = a.service_id
        AND r.deleted_at IS NULL
        AND r.purged_at IS NULL
        AND EXISTS (
            SELECT 1 FROM media_asset_copies copy
            WHERE copy.media_asset_id = o.id AND copy.state = 'active'
        )
        AND NOT EXISTS (
            SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id
        )
  );

CREATE OR REPLACE VIEW cm_detection_desired AS
SELECT r.id AS recording_id
FROM recordings r
JOIN recording_encode_policy p
  ON p.recording_id = r.id AND p.cm_detect
JOIN media_assets o
  ON o.recording_id = r.id AND o.kind = 'original'
LEFT JOIN current_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN cm_logos l
  ON l.network_id = r.network_id AND l.service_id = r.service_id
WHERE r.deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM media_asset_copies copy
      WHERE copy.media_asset_id = o.id AND copy.state = 'active'
  )
  AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
  AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
  AND (
      ca.recording_id IS NULL
      OR ca.state <> 'failed'
      OR (l.learned_at IS NOT NULL AND ca.attempted_at < l.learned_at)
  );

-- Keep the legacy columns that old binaries select during rolling updates.
DROP VIEW removed_encoded_assets;
CREATE VIEW removed_encoded_assets AS
SELECT a.id AS asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.state
FROM media_assets a
JOIN encoded_asset_removal_requests q
    ON q.recording_id = a.recording_id AND q.profile = a.profile
JOIN recording_encode_policy p ON p.recording_id = a.recording_id
WHERE a.kind = 'encoded'
  AND NOT (a.profile = ANY (p.encode_profiles))
  AND EXISTS (
      SELECT 1
      FROM media_assets other
      WHERE other.recording_id = a.recording_id
        AND (
            (other.kind = 'original' AND EXISTS (
                SELECT 1 FROM media_asset_copies c
                WHERE c.media_asset_id = other.id AND c.state = 'active'
            ))
            OR (
                other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id
                      AND oq.profile = other.profile
                )
                AND EXISTS (
                    SELECT 1 FROM media_asset_copies c
                    WHERE c.media_asset_id = other.id AND c.state = 'active'
                )
            )
        )
  );

-- 削除判定の定義はこの関数 1 つ。置き場ごとに評価するので、新しい読み手は
-- 置き場を渡して呼ぶ。取り返せない削除を決めるので、desired_store が NULL なら
-- 全置き場には倒さず、何も返さない（消さない側）。
CREATE FUNCTION until_encoded_deletable_originals(desired_store text)
RETURNS TABLE (asset_id bigint, recording_id bigint)
LANGUAGE sql STABLE
AS $$
SELECT a.id, a.recording_id
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN recording_encode_policy p ON p.recording_id = r.id
WHERE desired_store IS NOT NULL
  AND a.kind = 'original'
  AND p.keep_original = 'until_encoded'
  AND r.deleted_at IS NULL
  AND cardinality(p.encode_profiles) > 0
  AND NOT EXISTS (
      SELECT 1 FROM unsatisfied_encode_rebuilds q
      WHERE q.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1 FROM current_ts_scanned_originals s
      WHERE s.media_asset_id = a.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM unencoded_desired_profiles u
      WHERE u.recording_id = r.id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM unnest(p.encode_profiles) AS want(profile)
      WHERE NOT EXISTS (
          SELECT 1
          FROM media_assets e
          WHERE e.recording_id = r.id
            AND e.kind = 'encoded'
            AND e.profile = want.profile
            AND EXISTS (SELECT 1 FROM active_media_asset_copies(e.id, desired_store))
      )
  )
  AND EXISTS (
      SELECT 1 FROM media_assets t
      WHERE t.recording_id = r.id
        AND t.kind = 'thumbnail'
        AND EXISTS (SELECT 1 FROM active_media_asset_copies(t.id, desired_store))
  )
  AND EXISTS (
      SELECT 1 FROM media_assets s
      WHERE s.recording_id = r.id
        AND s.kind = 'seek_tiles'
        AND EXISTS (SELECT 1 FROM active_media_asset_copies(s.id, desired_store))
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
$$;

COMMENT ON FUNCTION until_encoded_deletable_originals(text) IS
    'until_encoded_deletable_originals は、desired_store に必要な active 派生コピーが揃い、保持条件を満たす原本を返す。';

-- ローリングアップデート中の旧 binary は、同名の旧 view を引数なしで読み、旧列
-- （rel_path / size_bytes / state）を使う。旧列を削除するまでの互換として、
-- view を関数の薄い包みにする。置き場ごとに関数を 1 回評価し、コピー行ごとには評価しない。
CREATE OR REPLACE VIEW until_encoded_deletable_originals AS
SELECT DISTINCT a.id AS asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.state
FROM (SELECT DISTINCT store FROM media_asset_copies) s
CROSS JOIN LATERAL until_encoded_deletable_originals(s.store) d
JOIN media_assets a ON a.id = d.asset_id;

-- +goose Down

CREATE OR REPLACE VIEW until_encoded_deletable_originals AS
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
      SELECT 1 FROM unsatisfied_encode_rebuilds q
      WHERE q.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1 FROM current_ts_scanned_originals s
      WHERE s.media_asset_id = a.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM unencoded_desired_profiles u
      WHERE u.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1 FROM media_assets t
      WHERE t.recording_id = r.id
        AND t.kind = 'thumbnail'
        AND t.state = 'active'
  )
  AND EXISTS (
      SELECT 1 FROM media_assets s
      WHERE s.recording_id = r.id
        AND s.kind = 'seek_tiles'
        AND s.state = 'active'
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
  );

DROP VIEW removed_encoded_assets;
CREATE VIEW removed_encoded_assets AS
SELECT a.id AS asset_id,
       a.recording_id,
       a.rel_path,
       a.size_bytes,
       a.state
FROM media_assets a
JOIN encoded_asset_removal_requests q
    ON q.recording_id = a.recording_id AND q.profile = a.profile
JOIN recording_encode_policy p ON p.recording_id = a.recording_id
WHERE a.kind = 'encoded'
  AND NOT (a.profile = ANY (p.encode_profiles))
  AND EXISTS (
      SELECT 1
      FROM media_assets other
      WHERE other.recording_id = a.recording_id
        AND other.state = 'active'
        AND (
            other.kind = 'original'
            OR (
                other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id
                      AND oq.profile = other.profile
                )
            )
        )
  );

CREATE OR REPLACE VIEW cm_detection_desired AS
SELECT r.id AS recording_id
FROM recordings r
JOIN recording_encode_policy p
  ON p.recording_id = r.id AND p.cm_detect
JOIN media_assets o
  ON o.recording_id = r.id AND o.kind = 'original' AND o.state = 'active'
LEFT JOIN current_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN cm_logos l
  ON l.network_id = r.network_id AND l.service_id = r.service_id
WHERE r.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
  AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
  AND (
      ca.recording_id IS NULL
      OR ca.state <> 'failed'
      OR (l.learned_at IS NOT NULL AND ca.attempted_at < l.learned_at)
  );

CREATE OR REPLACE VIEW cm_logo_candidate_desired AS
SELECT a.network_id,
       a.service_id,
       a.updated_at AS area_updated_at,
       (SELECT r2.id
        FROM recordings r2
        JOIN media_assets o2 ON o2.recording_id = r2.id
            AND o2.kind = 'original' AND o2.state = 'active'
        WHERE r2.network_id = a.network_id
          AND r2.service_id = a.service_id
          AND r2.deleted_at IS NULL
          AND r2.purged_at IS NULL
          AND NOT EXISTS (
              SELECT 1 FROM missing_media_assets m2 WHERE m2.media_asset_id = o2.id
          )
        ORDER BY r2.id DESC
        LIMIT 1) AS recording_id
FROM cm_logo_areas a
LEFT JOIN cm_logo_candidates c
  ON c.network_id = a.network_id AND c.service_id = a.service_id
LEFT JOIN cm_logos l
  ON l.network_id = a.network_id AND l.service_id = a.service_id
WHERE c.network_id IS NULL
  AND (l.network_id IS NULL OR l.learned_at < a.updated_at)
  AND EXISTS (
      SELECT 1
      FROM recordings r
      JOIN media_assets o ON o.recording_id = r.id
          AND o.kind = 'original' AND o.state = 'active'
      WHERE r.network_id = a.network_id
        AND r.service_id = a.service_id
        AND r.deleted_at IS NULL
        AND r.purged_at IS NULL
        AND NOT EXISTS (
            SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id
        )
  );

CREATE OR REPLACE VIEW unsatisfied_encode_rebuilds AS
SELECT q.recording_id,
       q.profile,
       q.requested_keep
FROM encoded_asset_rebuild_requests q
JOIN encodable_recordings er ON er.recording_id = q.recording_id
WHERE NOT EXISTS (
    SELECT 1
    FROM media_assets e
    JOIN media_asset_cuts c ON c.media_asset_id = e.id
    WHERE e.recording_id = q.recording_id
      AND e.kind = 'encoded'
      AND e.state = 'active'
      AND e.profile = q.profile
      AND c.keep_ranges = q.requested_keep
);

CREATE OR REPLACE VIEW unencoded_desired_profiles AS
SELECT p.recording_id,
       want.profile::text AS profile,
       want.ordinality::bigint AS ordinality
FROM recording_encode_policy p
CROSS JOIN LATERAL unnest(p.encode_profiles) WITH ORDINALITY AS want(profile, ordinality)
WHERE NOT EXISTS (
    SELECT 1 FROM media_assets e
    WHERE e.recording_id = p.recording_id
      AND e.kind = 'encoded'
      AND e.state = 'active'
      AND e.profile = want.profile
);

CREATE OR REPLACE VIEW encodable_recordings AS
SELECT r.id AS recording_id
FROM recordings r
WHERE r.deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM media_assets o
      WHERE o.recording_id = r.id
        AND o.kind = 'original'
        AND o.state = 'active'
  );

DROP FUNCTION until_encoded_deletable_originals(text);
DROP FUNCTION ordered_active_media_asset_copies(bigint, text);
DROP FUNCTION active_media_asset_copies(bigint, text);
DROP INDEX media_asset_copies_active_asset_store_idx;
