-- +goose Up

-- until_encoded is a decision made with the derived store selected by this
-- process. The logical derived rows must also have an active, present copy in
-- that store before an original can be removed.
DROP VIEW until_encoded_deletable_originals;

CREATE FUNCTION until_encoded_deletable_originals(desired_store text)
RETURNS SETOF media_assets
LANGUAGE sql STABLE
AS $$
    SELECT a.*
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
      AND NOT EXISTS (
          SELECT 1
          FROM media_assets d
          WHERE d.recording_id = r.id
            AND d.state = 'active'
            AND (
                (d.kind = 'encoded' AND d.profile = ANY(p.encode_profiles))
                OR d.kind IN ('thumbnail', 'seek_tiles')
            )
            AND NOT EXISTS (
                SELECT 1 FROM media_asset_copies c
                WHERE c.media_asset_id = d.id
                  AND c.store = desired_store
                  AND c.state = 'active'
                  AND NOT EXISTS (
                      SELECT 1 FROM missing_media_assets m
                      WHERE m.media_asset_id = d.id AND m.store = desired_store
                  )
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

COMMENT ON FUNCTION until_encoded_deletable_originals(text) IS
    'Returns until_encoded originals only after every desired derived asset has an active, non-missing copy in the supplied derived store.';


-- A transfer attempt row exists only after a failed attempt. Successful work
-- clears it, so absence means no remembered failure.
CREATE TABLE media_asset_transfer_attempts (
    media_asset_id bigint NOT NULL,
    store          text   NOT NULL,
    attempt_count  integer NOT NULL CHECK (attempt_count > 0),
    last_error     text   NOT NULL,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (media_asset_id, store),
    FOREIGN KEY (media_asset_id, store)
        REFERENCES media_asset_copies (media_asset_id, store) ON DELETE CASCADE,
    CONSTRAINT media_asset_transfer_attempts_store_check
        CHECK (store ~ '^[a-z0-9-]+$')
);

COMMENT ON TABLE media_asset_transfer_attempts IS
    'Failure observations for a transfer from a logical media asset to one store; successful transfers delete the row.';

-- A physical copy is prunable only after the current desired copy has been
-- active, present, and past overlap, and another present active copy remains.
-- deleting copies never satisfy either condition.
CREATE FUNCTION media_asset_copy_prunable(
    p_asset_id bigint,
    p_retiring_store text,
    p_original_rules jsonb,
    p_derived_store text,
    p_overlap interval
)
RETURNS boolean
LANGUAGE sql STABLE
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM media_assets a
        JOIN recordings r ON r.id = a.recording_id
        JOIN media_asset_copies retiring
          ON retiring.media_asset_id = a.id
         AND retiring.store = p_retiring_store
        WHERE a.id = p_asset_id
          AND a.state = 'active'
          AND r.deleted_at IS NULL
          AND retiring.state IN ('active', 'deleting')
          AND desired_media_store(
              a.kind, p_original_rules, p_derived_store, r.site, (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
          ) IS DISTINCT FROM p_retiring_store
          AND EXISTS (
              SELECT 1
              FROM media_asset_copies desired
              WHERE desired.media_asset_id = a.id
                AND desired.store = desired_media_store(
                    a.kind, p_original_rules, p_derived_store, r.site, (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
                )
                AND desired.state = 'active'
                AND desired.created_at <= now() - p_overlap
                AND NOT EXISTS (
                    SELECT 1 FROM missing_media_assets m
                    WHERE m.media_asset_id = a.id AND m.store = desired.store
                )
          )
          AND EXISTS (
              SELECT 1
              FROM media_asset_copies survivor
              WHERE survivor.media_asset_id = a.id
                AND survivor.store <> p_retiring_store
                AND survivor.state = 'active'
                AND NOT EXISTS (
                    SELECT 1 FROM missing_media_assets m
                    WHERE m.media_asset_id = a.id AND m.store = survivor.store
                )
          )
    )
$$;

COMMENT ON FUNCTION media_asset_copy_prunable(bigint, text, jsonb, text, interval) IS
    'Checks current policy under the caller''s media_assets row lock; desired and surviving copies must be active and present.';

-- +goose Down

DROP FUNCTION media_asset_copy_prunable(bigint, text, jsonb, text, interval);
DROP TABLE media_asset_transfer_attempts;
DROP FUNCTION until_encoded_deletable_originals(text);

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
