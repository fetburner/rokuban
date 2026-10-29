-- +goose Up

-- A copy row follows one physical file through its lifecycle. The logical asset
-- may therefore have several copies in the same store while generations overlap.
DROP VIEW cm_detection_desired;
DROP VIEW cm_logo_candidate_desired;

ALTER TABLE media_asset_copies
    DROP CONSTRAINT media_asset_copies_asset_store_key,
    ADD COLUMN generation bigint;

UPDATE media_asset_copies AS copy
SET generation = CASE
    WHEN asset.kind = 'original' THEN 0
    ELSE COALESCE(media_asset_generation_from_rel_path(copy.rel_path), 0)
END
FROM media_assets AS asset
WHERE asset.id = copy.media_asset_id;

ALTER TABLE media_asset_copies
    ALTER COLUMN generation SET NOT NULL;

-- A store holds at most one live copy per asset generation, so readers never
-- choose between two files for the same content.
CREATE UNIQUE INDEX media_asset_copies_asset_store_generation_idx
    ON media_asset_copies (media_asset_id, store, generation)
    WHERE state <> 'deleted';

COMMENT ON COLUMN media_asset_copies.generation IS
    'Content generation stored in this physical copy; immutable after insert';

-- Missing-file observations describe physical copies, not logical assets. Rows
-- can be rebuilt from a filesystem scan, so discard observations whose copy is
-- already absent instead of carrying an unresolvable legacy key forward.
ALTER TABLE missing_media_assets ADD COLUMN media_asset_copy_id bigint;

UPDATE missing_media_assets AS missing
SET media_asset_copy_id = copy.id
FROM media_asset_copies AS copy
WHERE copy.media_asset_id = missing.media_asset_id
  AND copy.store = missing.store;

DELETE FROM missing_media_assets WHERE media_asset_copy_id IS NULL;

ALTER TABLE missing_media_assets
    DROP CONSTRAINT missing_media_assets_pkey,
    DROP CONSTRAINT missing_media_assets_media_asset_id_fkey;
DROP INDEX missing_media_assets_asset_id_legacy_key;
ALTER TABLE missing_media_assets
    DROP COLUMN media_asset_id,
    DROP COLUMN store,
    ALTER COLUMN media_asset_copy_id SET NOT NULL,
    ADD CONSTRAINT missing_media_assets_pkey PRIMARY KEY (media_asset_copy_id),
    ADD CONSTRAINT missing_media_assets_media_asset_copy_id_fkey
        FOREIGN KEY (media_asset_copy_id) REFERENCES media_asset_copies (id) ON DELETE CASCADE;

COMMENT ON TABLE missing_media_assets IS
    'Physical media copies that a filesystem scan failed to observe';

-- The current-copy predicate is the only definition of which derived generation
-- is visible to readers. Originals have generation zero and no derived counter.
CREATE OR REPLACE FUNCTION active_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT copy.*
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.media_asset_id = p_asset_id
  AND copy.state = 'active'
  AND copy.generation = COALESCE(asset.derived_generation, 0)
  AND (p_store IS NULL OR copy.store = p_store)
$$;

COMMENT ON FUNCTION active_media_asset_copies(bigint, text) IS
    'active_media_asset_copies は、論理資産の derived_generation と一致する active コピーを返す。original の NULL 世代は 0 として扱う。p_store が NULL なら全置き場を対象にする。';

-- Keep the old-generation test named and shared by deletion selection, retry,
-- and the negative predicate used when a deleting copy is restored.
-- created_at on a stale row predates its replacement and cannot start the
-- overlap window. The first copy row for the current generation is committed
-- with its published file; retain that timestamp even if the copy is later
-- deleted so a previously committed replacement does not lose its age.
CREATE VIEW old_generation_media_asset_copies AS
SELECT old_copy.*,
       (SELECT min(current_copy.created_at)
        FROM media_asset_copies current_copy
        WHERE current_copy.media_asset_id = old_copy.media_asset_id
          AND current_copy.generation = COALESCE(asset.derived_generation, 0)) AS generation_started_at
FROM media_asset_copies old_copy
JOIN media_assets asset ON asset.id = old_copy.media_asset_id
WHERE asset.kind <> 'original'
  AND old_copy.generation < COALESCE(asset.derived_generation, 0)
  -- The current generation must still be observable somewhere. If the replacement
  -- file vanished, the old copy may be the only remaining content.
  AND EXISTS (
      SELECT 1
      FROM active_media_asset_copies(old_copy.media_asset_id, NULL) current_copy
      WHERE NOT EXISTS (
          SELECT 1 FROM missing_media_assets missing
          WHERE missing.media_asset_copy_id = current_copy.id
      )
  );

COMMENT ON VIEW old_generation_media_asset_copies IS
    'old_generation_media_asset_copies は、論理資産の derived_generation より古い派生コピーのうち、実体無しでない現行の active コピーがどこかの置き場にあるものを返す。generation_started_at は現行世代の最初のコピー行の created_at で、呼び出し側が overlap の猶予を判定する。';

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
      AND EXISTS (SELECT 1 FROM active_media_asset_copies(e.id, NULL))
);

