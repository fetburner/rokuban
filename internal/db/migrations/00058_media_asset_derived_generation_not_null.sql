-- +goose Up

-- 原本コピーと派生物の初期世代はいずれも 0 なので、全行を NOT NULL / DEFAULT 0 にし、
-- 原本を CHECK で 0 に固定する。原本だけ NULL を残すと現行コピーの述語に COALESCE が残り、
-- 1 箇所でも書き忘れた読み手から NULL の原本コピーが見えなくなるため、全行を 0 に埋める。
-- DEFAULT 0 が旧バイナリを救うのは、列を省略する INSERT（ingest の原本取り込み）だけ。
-- 旧 UpsertInPlaceMediaAsset は rokuban rescue の媒体走査と rokuban import epgstation --library-json
-- から、旧 CatalogUpsertMediaAsset は rokuban rescue の catalog 復元から呼ばれ、原本に NULL を明示する。
-- 旧 CreateMediaAsset も原本に NULL を明示する SQL だが、現ツリーの非テスト呼び出し元は
-- 手動実行の streamer conformance utility で、encoded を登録する。これらは自動ループからは呼ばれない。
-- 入れ替え中に旧バイナリで rescue / import を実行すると NOT NULL 違反になり、新バイナリで再実行すれば通る。
ALTER TABLE media_assets
    DROP CONSTRAINT media_assets_derived_generation_check;

UPDATE media_assets
SET derived_generation = 0
WHERE derived_generation IS NULL;

ALTER TABLE media_assets
    ALTER COLUMN derived_generation SET DEFAULT 0,
    ALTER COLUMN derived_generation SET NOT NULL,
    ADD CONSTRAINT media_assets_derived_generation_check
        CHECK ((kind <> 'original' OR derived_generation = 0) AND derived_generation >= 0);

COMMENT ON COLUMN media_assets.derived_generation IS
    'Monotonic generation for derived output paths; originals are fixed at generation zero';

CREATE OR REPLACE FUNCTION active_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT copy.*
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.media_asset_id = p_asset_id
  AND copy.state = 'active'
  AND copy.generation = asset.derived_generation
  AND (p_store IS NULL OR copy.store = p_store)
$$;

COMMENT ON FUNCTION active_media_asset_copies(bigint, text) IS
    'active_media_asset_copies は、論理資産の derived_generation と一致する active コピーを返す。原本の世代は 0。p_store が NULL なら全置き場を対象にする。';

CREATE OR REPLACE VIEW old_generation_media_asset_copies AS
SELECT old_copy.*,
       (SELECT min(current_copy.created_at)
        FROM media_asset_copies current_copy
        WHERE current_copy.media_asset_id = old_copy.media_asset_id
          AND current_copy.generation = asset.derived_generation) AS generation_started_at
FROM media_asset_copies old_copy
JOIN media_assets asset ON asset.id = old_copy.media_asset_id
WHERE asset.kind <> 'original'
  AND old_copy.generation < asset.derived_generation
  AND EXISTS (
      SELECT 1
      FROM active_present_media_asset_copies(old_copy.media_asset_id, NULL) current_copy
  );

COMMENT ON VIEW old_generation_media_asset_copies IS
    'old_generation_media_asset_copies は、論理資産の derived_generation より古い派生コピーのうち、実体無しでない現行の active コピーがどこかの置き場にあるものを返す。generation_started_at は現行世代の最初のコピー行の created_at で、呼び出し側が overlap の猶予を判定する。';

CREATE OR REPLACE FUNCTION media_asset_copy_prunable(
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
          AND retiring.generation = retiring.derived_generation
          AND retiring.deleted_at IS NULL
          AND retiring.desired_store IS NOT NULL
          AND retiring.desired_store IS DISTINCT FROM retiring.store
          AND EXISTS (
              SELECT 1
              FROM active_present_media_asset_copies(retiring.media_asset_id, retiring.desired_store) desired
              WHERE desired.created_at <= p_overlap_cutoff
          )
    )
$$;

CREATE OR REPLACE FUNCTION storage_transfer_attempt_count(p_media_asset_id bigint, p_desired_store text)
RETURNS integer
LANGUAGE sql STABLE
AS $$
    SELECT COALESCE(max(attempt.attempt_count), 0)::integer
    FROM media_asset_copies target
    JOIN media_assets asset ON asset.id = target.media_asset_id
    JOIN media_asset_transfer_attempts attempt ON attempt.copy_id = target.id
    WHERE target.media_asset_id = p_media_asset_id
      AND target.store = p_desired_store
      AND target.generation = asset.derived_generation
      AND target.state <> 'deleted'
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
          SELECT storage_transfer_destination(
              asset.kind, p_original_rules, p_derived_store, p_fs_stores, recording.site,
              (recording.program_start_at + recording.program_duration_ms * interval '1 millisecond')::timestamptz,
              recording.deleted_at
          ) AS desired_store
      ) placement
      WHERE copy.id = p_copy_id
        AND copy.state = 'pending'
        AND (
            copy.generation <> asset.derived_generation
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

CREATE OR REPLACE FUNCTION active_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT copy.*
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.media_asset_id = p_asset_id
  AND copy.state = 'active'
  AND copy.generation = COALESCE(asset.derived_generation, 0)
  AND (p_store IS NULL OR copy.store = p_store)
$$;

COMMENT ON FUNCTION active_media_asset_copies(bigint, text) IS
    'active_media_asset_copies は、論理資産の derived_generation と一致する active コピーを返す。original の NULL 世代は 0 として扱う。p_store が NULL なら全置き場を対象にする。';

CREATE OR REPLACE VIEW old_generation_media_asset_copies AS
SELECT old_copy.id,
       old_copy.media_asset_id,
       old_copy.store,
       old_copy.rel_path,
       old_copy.state,
       old_copy.created_at,
       old_copy.deleted_at,
       old_copy.generation,
       (SELECT min(current_copy.created_at)
        FROM media_asset_copies current_copy
        WHERE current_copy.media_asset_id = old_copy.media_asset_id
          AND current_copy.generation = COALESCE(asset.derived_generation, 0)) AS generation_started_at
FROM media_asset_copies old_copy
JOIN media_assets asset ON asset.id = old_copy.media_asset_id
WHERE asset.kind <> 'original'
  AND old_copy.generation < COALESCE(asset.derived_generation, 0)
  AND EXISTS (SELECT 1 FROM active_present_media_asset_copies(old_copy.media_asset_id, NULL::text));

COMMENT ON VIEW old_generation_media_asset_copies IS
    'old_generation_media_asset_copies は、論理資産の derived_generation より古い派生コピーのうち、実体無しでない現行の active コピーがどこかの置き場にあるものを返す。generation_started_at は現行世代の最初のコピー行の created_at で、呼び出し側が overlap の猶予を判定する。';

CREATE OR REPLACE FUNCTION media_asset_copy_prunable(
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

CREATE OR REPLACE FUNCTION storage_transfer_attempt_count(p_media_asset_id bigint, p_desired_store text)
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

ALTER TABLE media_assets
    DROP CONSTRAINT media_assets_derived_generation_check,
    ALTER COLUMN derived_generation DROP NOT NULL,
    ALTER COLUMN derived_generation DROP DEFAULT;

UPDATE media_assets SET derived_generation = NULL WHERE kind = 'original';

ALTER TABLE media_assets
    ADD CONSTRAINT media_assets_derived_generation_check
        CHECK (derived_generation IS NULL OR (kind <> 'original' AND derived_generation >= 0));

COMMENT ON COLUMN media_assets.derived_generation IS
    'Monotonic generation for derived output paths; NULL for original assets';
