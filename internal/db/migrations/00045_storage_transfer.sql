-- +goose Up

-- A transfer attempt exists only while a particular destination copy is failing.
-- Binding it to the physical row also retires failures when a copy generation is
-- replaced or the copy row is deleted.
CREATE TABLE media_asset_transfer_attempts (
    copy_id       bigint PRIMARY KEY REFERENCES media_asset_copies (id) ON DELETE CASCADE,
    attempt_count integer NOT NULL CHECK (attempt_count > 0),
    last_error    text NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE media_asset_transfer_attempts IS
    'Failure observations for one destination media asset copy; successful transfers clear the row.';

-- Candidate selection and its metrics share this predicate; only the attempt
-- threshold differs between waiting and exhausted measurements.
CREATE FUNCTION storage_transfer_candidate(
    p_media_asset_id bigint,
    p_original_rules jsonb,
    p_fs_stores text[],
    p_derived_store text
)
RETURNS boolean
LANGUAGE sql STABLE
AS $$
    SELECT EXISTS (
      SELECT 1
      FROM media_assets a
      JOIN recordings r ON r.id = a.recording_id
      CROSS JOIN LATERAL (
          SELECT desired_media_store(
              a.kind, p_original_rules, p_derived_store, r.site,
              (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz
          ) AS desired_store
      ) placement
      WHERE a.id = p_media_asset_id
      AND r.deleted_at IS NULL
      AND placement.desired_store IS NOT NULL
      AND placement.desired_store = ANY(p_fs_stores)
      AND NOT EXISTS (
          SELECT 1 FROM active_media_asset_copies(a.id, placement.desired_store) target
          WHERE NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = target.id)
      )
      AND EXISTS (
          SELECT 1 FROM active_media_asset_copies(a.id, NULL) source
          WHERE source.store = ANY(p_fs_stores)
            AND source.store <> placement.desired_store
            AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = source.id)
      )
      AND NOT (
          a.kind = 'original'
          AND EXISTS (
              SELECT 1 FROM until_encoded_deletable_originals(p_derived_store) u
              WHERE u.asset_id = a.id
          )
      )
    )
$$;

COMMENT ON FUNCTION storage_transfer_candidate(bigint, jsonb, text[], text) IS
    'Returns true for an active logical asset whose desired FS copy is absent and whose source is present in a configured FS store.';

-- Pending rows are reservations, not published copies. Retire one only after the
-- current placement has made that reservation obsolete; a failed but still desired
-- transfer remains pending so its per-copy retry limit and checkpoint can continue.
CREATE FUNCTION storage_transfer_pending_copy_obsolete(
    p_copy_id bigint,
    p_original_rules jsonb,
    p_fs_stores text[],
    p_derived_store text
)
RETURNS boolean
LANGUAGE sql STABLE
AS $$
    SELECT EXISTS (
      SELECT 1
      FROM media_asset_copies copy
      JOIN media_assets asset ON asset.id = copy.media_asset_id
      JOIN recordings recording ON recording.id = asset.recording_id
      CROSS JOIN LATERAL (
          SELECT desired_media_store(
              asset.kind, p_original_rules, p_derived_store, recording.site,
              (recording.program_start_at + recording.program_duration_ms * interval '1 millisecond')::timestamptz
          ) AS desired_store
      ) placement
      WHERE copy.id = p_copy_id
        AND copy.state = 'pending'
        AND (
            recording.deleted_at IS NOT NULL
            OR copy.generation <> COALESCE(asset.derived_generation, 0)
            OR placement.desired_store IS DISTINCT FROM copy.store
            OR NOT (placement.desired_store = ANY(p_fs_stores))
            OR (
                asset.kind = 'original'
                AND EXISTS (
                    SELECT 1 FROM until_encoded_deletable_originals(p_derived_store) u
                    WHERE u.asset_id = asset.id
                )
            )
        )
    )
$$;

COMMENT ON FUNCTION storage_transfer_pending_copy_obsolete(bigint, jsonb, text[], text) IS
    'Returns true only for a pending transfer copy whose recording, generation, desired placement, or until_encoded retention has made the reservation obsolete.';

CREATE FUNCTION storage_transfer_attempt_count(p_media_asset_id bigint, p_desired_store text)
RETURNS integer
LANGUAGE sql STABLE
AS $$
    SELECT COALESCE(max(attempt.attempt_count), 0)::integer
    FROM media_asset_copies target
    JOIN media_assets asset ON asset.id = target.media_asset_id
    JOIN media_asset_transfer_attempts attempt ON attempt.copy_id = target.id
    WHERE target.media_asset_id = p_media_asset_id
      AND target.store = p_desired_store
      AND target.generation = COALESCE(asset.derived_generation, 0)
      AND target.state <> 'deleted'
$$;

COMMENT ON FUNCTION storage_transfer_attempt_count(bigint, text) IS
    'Returns the failure count for the current-generation destination copy.';

