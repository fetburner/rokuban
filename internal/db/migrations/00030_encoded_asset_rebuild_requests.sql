-- +goose Up

-- 行の存在が「利用者がこの keep 区間でカット版を作り直したい」という未充足の
-- 命令である。現在のチャプターから導出できる cutStale とは別の寿命を持つため、
-- media_assets の子ではなく録画・profile をキーにする衛星表にする。
CREATE SEQUENCE encoded_asset_rebuild_request_generation_seq AS bigint;

CREATE TABLE encoded_asset_rebuild_requests (
    recording_id  bigint NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
    profile       text NOT NULL,
    requested_keep int8multirange NOT NULL CHECK (NOT isempty(requested_keep)),
    request_generation bigint NOT NULL DEFAULT nextval('encoded_asset_rebuild_request_generation_seq') CHECK (request_generation > 0),
    requested_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (recording_id, profile)
);
ALTER SEQUENCE encoded_asset_rebuild_request_generation_seq
    OWNED BY encoded_asset_rebuild_requests.request_generation;

COMMENT ON TABLE encoded_asset_rebuild_requests IS
    '利用者が明示したカット版の再作成要求。requested_keep は操作時点の量子化済み区間。catalog には含めない。';

-- 未充足の作り直し命令がある間は原本を保持する。要求が stale になれば
-- encode_reconcile が比較交換で要求行を消し、次の削除パスで原本を再判定する。
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

DROP TABLE encoded_asset_rebuild_requests;
