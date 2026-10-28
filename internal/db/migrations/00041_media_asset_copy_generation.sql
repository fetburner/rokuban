-- +goose Up

ALTER TABLE media_asset_copies
    ADD COLUMN generation bigint;

UPDATE media_asset_copies copy
SET generation = CASE
    WHEN asset.kind = 'original' THEN COALESCE(asset.ingest_generation, 0)
    ELSE COALESCE(asset.derived_generation, 0)
END
FROM media_assets asset
WHERE asset.id = copy.media_asset_id;

ALTER TABLE media_asset_copies
    ALTER COLUMN generation SET NOT NULL,
    ADD CONSTRAINT media_asset_copies_generation_check CHECK (generation >= 0);

-- 旧 binary は insert / path 更新 / pending から active への遷移で generation を省く。
-- rolling update 中も active copy を読めるよう、論理行から補う。
-- +goose StatementBegin
CREATE FUNCTION set_media_asset_copy_generation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF (TG_OP = 'INSERT' AND NEW.generation IS NULL)
       OR (TG_OP = 'UPDATE'
           AND (NEW.rel_path IS DISTINCT FROM OLD.rel_path
                OR (NEW.state = 'active' AND OLD.state = 'pending'))
           AND NEW.generation IS NOT DISTINCT FROM OLD.generation) THEN
        SELECT CASE WHEN asset.kind = 'original' THEN COALESCE(asset.ingest_generation, 0)
                    ELSE COALESCE(asset.derived_generation, 0) END
        INTO NEW.generation
        FROM media_assets asset
        WHERE asset.id = NEW.media_asset_id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER media_asset_copies_set_generation
BEFORE INSERT OR UPDATE OF rel_path, state ON media_asset_copies
FOR EACH ROW EXECUTE FUNCTION set_media_asset_copy_generation();

CREATE INDEX media_asset_copies_generation_idx
    ON media_asset_copies (media_asset_id, generation);

CREATE OR REPLACE FUNCTION active_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT copy.*
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.media_asset_id = p_asset_id
  AND copy.state = 'active'
  AND copy.generation = CASE WHEN asset.kind = 'original'
                             THEN COALESCE(asset.ingest_generation, 0)
                             ELSE COALESCE(asset.derived_generation, 0) END
  AND (p_store IS NULL OR copy.store = p_store)
$$;

-- 既存の名前付き view も同じ現在世代の定義を使う。ここを変えないと、
-- stale copy が encode 不足・CM 検出・削除の条件を満たしたように見えてしまう。
CREATE OR REPLACE VIEW encodable_recordings AS
SELECT r.id AS recording_id
FROM recordings r
WHERE r.deleted_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM media_assets o
      WHERE o.recording_id = r.id
        AND o.kind = 'original'
        AND EXISTS (SELECT 1 FROM active_media_asset_copies(o.id, NULL::text))
  );

COMMENT ON VIEW encodable_recordings IS
    'encodable_recordings は、ごみ箱になく現在世代の原本コピーがある録画を表す。encode の不足の補完・待機列・作り直し要求の列挙が共有する。';

CREATE OR REPLACE VIEW unencoded_desired_profiles AS
SELECT p.recording_id,
       want.profile::text AS profile,
       want.ordinality::bigint AS ordinality
FROM recording_encode_policy p
CROSS JOIN LATERAL unnest(p.encode_profiles) WITH ORDINALITY AS want(profile, ordinality)
WHERE NOT EXISTS (
    SELECT 1
    FROM media_assets e
    WHERE e.recording_id = p.recording_id
      AND e.kind = 'encoded'
      AND e.profile = want.profile
      AND EXISTS (SELECT 1 FROM active_media_asset_copies(e.id, NULL::text))
);

COMMENT ON VIEW unencoded_desired_profiles IS
    'unencoded_desired_profiles は、凍結済みの desired のうち現在世代の encoded コピーが無い (recording_id, profile) を表す。現在の設定では絞らない。encode の不足の補完・待機列と、until_encoded の削除判定が共有する。';

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
      AND e.profile = q.profile
      AND c.keep_ranges = q.requested_keep
      AND EXISTS (SELECT 1 FROM active_media_asset_copies(e.id, NULL::text))
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
          AND EXISTS (SELECT 1 FROM active_media_asset_copies(o2.id, NULL::text))
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
        AND EXISTS (SELECT 1 FROM active_media_asset_copies(o.id, NULL::text))
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
  AND EXISTS (SELECT 1 FROM active_media_asset_copies(o.id, NULL::text))
  AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
  AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
  AND (
      ca.recording_id IS NULL
      OR ca.state <> 'failed'
      OR (l.learned_at IS NOT NULL AND ca.attempted_at < l.learned_at)
  );

CREATE OR REPLACE VIEW removed_encoded_assets AS
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
                SELECT 1 FROM active_media_asset_copies(other.id, NULL::text)
            ))
            OR (
                other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id
                      AND oq.profile = other.profile
                )
                AND EXISTS (
                    SELECT 1 FROM active_media_asset_copies(other.id, NULL::text)
                )
            )
        )
  );

-- +goose Down

DROP TRIGGER media_asset_copies_set_generation ON media_asset_copies;
DROP FUNCTION set_media_asset_copy_generation();

CREATE OR REPLACE FUNCTION active_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT copy.*
FROM media_asset_copies copy
WHERE copy.media_asset_id = p_asset_id
  AND copy.state = 'active'
  AND (p_store IS NULL OR copy.store = p_store)
$$;

DROP INDEX media_asset_copies_generation_idx;
ALTER TABLE media_asset_copies
    DROP CONSTRAINT media_asset_copies_generation_check,
    DROP COLUMN generation;