-- This named predicate is one arm of the delete-reconcile predicates: the entry,
-- the pending re-pick, and their negations all reference it. The desired copy is
-- an active, present row in another store, so it is itself the surviving copy and
-- no separate survivor check exists. The caller holds the media_assets row lock
-- before marking a copy deleting, so a policy change cannot remove both copies.
CREATE FUNCTION media_asset_copy_prunable(
    p_copy_id bigint,
    p_original_rules jsonb,
    p_derived_store text,
    p_overlap_cutoff timestamptz
)
RETURNS boolean
LANGUAGE sql STABLE
AS $$
    WITH retiring AS MATERIALIZED (
        SELECT retiring_copy.id, retiring_copy.media_asset_id, retiring_copy.store,
               retiring_copy.state, retiring_copy.generation,
               asset.kind, asset.derived_generation, asset.recording_id,
               recording.site, recording.program_start_at, recording.program_duration_ms,
               recording.deleted_at
        FROM media_asset_copies retiring_copy
        JOIN media_assets asset ON asset.id = retiring_copy.media_asset_id
        JOIN recordings recording ON recording.id = asset.recording_id
        WHERE retiring_copy.id = p_copy_id
    ), placement AS MATERIALIZED (
        SELECT retiring.*,
               desired_media_store(
                   retiring.kind, p_original_rules, p_derived_store, retiring.site,
                   (retiring.program_start_at + retiring.program_duration_ms * interval '1 millisecond')::timestamptz
               ) AS desired_store
        FROM retiring
    )
    SELECT EXISTS (
        SELECT 1
        FROM placement retiring
        WHERE retiring.state IN ('active', 'deleting')
          AND retiring.generation = COALESCE(retiring.derived_generation, 0)
          AND retiring.deleted_at IS NULL
          AND retiring.desired_store IS NOT NULL
          AND retiring.desired_store IS DISTINCT FROM retiring.store
          AND EXISTS (
              SELECT 1
              FROM active_media_asset_copies(retiring.media_asset_id, retiring.desired_store) desired
              WHERE desired.created_at <= p_overlap_cutoff
                AND NOT EXISTS (
                    SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = desired.id
                )
          )
    )
$$;

COMMENT ON FUNCTION media_asset_copy_prunable(bigint, jsonb, text, timestamptz) IS
    'Returns true only when a present active copy in the desired store was created at or before the overlap cutoff.';

-- The existing until_encoded function already accepts the desired derived store.
-- Add physical presence to that same decision: an active row with a missing-file
-- observation is not evidence that the derived output is safe to retain alone.
CREATE OR REPLACE FUNCTION until_encoded_deletable_originals(desired_store text)
RETURNS TABLE (asset_id bigint, recording_id bigint)
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
                SELECT 1 FROM active_media_asset_copies(e.id, desired_store) copy
                WHERE NOT EXISTS (
                    SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = copy.id
                )
            )
      )
  )
  AND EXISTS (
      SELECT 1
      FROM media_assets thumbnail
      WHERE thumbnail.recording_id = r.id
        AND thumbnail.kind = 'thumbnail'
        AND EXISTS (
            SELECT 1 FROM active_media_asset_copies(thumbnail.id, desired_store) copy
            WHERE NOT EXISTS (
                SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = copy.id
            )
        )
  )
  AND EXISTS (
      SELECT 1
      FROM media_assets seek_tiles
      WHERE seek_tiles.recording_id = r.id
        AND seek_tiles.kind = 'seek_tiles'
        AND EXISTS (
            SELECT 1 FROM active_media_asset_copies(seek_tiles.id, desired_store) copy
            WHERE NOT EXISTS (
                SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = copy.id
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
    'until_encoded_deletable_originals は desired_store の active・実体ありの派生コピーが揃い、保持条件を満たす原本を返す。';

-- +goose Down

DROP FUNCTION media_asset_copy_prunable(bigint, jsonb, text, timestamptz);
DROP FUNCTION storage_transfer_pending_copy_obsolete(bigint, jsonb, text[], text);
DROP FUNCTION storage_transfer_attempt_count(bigint, text);
DROP FUNCTION storage_transfer_candidate(bigint, jsonb, text[], text);
DROP TABLE media_asset_transfer_attempts;

CREATE OR REPLACE FUNCTION until_encoded_deletable_originals(desired_store text)
RETURNS TABLE (asset_id bigint, recording_id bigint)
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
  AND EXISTS (SELECT 1 FROM current_ts_scanned_originals s WHERE s.media_asset_id = a.id)
  AND NOT EXISTS (SELECT 1 FROM unencoded_desired_profiles u WHERE u.recording_id = r.id)
  AND NOT EXISTS (
      SELECT 1 FROM unnest(p.encode_profiles) AS want(profile)
      WHERE NOT EXISTS (
          SELECT 1 FROM media_assets e
          WHERE e.recording_id = r.id AND e.kind = 'encoded' AND e.profile = want.profile
            AND EXISTS (SELECT 1 FROM active_media_asset_copies(e.id, desired_store))
      )
  )
  AND EXISTS (
      SELECT 1 FROM media_assets thumbnail
      WHERE thumbnail.recording_id = r.id AND thumbnail.kind = 'thumbnail'
        AND EXISTS (SELECT 1 FROM active_media_asset_copies(thumbnail.id, desired_store))
  )
  AND EXISTS (
      SELECT 1 FROM media_assets seek_tiles
      WHERE seek_tiles.recording_id = r.id AND seek_tiles.kind = 'seek_tiles'
        AND EXISTS (SELECT 1 FROM active_media_asset_copies(seek_tiles.id, desired_store))
  )
  AND (
      NOT p.cm_detect
      OR EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
      OR EXISTS (
          SELECT 1 FROM current_cm_attempts ca
          WHERE ca.recording_id = r.id AND ca.state = 'failed' AND ca.stage IS DISTINCT FROM 'adopt'
      )
  )
$$;
