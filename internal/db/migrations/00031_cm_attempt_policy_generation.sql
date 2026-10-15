-- +goose Up

-- CM 検出の要否（cm_detect）が実際に切り替わるたびに 1 ずつ進む世代番号。
-- 試行行は作られた時点の世代を持ち、方針の世代と一致する間だけ「現在の試行」になる。
ALTER TABLE recording_encode_policy
    ADD COLUMN cm_detect_generation bigint NOT NULL DEFAULT 0;

ALTER TABLE recording_cm_attempts
    ADD COLUMN policy_generation bigint NOT NULL DEFAULT 0;
-- 既存行は全方針の世代 0 と一致する世代 0 で始める（方針が無い行も 0）。
ALTER TABLE recording_cm_attempts
    ALTER COLUMN policy_generation DROP DEFAULT;

-- 現在の方針で有効かつ同じ世代の試行だけを返す。試行を読む側（API・章・ロゴ状態・
-- 原本の削除判定・desired）はこの view だけを読み、世代の判定を複製しない。
CREATE VIEW current_cm_attempts AS
SELECT ca.recording_id, ca.state, ca.stage, ca.error, ca.attempted_at,
       ca.attempt_count, ca.policy_generation
FROM recording_cm_attempts ca
JOIN recording_encode_policy p ON p.recording_id = ca.recording_id
WHERE p.cm_detect
  AND ca.policy_generation = p.cm_detect_generation;

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
      SELECT 1 FROM encoded_asset_rebuild_requests q
      WHERE q.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1
      FROM current_ts_scanned_originals s
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
      SELECT 1 FROM encoded_asset_rebuild_requests q
      WHERE q.recording_id = r.id
  )
  AND EXISTS (
      SELECT 1
      FROM current_ts_scanned_originals s
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
          SELECT 1 FROM recording_cm_attempts ca
          WHERE ca.recording_id = r.id
            AND ca.state = 'failed'
            AND ca.stage IS DISTINCT FROM 'adopt'
      )
  );

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

DROP VIEW current_cm_attempts;

ALTER TABLE recording_cm_attempts
    DROP COLUMN policy_generation;

ALTER TABLE recording_encode_policy
    DROP COLUMN cm_detect_generation;
