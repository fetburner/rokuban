-- +goose Up

-- 「現行世代で active、かつ実体無しでない」コピーの唯一の定義。実体無しの観測
-- （missing_media_assets）を除く読み手はこの関数を読む。観測を除かない読み手は
-- active_media_asset_copies を直接読み、理由を定義の COMMENT に書く。
CREATE FUNCTION active_present_media_asset_copies(p_asset_id bigint, p_store text)
RETURNS SETOF media_asset_copies
LANGUAGE sql STABLE
AS $$
SELECT c.*
FROM active_media_asset_copies(p_asset_id, p_store) c
WHERE NOT EXISTS (
    SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = c.id
)
$$;

COMMENT ON FUNCTION active_present_media_asset_copies(bigint, text) IS
    'active_present_media_asset_copies は、active_media_asset_copies のうち実体無しの観測（missing_media_assets）が無いコピーを返す。p_store が NULL なら全置き場を対象にする。実体無しを除く読み手はこの関数だけを読む。';

-- 依存の順: この関数（上）→ until_encoded_deletable_originals → storage_transfer_candidate。
-- media_asset_copy_prunable と各 view は関数に依らない。

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

COMMENT ON FUNCTION until_encoded_deletable_originals(text) IS
    'until_encoded_deletable_originals は desired_store の active・実体ありの派生コピーが揃い、保持条件を満たす原本を返す。派生コピーの実体有無は active_present_media_asset_copies で判定する。原本自身の実体有無は見ない（実体無しの原本の削除は ErrNotExist から削除済みの確定まで進む）。';

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
          SELECT 1 FROM active_present_media_asset_copies(a.id, placement.desired_store) target
      )
      AND EXISTS (
          SELECT 1 FROM active_present_media_asset_copies(a.id, NULL) source
          WHERE source.store = ANY(p_fs_stores)
            AND source.store <> placement.desired_store
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
              FROM active_present_media_asset_copies(retiring.media_asset_id, retiring.desired_store) desired
              WHERE desired.created_at <= p_overlap_cutoff
          )
    )
$$;

CREATE OR REPLACE VIEW cm_detection_desired AS
SELECT r.id AS recording_id
FROM recordings r
JOIN recording_encode_policy p ON p.recording_id = r.id AND p.cm_detect
JOIN media_assets o ON o.recording_id = r.id AND o.kind = 'original'
LEFT JOIN current_cm_attempts ca ON ca.recording_id = r.id
LEFT JOIN cm_logos l ON l.network_id = r.network_id AND l.service_id = r.service_id
WHERE r.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM active_present_media_asset_copies(o.id, NULL::text))
  AND NOT EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
  AND (ca.recording_id IS NULL OR ca.state <> 'failed'
       OR (l.learned_at IS NOT NULL AND ca.attempted_at < l.learned_at));

COMMENT ON VIEW cm_detection_desired IS
    'cm_detection_desired は、cm_detect が有効で、CM 検出が未了かつ再試行の対象になる録画を表す。原本は実体の有る現行のコピーに限る（実体無しの原本は CM 検出の入力として読めないと観測されたものなので候補にしない）。';

CREATE OR REPLACE VIEW cm_logo_candidate_desired AS
SELECT area.network_id,
       area.service_id,
       area.updated_at AS area_updated_at,
       (SELECT r2.id
        FROM recordings r2
        JOIN media_assets original ON original.recording_id = r2.id AND original.kind = 'original'
        WHERE r2.network_id = area.network_id
          AND r2.service_id = area.service_id
          AND r2.deleted_at IS NULL
          AND r2.purged_at IS NULL
          AND EXISTS (SELECT 1 FROM active_present_media_asset_copies(original.id, NULL::text))
        ORDER BY r2.id DESC
        LIMIT 1) AS recording_id
FROM cm_logo_areas area
LEFT JOIN cm_logo_candidates candidate
  ON candidate.network_id = area.network_id AND candidate.service_id = area.service_id
LEFT JOIN cm_logos logo
  ON logo.network_id = area.network_id AND logo.service_id = area.service_id
