-- +goose Up

ALTER TABLE recording_encode_policy
    ADD COLUMN cm_detect boolean NOT NULL DEFAULT false;

CREATE TABLE recording_cm_detections (
    recording_id bigint PRIMARY KEY REFERENCES recordings (id) ON DELETE CASCADE,
    cm_ranges int8multirange NOT NULL,
    detected_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE recording_cm_attempts (
    recording_id bigint PRIMARY KEY REFERENCES recordings (id) ON DELETE CASCADE,
    state text NOT NULL CHECK (state = ANY (ARRAY['running', 'retrying', 'failed'])),
    error text,
    attempted_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE cm_logos (
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    lgd bytea NOT NULL,
    preview_png bytea,
    learned_at timestamptz NOT NULL DEFAULT now(),
    learned_from bigint REFERENCES recordings (id) ON DELETE SET NULL,
    PRIMARY KEY (network_id, service_id)
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
          WHERE ca.recording_id = r.id AND ca.state = 'failed'
      )
  );

-- +goose Down

DROP VIEW until_encoded_deletable_originals;
CREATE VIEW until_encoded_deletable_originals AS
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
  );

DROP TABLE cm_logos;
DROP TABLE recording_cm_attempts;
DROP TABLE recording_cm_detections;
ALTER TABLE recording_encode_policy DROP COLUMN cm_detect;
