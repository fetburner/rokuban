-- +goose Up

-- desired_media_store は資産の desired 置き場を、DB の判断時点で返す。
-- original_rules は config.StorageConfig.OriginalRulesJSON が出す正規化済みの JSON。
-- 規則は Go で再評価しない。older_than の比較は録画の program_end_at と now() を使い、
-- 同じ SQL 文の中で適用まで行う。
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
              AND (entry.rule->'match'->>'older_than_us' IS NULL
                   OR (
                       program_end_at IS NOT NULL
                       -- 経過時間は interval の time 成分（マイクロ秒）で表す。days 成分は DST をまたぐと暦の日数になる。
                       -- TestDesiredMediaStore の DST のケースがこれを固定している。
                       AND program_end_at < now()
                           - (entry.rule->'match'->>'older_than_us')::bigint * interval '1 microsecond'
                   ))
            ORDER BY entry.ordinal
            LIMIT 1
        )
    END
$$;

COMMENT ON FUNCTION desired_media_store(text, jsonb, text, text, timestamptz) IS
    'desired_media_store は original の規則を先勝ちで評価し、その他の資産には派生物の置き場を返す。older_than_us は固定の経過時間（マイクロ秒）として、放送終了と now() を厳密に比較する。';

-- +goose Down

DROP FUNCTION desired_media_store(text, jsonb, text, text, timestamptz);