COMMENT ON VIEW unencoded_desired_profiles IS
    'unencoded_desired_profiles は、凍結済みの desired のうち current active encoded コピーが無い (recording_id, profile) を表す。現在の設定では絞らない。encode の不足の補完・待機列と、until_encoded の削除判定が共有する。';

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
      AND EXISTS (SELECT 1 FROM active_media_asset_copies(e.id, NULL))
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
        AND (
            (other.kind = 'original' AND EXISTS (
                SELECT 1 FROM active_media_asset_copies(other.id, NULL)
            ))
            OR (
                other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id
                      AND oq.profile = other.profile
                )
                AND EXISTS (
                    SELECT 1 FROM active_media_asset_copies(other.id, NULL)
                )
            )
        )
  );

CREATE VIEW cm_logo_candidate_desired AS
SELECT area.network_id,
       area.service_id,
       area.updated_at AS area_updated_at,
       (SELECT r2.id
        FROM recordings r2
        JOIN media_assets original ON original.recording_id = r2.id AND original.kind = 'original'
        WHERE r2.network_id = area.network_id
          AND r2.service_id = area.service_id
          AND r2.deleted_at IS NULL
          AND r2.purged_at IS NULL
          AND EXISTS (
              SELECT 1 FROM media_asset_copies copy
              WHERE copy.media_asset_id = original.id
                AND copy.state = 'active'
                AND NOT EXISTS (
                    SELECT 1 FROM missing_media_assets missing
                    WHERE missing.media_asset_copy_id = copy.id
                )
          )
        ORDER BY r2.id DESC
        LIMIT 1) AS recording_id
FROM cm_logo_areas area
LEFT JOIN cm_logo_candidates candidate
  ON candidate.network_id = area.network_id AND candidate.service_id = area.service_id
LEFT JOIN cm_logos logo
  ON logo.network_id = area.network_id AND logo.service_id = area.service_id
WHERE candidate.network_id IS NULL
  AND (logo.network_id IS NULL OR logo.learned_at < area.updated_at)
  AND EXISTS (
      SELECT 1
      FROM recordings r
      JOIN media_assets original ON original.recording_id = r.id AND original.kind = 'original'
      WHERE r.network_id = area.network_id
        AND r.service_id = area.service_id
        AND r.deleted_at IS NULL
        AND r.purged_at IS NULL
        AND EXISTS (
            SELECT 1 FROM media_asset_copies copy
            WHERE copy.media_asset_id = original.id
              AND copy.state = 'active'
              AND NOT EXISTS (
                  SELECT 1 FROM missing_media_assets missing
                  WHERE missing.media_asset_copy_id = copy.id
              )
        )
  );

CREATE VIEW cm_detection_desired AS
SELECT r.id AS recording_id
FROM recordings r
JOIN recording_encode_policy p
  ON p.recording_id = r.id AND p.cm_detect
JOIN media_assets original
  ON original.recording_id = r.id AND original.kind = 'original'
LEFT JOIN current_cm_attempts attempt ON attempt.recording_id = r.id
LEFT JOIN cm_logos logo
  ON logo.network_id = r.network_id AND logo.service_id = r.service_id
