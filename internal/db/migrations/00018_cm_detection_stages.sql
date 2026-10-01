-- +goose Up

ALTER TABLE recording_cm_attempts
    ADD COLUMN stage text,
    ADD CONSTRAINT recording_cm_attempts_stage_check CHECK (
        stage IS NULL OR stage = ANY (ARRAY[
            'setup', 'probe', 'area', 'logo', 'chapter', 'join', 'parse', 'save', 'stopped'
        ]::text[])
    );

-- CM 検出の投入条件。ingest のヒントと定期 reconcile が同じ述語を読む。
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

-- +goose Down

DROP VIEW cm_detection_desired;
ALTER TABLE recording_cm_attempts
    DROP CONSTRAINT recording_cm_attempts_stage_check,
    DROP COLUMN stage;