WHERE candidate.network_id IS NULL
  AND (logo.network_id IS NULL OR logo.learned_at < area.updated_at)
  AND EXISTS (
      SELECT 1
      FROM recordings r
      JOIN media_assets original ON original.recording_id = r.id AND original.kind = 'original'
      WHERE r.network_id = area.network_id
        AND r.service_id = area.service_id
        AND r.deleted_at IS NULL
        AND r.purged_at IS NULL
        AND EXISTS (SELECT 1 FROM active_present_media_asset_copies(original.id, NULL::text))
  );

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

-- 実体無しを除かない読み手（観測を読まない）の理由は各 COMMENT に書く。
-- 実体無しは「直前の走査で観測できなかった」という主張で、自動の削除・作り直しの根拠にしない。

COMMENT ON VIEW encodable_recordings IS
    'encodable_recordings は、ごみ箱になく active な原本コピーがある録画を表す。encode の不足の補完・待機列・作り直し要求の列挙が共有する。実体無しの原本も含める。単発の走査揺れで encode の入力選択を止めないため。encode の失敗は試行行に記録される。';

COMMENT ON VIEW unencoded_desired_profiles IS
    'unencoded_desired_profiles は、凍結済みの desired のうち current active encoded コピーが無い (recording_id, profile) を表す。現在の設定では絞らない。実体無しの encoded も「有る」と数える（実体無しを作り直しの根拠にしない）。encode の不足の補完・待機列と、until_encoded の削除判定が共有する。';

COMMENT ON VIEW unsatisfied_encode_rebuilds IS
    'unsatisfied_encode_rebuilds は、requested_keep と一致する凍結区間の active な encoded がまだ無い作り直し要求を表す。ごみ箱・原本が active でない録画は含まない。実体無しの encoded は「有る」と数える（unencoded_desired_profiles と同じ理由）。';

COMMENT ON VIEW removed_encoded_assets IS
    'removed_encoded_assets は、ユーザーが外した encoded 版のうち、他の版が active で残るものを表す。実体無しを除かない。削除側は unlink の ErrNotExist から削除済みの確定まで進むので、除くと実体無しの行が残り続ける。他の版の判定も実体無しを除かないため、実体無しの原本も「残る版」として数える（この判定を実体で見るべきかは未決）。';

-- +goose Down

COMMENT ON VIEW removed_encoded_assets IS NULL;

COMMENT ON VIEW unsatisfied_encode_rebuilds IS
    'unsatisfied_encode_rebuilds は、requested_keep と一致する凍結区間の active な encoded がまだ無い作り直し要求を表す。ごみ箱・原本が active でない録画は含まない。';

COMMENT ON VIEW unencoded_desired_profiles IS
    'unencoded_desired_profiles は、凍結済みの desired のうち current active encoded コピーが無い (recording_id, profile) を表す。現在の設定では絞らない。encode の不足の補完・待機列と、until_encoded の削除判定が共有する。';

COMMENT ON VIEW encodable_recordings IS
    'encodable_recordings は、ごみ箱になく active な原本コピーがある録画を表す。encode の不足の補完・待機列・作り直し要求の列挙が共有する。';

COMMENT ON VIEW cm_detection_desired IS NULL;


CREATE OR REPLACE VIEW cm_detection_desired AS
 SELECT r.id AS recording_id
   FROM recordings r
     JOIN recording_encode_policy p ON p.recording_id = r.id AND p.cm_detect
     JOIN media_assets original ON original.recording_id = r.id AND original.kind = 'original'::text
     LEFT JOIN current_cm_attempts attempt ON attempt.recording_id = r.id
     LEFT JOIN cm_logos logo ON logo.network_id = r.network_id AND logo.service_id = r.service_id
  WHERE r.deleted_at IS NULL AND (EXISTS ( SELECT 1
           FROM media_asset_copies copy
          WHERE copy.media_asset_id = original.id AND copy.state = 'active'::text AND NOT (EXISTS ( SELECT 1
                   FROM missing_media_assets missing
                  WHERE missing.media_asset_copy_id = copy.id)))) AND NOT (EXISTS ( SELECT 1
           FROM recording_cm_detections d
          WHERE d.recording_id = r.id)) AND (attempt.recording_id IS NULL OR attempt.state <> 'failed'::text OR logo.learned_at IS NOT NULL AND attempt.attempted_at < logo.learned_at);

