-- +goose Up

-- desired_media_store は資産の desired 置き場を DB の判断時点で返す。
-- original_rules は config.StorageConfig.OriginalRulesJSON が出す正規化済み JSON。
-- 規則を Go で再評価しない。録画の program_end_at と now() を使うため、
-- older_than の比較と適用は同じ SQL 文の中で行われる。
CREATE FUNCTION desired_media_store(
    asset_kind text,
    original_rules jsonb,
    derived_store text,
    recording_site text,
    program_end_at timestamptz
)
RETURNS text
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE
        WHEN asset_kind IS NULL THEN NULL
        WHEN asset_kind <> 'original' THEN derived_store
        ELSE (
            SELECT entry.rule->>'store'
            FROM jsonb_array_elements(COALESCE(original_rules, '[]'::jsonb))
                 WITH ORDINALITY AS entry(rule, ordinal)
            WHERE (entry.rule->'match'->>'site' IS NULL
                   OR entry.rule->'match'->>'site' = recording_site)
              AND (entry.rule->'match'->>'older_than_ns' IS NULL
                   OR (
                       program_end_at IS NOT NULL
                       -- Put the duration in interval's time component: its days component is calendar-based across DST.
                       -- TestDesiredMediaStore/older_than_is_fixed_elapsed_time_across_daylight_saving_changes covers this.
                       AND program_end_at < now() - (
                           make_interval(
                               secs => (
                                   (entry.rule->'match'->>'older_than_ns')::bigint
                                   / 1000000000
                               )::double precision
                           ) + make_interval(
                               secs => (
                                   ((entry.rule->'match'->>'older_than_ns')::bigint
                                    % 1000000000)::double precision
                                   / 1000000000::double precision
                               )
                           )
                       )
                   ))
            ORDER BY entry.ordinal
            LIMIT 1
        )
    END
$$;

COMMENT ON FUNCTION desired_media_store(text, jsonb, text, text, timestamptz) IS
    'desired_media_store は original の規則を先勝ちで評価し、その他の資産には派生物の置き場を返す。older_than_ns は固定の経過時間として放送終了と now() を厳密に比較する。';

-- +goose Down

DROP FUNCTION desired_media_store(text, jsonb, text, text, timestamptz);
