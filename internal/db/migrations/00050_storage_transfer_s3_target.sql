-- +goose Up

-- 派生物の置き場が S3 のとき、その置き場も転送の宛先になる。p_fs_stores は転送元の FS 置き場の集合のままで、
-- 宛先の判定だけが p_derived_store を足す。S3 を p_fs_stores に入れると、古い派生物の S3 コピーを転送元に数えてしまう。
-- 原本の宛先は FS だけ（原本の規則は FS の置き場しか指せない）。
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
      AND (
          placement.desired_store = ANY(p_fs_stores)
          OR (a.kind <> 'original' AND placement.desired_store = p_derived_store)
      )
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

CREATE OR REPLACE FUNCTION storage_transfer_pending_copy_obsolete(
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
            OR NOT (
                placement.desired_store = ANY(p_fs_stores)
                OR (asset.kind <> 'original' AND placement.desired_store = p_derived_store)
            )
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

CREATE OR REPLACE FUNCTION storage_transfer_pending_copy_obsolete(
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