CREATE OR REPLACE VIEW cm_logo_candidate_desired AS
 SELECT area.network_id,
    area.service_id,
    area.updated_at AS area_updated_at,
    ( SELECT r2.id
           FROM recordings r2
             JOIN media_assets original ON original.recording_id = r2.id AND original.kind = 'original'::text
          WHERE r2.network_id = area.network_id AND r2.service_id = area.service_id AND r2.deleted_at IS NULL AND r2.purged_at IS NULL AND (EXISTS ( SELECT 1
                   FROM media_asset_copies copy
                  WHERE copy.media_asset_id = original.id AND copy.state = 'active'::text AND NOT (EXISTS ( SELECT 1
                           FROM missing_media_assets missing
                          WHERE missing.media_asset_copy_id = copy.id))))
          ORDER BY r2.id DESC
         LIMIT 1) AS recording_id
   FROM cm_logo_areas area
     LEFT JOIN cm_logo_candidates candidate ON candidate.network_id = area.network_id AND candidate.service_id = area.service_id
     LEFT JOIN cm_logos logo ON logo.network_id = area.network_id AND logo.service_id = area.service_id
  WHERE candidate.network_id IS NULL AND (logo.network_id IS NULL OR logo.learned_at < area.updated_at) AND (EXISTS ( SELECT 1
           FROM recordings r
             JOIN media_assets original ON original.recording_id = r.id AND original.kind = 'original'::text
          WHERE r.network_id = area.network_id AND r.service_id = area.service_id AND r.deleted_at IS NULL AND r.purged_at IS NULL AND (EXISTS ( SELECT 1
                   FROM media_asset_copies copy
                  WHERE copy.media_asset_id = original.id AND copy.state = 'active'::text AND NOT (EXISTS ( SELECT 1
                           FROM missing_media_assets missing
                          WHERE missing.media_asset_copy_id = copy.id))))));

CREATE OR REPLACE VIEW old_generation_media_asset_copies AS
 SELECT old_copy.id,
    old_copy.media_asset_id,
    old_copy.store,
    old_copy.rel_path,
    old_copy.state,
    old_copy.created_at,
    old_copy.deleted_at,
    old_copy.generation,
    ( SELECT min(current_copy.created_at) AS min
           FROM media_asset_copies current_copy
          WHERE current_copy.media_asset_id = old_copy.media_asset_id AND current_copy.generation = COALESCE(asset.derived_generation, 0::bigint)) AS generation_started_at
   FROM media_asset_copies old_copy
     JOIN media_assets asset ON asset.id = old_copy.media_asset_id
  WHERE asset.kind <> 'original'::text AND old_copy.generation < COALESCE(asset.derived_generation, 0::bigint) AND (EXISTS ( SELECT 1
           FROM active_media_asset_copies(old_copy.media_asset_id, NULL::text) current_copy(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)
          WHERE NOT (EXISTS ( SELECT 1
                   FROM missing_media_assets missing
                  WHERE missing.media_asset_copy_id = current_copy.id))));

CREATE OR REPLACE FUNCTION public.media_asset_copy_prunable(p_copy_id bigint, p_original_rules jsonb, p_derived_store text, p_overlap_cutoff timestamp with time zone)
 RETURNS boolean
 LANGUAGE sql
 STABLE
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

CREATE OR REPLACE FUNCTION public.storage_transfer_candidate(p_media_asset_id bigint, p_original_rules jsonb, p_fs_stores text[], p_derived_store text)
 RETURNS boolean
 LANGUAGE sql
 STABLE
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

CREATE OR REPLACE FUNCTION public.until_encoded_deletable_originals(desired_store text)
 RETURNS TABLE(asset_id bigint, recording_id bigint)
 LANGUAGE sql
 STABLE
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

DROP FUNCTION active_present_media_asset_copies(bigint, text);
