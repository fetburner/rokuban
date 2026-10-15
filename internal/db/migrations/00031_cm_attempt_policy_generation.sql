-- +goose Up

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
  AND EXISTS (
      SELECT 1
      FROM current_ts_scanned_originals s
      WHERE s.media_asset_id = a.id
  )
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
            AND cm_attempt_is_current_generation(ca.attempted_at, p.cm_detect_changed_at)
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
  AND EXISTS (
      SELECT 1
      FROM current_ts_scanned_originals s
      WHERE s.media_asset_id = a.id
  )
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