WHERE r.deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM media_asset_copies copy
      WHERE copy.media_asset_id = original.id
        AND copy.state = 'active'
        AND NOT EXISTS (
            SELECT 1 FROM missing_media_assets missing
            WHERE missing.media_asset_copy_id = copy.id
        )
  )
  AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
  AND (
      attempt.recording_id IS NULL
      OR attempt.state <> 'failed'
      OR (logo.learned_at IS NOT NULL AND attempt.attempted_at < logo.learned_at)
  );

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM media_asset_copies
        GROUP BY media_asset_id, store
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot restore one-copy-per-store constraint while generations overlap';
    END IF;
    IF EXISTS (
        SELECT copy.media_asset_id
        FROM missing_media_assets missing
        JOIN media_asset_copies copy ON copy.id = missing.media_asset_copy_id
        GROUP BY copy.media_asset_id
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot restore one missing observation per logical asset while multiple physical copies are missing';
    END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX media_asset_copies_asset_store_generation_idx;

DROP VIEW cm_detection_desired;
DROP VIEW cm_logo_candidate_desired;
DROP VIEW removed_encoded_assets;
CREATE VIEW removed_encoded_assets AS
SELECT a.id AS asset_id, a.recording_id, a.rel_path, a.size_bytes, a.state
FROM media_assets a
JOIN encoded_asset_removal_requests q
  ON q.recording_id = a.recording_id AND q.profile = a.profile
JOIN recording_encode_policy p ON p.recording_id = a.recording_id
WHERE a.kind = 'encoded'
  AND NOT (a.profile = ANY (p.encode_profiles))
  AND EXISTS (
      SELECT 1 FROM media_assets other
      WHERE other.recording_id = a.recording_id
        AND (
            (other.kind = 'original' AND EXISTS (
                SELECT 1 FROM media_asset_copies c
                WHERE c.media_asset_id = other.id AND c.state = 'active'
            ))
            OR (other.kind = 'encoded'
                AND NOT EXISTS (
                    SELECT 1 FROM encoded_asset_removal_requests oq
                    WHERE oq.recording_id = other.recording_id AND oq.profile = other.profile
                )
                AND EXISTS (
                    SELECT 1 FROM media_asset_copies c
                    WHERE c.media_asset_id = other.id AND c.state = 'active'
                ))
        )
  );

CREATE OR REPLACE VIEW unsatisfied_encode_rebuilds AS
SELECT q.recording_id, q.profile, q.requested_keep
FROM encoded_asset_rebuild_requests q
JOIN encodable_recordings er ON er.recording_id = q.recording_id
WHERE NOT EXISTS (
    SELECT 1 FROM media_assets e
    JOIN media_asset_cuts c ON c.media_asset_id = e.id
    JOIN media_asset_copies copy ON copy.media_asset_id = e.id
    WHERE e.recording_id = q.recording_id AND e.kind = 'encoded'
      AND copy.state = 'active' AND e.profile = q.profile
      AND c.keep_ranges = q.requested_keep
);

CREATE OR REPLACE VIEW unencoded_desired_profiles AS
SELECT p.recording_id, want.profile::text AS profile, want.ordinality::bigint AS ordinality
FROM recording_encode_policy p
CROSS JOIN LATERAL unnest(p.encode_profiles) WITH ORDINALITY AS want(profile, ordinality)
WHERE NOT EXISTS (
    SELECT 1 FROM media_assets e
    JOIN media_asset_copies c ON c.media_asset_id = e.id
    WHERE e.recording_id = p.recording_id AND e.kind = 'encoded'
      AND c.state = 'active' AND e.profile = want.profile
);

DROP VIEW old_generation_media_asset_copies;
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
COMMENT ON FUNCTION active_media_asset_copies(bigint, text) IS
    'active_media_asset_copies は指定された論理資産の active コピーを返す。p_store が NULL なら全置き場を対象にする。';

ALTER TABLE missing_media_assets
    DROP CONSTRAINT missing_media_assets_media_asset_copy_id_fkey,
    DROP CONSTRAINT missing_media_assets_pkey,
    ADD COLUMN media_asset_id bigint,
    ADD COLUMN store text;

UPDATE missing_media_assets AS missing
SET media_asset_id = copy.media_asset_id,
    store = copy.store
FROM media_asset_copies AS copy
WHERE copy.id = missing.media_asset_copy_id;

ALTER TABLE missing_media_assets
    DROP COLUMN media_asset_copy_id,
    ALTER COLUMN media_asset_id SET NOT NULL,
    ALTER COLUMN store SET NOT NULL,
    ADD CONSTRAINT missing_media_assets_pkey PRIMARY KEY (media_asset_id, store),
    ADD CONSTRAINT missing_media_assets_media_asset_id_fkey
        FOREIGN KEY (media_asset_id) REFERENCES media_assets (id) ON DELETE CASCADE;
CREATE UNIQUE INDEX missing_media_assets_asset_id_legacy_key ON missing_media_assets (media_asset_id);

CREATE VIEW cm_logo_candidate_desired AS
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
          AND EXISTS (SELECT 1 FROM media_asset_copies c2
                      WHERE c2.media_asset_id = o2.id AND c2.state = 'active')
          AND NOT EXISTS (SELECT 1 FROM missing_media_assets m2 WHERE m2.media_asset_id = o2.id)
        ORDER BY r2.id DESC LIMIT 1) AS recording_id
FROM cm_logo_areas a
LEFT JOIN cm_logo_candidates c ON c.network_id = a.network_id AND c.service_id = a.service_id
LEFT JOIN cm_logos l ON l.network_id = a.network_id AND l.service_id = a.service_id
WHERE c.network_id IS NULL
  AND (l.network_id IS NULL OR l.learned_at < a.updated_at)
  AND EXISTS (
      SELECT 1 FROM recordings r
      JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original'
      WHERE r.network_id = a.network_id AND r.service_id = a.service_id
        AND r.deleted_at IS NULL AND r.purged_at IS NULL
        AND EXISTS (SELECT 1 FROM media_asset_copies copy
                    WHERE copy.media_asset_id = o.id AND copy.state = 'active')
        AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
  );

CREATE VIEW cm_detection_desired AS
SELECT r.id AS recording_id
FROM recordings r
JOIN recording_encode_policy p ON p.recording_id = r.id AND p.cm_detect
JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original'
LEFT JOIN current_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN cm_logos l ON l.network_id = r.network_id AND l.service_id = r.service_id
WHERE r.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM media_asset_copies copy
              WHERE copy.media_asset_id = o.id AND copy.state = 'active')
  AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
  AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
  AND (ca.recording_id IS NULL OR ca.state <> 'failed'
       OR (l.learned_at IS NOT NULL AND ca.attempted_at < l.learned_at));

ALTER TABLE media_asset_copies
    DROP COLUMN generation,
    ADD CONSTRAINT media_asset_copies_asset_store_key UNIQUE (media_asset_id, store);
