-- +goose Up

ALTER TABLE recording_cm_attempts
    DROP CONSTRAINT recording_cm_attempts_stage_check,
    ADD CONSTRAINT recording_cm_attempts_stage_check CHECK (
        stage IS NULL OR stage = ANY (ARRAY[
            'setup', 'probe', 'area', 'logo', 'chapter', 'join', 'parse', 'save',
            'stopped', 'resolution', 'match', 'adopt'
        ]::text[])
    );

-- A candidate is a station asset in progress, not a per-recording detection result.
-- The area FK makes deleting the taught intent delete the candidate atomically.
CREATE TABLE cm_logo_candidates (
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    state text NOT NULL CHECK (state = ANY (ARRAY['running', 'failed', 'ready']::text[])),
    stage text,
    error text,
    x integer NOT NULL,
    y integer NOT NULL,
    w integer NOT NULL,
    h integer NOT NULL,
    coded_width integer NOT NULL,
    coded_height integer NOT NULL,
    recording_id bigint REFERENCES recordings (id) ON DELETE SET NULL,
    observed_area_updated_at timestamptz NOT NULL,
    lgd bytea,
    preview_png bytea,
    attempted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (network_id, service_id),
    FOREIGN KEY (network_id, service_id)
        REFERENCES cm_logo_areas (network_id, service_id) ON DELETE CASCADE,
    CONSTRAINT cm_logo_candidates_rect_within_frame CHECK (
        coded_width > 0
        AND coded_height > 0
        AND x >= 0
        AND y >= 0
        AND w > 0
        AND h > 0
        AND x + w <= coded_width
        AND y + h <= coded_height
    ),
    CONSTRAINT cm_logo_candidates_stage_check CHECK (
        stage IS NULL OR stage = ANY (ARRAY[
            'setup', 'probe', 'area', 'logo', 'match', 'save', 'stopped'
        ]::text[])
    )
);

-- A station has one candidate job at a time. A recording is selected only when an
-- active original is available, so the reconcile pass can always enqueue useful work.
CREATE VIEW cm_logo_candidate_desired AS
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

-- A taught area no longer makes an old detection attempt eligible by itself. The
-- attempt is re-run after adoption (learned_at) or by an explicit retry.
CREATE OR REPLACE VIEW cm_detection_desired AS
SELECT r.id AS recording_id
FROM recordings r
JOIN recording_encode_policy p
  ON p.recording_id = r.id AND p.cm_detect
JOIN media_assets o
  ON o.recording_id = r.id AND o.kind = 'original' AND o.state = 'active'
LEFT JOIN recording_cm_attempts ca ON ca.recording_id = r.id
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
      SELECT 1
      FROM unnest(p.encode_profiles) AS want(profile)
      WHERE NOT EXISTS (
          SELECT 1
          FROM media_assets e
          WHERE e.recording_id = r.id
            AND e.kind = 'encoded'
            AND e.state = 'active'
            AND e.profile = want.profile
      )
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
          SELECT 1 FROM recording_cm_attempts ca
          WHERE ca.recording_id = r.id
            AND ca.state = 'failed'
            AND ca.stage IS DISTINCT FROM 'adopt'
      )
  );

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
      SELECT 1
      FROM unnest(p.encode_profiles) AS want(profile)
      WHERE NOT EXISTS (
          SELECT 1
          FROM media_assets e
          WHERE e.recording_id = r.id
            AND e.kind = 'encoded'
            AND e.state = 'active'
            AND e.profile = want.profile
      )
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
          SELECT 1 FROM recording_cm_attempts ca
          WHERE ca.recording_id = r.id AND ca.state = 'failed'
      )
  );

DROP VIEW cm_detection_desired;
CREATE VIEW cm_detection_desired AS
SELECT r.id AS recording_id
FROM recordings r
JOIN recording_encode_policy p
  ON p.recording_id = r.id AND p.cm_detect
JOIN media_assets o
  ON o.recording_id = r.id AND o.kind = 'original' AND o.state = 'active'
LEFT JOIN recording_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN cm_logos l
  ON l.network_id = r.network_id AND l.service_id = r.service_id
LEFT JOIN cm_logo_areas a
  ON a.network_id = r.network_id AND a.service_id = r.service_id
WHERE r.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
  AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_id = o.id)
  AND (
      ca.recording_id IS NULL
      OR ca.state <> 'failed'
      OR (l.learned_at IS NOT NULL AND ca.attempted_at < l.learned_at)
      OR (a.updated_at IS NOT NULL AND ca.attempted_at < a.updated_at)
  );

DROP VIEW cm_logo_candidate_desired;
DROP TABLE cm_logo_candidates;

UPDATE recording_cm_attempts
SET stage = NULL
WHERE stage = 'adopt';

ALTER TABLE recording_cm_attempts
    DROP CONSTRAINT recording_cm_attempts_stage_check,
    ADD CONSTRAINT recording_cm_attempts_stage_check CHECK (
        stage IS NULL OR stage = ANY (ARRAY[
            'setup', 'probe', 'area', 'logo', 'chapter', 'join', 'parse', 'save',
            'stopped', 'resolution', 'match'
        ]::text[])
    );
