-- +goose Up

-- The destination of a transfer is the FS store that the desired placement names,
-- or NULL when the recording is deleted or the placement is not an FS store.
-- The backlog prefilter and the candidate predicate both call this function, so
-- the three conditions are written once.
CREATE FUNCTION storage_transfer_destination(
    asset_kind text,
    original_rules jsonb,
    derived_store text,
    fs_stores text[],
    recording_site text,
    program_end_at timestamptz,
    recording_deleted_at timestamptz
)
RETURNS text
LANGUAGE sql STABLE
AS $$
    SELECT CASE
        WHEN recording_deleted_at IS NULL
         AND desired_media_store(asset_kind, original_rules, derived_store, recording_site, program_end_at) = ANY(fs_stores)
        THEN desired_media_store(asset_kind, original_rules, derived_store, recording_site, program_end_at)
    END
$$;

COMMENT ON FUNCTION storage_transfer_destination(text, jsonb, text, text[], text, timestamptz, timestamptz) IS
    'Returns the FS store a transfer should write to, or NULL when the recording is deleted or the desired placement is not an FS store.';

CREATE OR REPLACE FUNCTION storage_transfer_candidate(
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
      -- OFFSET 0 は副問い合わせを展開させない囲い。これがないと宛先の式が参照のたびに
      -- 展開され、規則の評価が行ごとに何度も走る（20k 行の計測で約 2 倍遅くなった）。
      CROSS JOIN LATERAL (
          SELECT storage_transfer_destination(
              a.kind, p_original_rules, p_derived_store, p_fs_stores, r.site,
              (r.program_start_at + r.program_duration_ms * interval '1 millisecond')::timestamptz,
              r.deleted_at
          ) AS desired_store
          OFFSET 0
      ) placement
      WHERE a.id = p_media_asset_id
      AND placement.desired_store IS NOT NULL
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

-- +goose Down

CREATE OR REPLACE FUNCTION storage_transfer_candidate(
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

DROP FUNCTION storage_transfer_destination(text, jsonb, text, text[], text, timestamptz, timestamptz);
