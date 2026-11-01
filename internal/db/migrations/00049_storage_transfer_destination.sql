-- +goose Up

-- storage_transfer_destination は転送の宛先を返す。録画が削除済みでなく、desired 置き場が
-- FS 置き場ならその置き場、それ以外は NULL を返す。
-- 転送の宛先（削除済みでなく、desired 置き場が FS 置き場であること）を判定する述語は、この関数を通す。
-- PrepareMediaAssetTransfer / CompleteMediaAssetTransfer は、適用の瞬間に desired_media_store を
-- 呼び出し元が渡した target_store と照合しており、この関数を通らない。
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
      -- OFFSET 0 は最適化のヒントにすぎず、無くても結果は変わらない。
      -- 2 万行の計測（集計の中央値）では、なしが 2,181 ms、ありが 1,044 ms。
      -- ヒントがないと、宛先の式が本体で参照されるたびに規則の評価が走る。
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

-- 予約（pending の copy）が古くなったかを判定する。宛先が NULL（削除済み・FS 外）か
-- 予約と別の置き場なら、IS DISTINCT FROM が真になる。copy.store は NOT NULL なので、
-- 宛先が NULL の場合も真になる。
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
          SELECT storage_transfer_destination(
              asset.kind, p_original_rules, p_derived_store, p_fs_stores, recording.site,
              (recording.program_start_at + recording.program_duration_ms * interval '1 millisecond')::timestamptz,
              recording.deleted_at
          ) AS desired_store
      ) placement
      WHERE copy.id = p_copy_id
        AND copy.state = 'pending'
        AND (
            copy.generation <> COALESCE(asset.derived_generation, 0)
            OR placement.desired_store IS DISTINCT FROM copy.store
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
