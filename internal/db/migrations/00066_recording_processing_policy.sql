-- +goose Up
ALTER TABLE recording_encode_policy RENAME TO recording_processing_policy;
ALTER TABLE recording_processing_policy RENAME CONSTRAINT recording_encode_policy_check TO recording_processing_policy_check;
ALTER TABLE recording_processing_policy RENAME CONSTRAINT recording_encode_policy_keep_original_check TO recording_processing_policy_keep_original_check;
ALTER TABLE recording_processing_policy RENAME CONSTRAINT recording_encode_policy_pkey TO recording_processing_policy_pkey;
ALTER TABLE recording_processing_policy RENAME CONSTRAINT recording_encode_policy_recording_id_fkey TO recording_processing_policy_recording_id_fkey;

-- view は表の OID に追随するため再定義しない。SQL 本文の関数は実行時に
-- 名前を解決するため再定義する。本体は直前の定義の表名だけを置き換える。
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.until_encoded_deletable_originals(desired_store text) RETURNS TABLE(asset_id bigint, recording_id bigint)
    LANGUAGE sql STABLE
    AS $$
SELECT a.id, a.recording_id
FROM media_assets a
JOIN recordings r ON r.id = a.recording_id
JOIN recording_processing_policy p ON p.recording_id = r.id
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
            AND EXISTS (
                SELECT 1 FROM active_present_media_asset_copies(e.id, desired_store) copy
            )
      )
  )
  AND EXISTS (
      SELECT 1
      FROM media_assets thumbnail
      WHERE thumbnail.recording_id = r.id
        AND thumbnail.kind = 'thumbnail'
        AND EXISTS (
            SELECT 1 FROM active_present_media_asset_copies(thumbnail.id, desired_store) copy
        )
  )
  AND EXISTS (
      SELECT 1
      FROM media_assets seek_tiles
      WHERE seek_tiles.recording_id = r.id
        AND seek_tiles.kind = 'seek_tiles'
        AND EXISTS (
            SELECT 1 FROM active_present_media_asset_copies(seek_tiles.id, desired_store) copy
        )
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
-- +goose StatementEnd

-- +goose Down
ALTER TABLE recording_processing_policy RENAME TO recording_encode_policy;
ALTER TABLE recording_encode_policy RENAME CONSTRAINT recording_processing_policy_check TO recording_encode_policy_check;
ALTER TABLE recording_encode_policy RENAME CONSTRAINT recording_processing_policy_keep_original_check TO recording_encode_policy_keep_original_check;
ALTER TABLE recording_encode_policy RENAME CONSTRAINT recording_processing_policy_pkey TO recording_encode_policy_pkey;
ALTER TABLE recording_encode_policy RENAME CONSTRAINT recording_processing_policy_recording_id_fkey TO recording_encode_policy_recording_id_fkey;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.until_encoded_deletable_originals(desired_store text) RETURNS TABLE(asset_id bigint, recording_id bigint)
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
            AND EXISTS (
                SELECT 1 FROM active_present_media_asset_copies(e.id, desired_store) copy
            )
      )
  )
  AND EXISTS (
      SELECT 1
      FROM media_assets thumbnail
      WHERE thumbnail.recording_id = r.id
        AND thumbnail.kind = 'thumbnail'
        AND EXISTS (
            SELECT 1 FROM active_present_media_asset_copies(thumbnail.id, desired_store) copy
        )
  )
  AND EXISTS (
      SELECT 1
      FROM media_assets seek_tiles
      WHERE seek_tiles.recording_id = r.id
        AND seek_tiles.kind = 'seek_tiles'
        AND EXISTS (
            SELECT 1 FROM active_present_media_asset_copies(seek_tiles.id, desired_store) copy
        )
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
-- +goose StatementEnd
