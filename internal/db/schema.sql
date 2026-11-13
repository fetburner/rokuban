--
-- PostgreSQL database dump
--

--
-- Name: btree_gist; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;

--
-- Name: EXTENSION btree_gist; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION btree_gist IS 'support for indexing common datatypes in GiST';

--
-- Name: pg_trgm; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;

--
-- Name: EXTENSION pg_trgm; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';

--
-- Name: media_asset_copies; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_asset_copies (
    id bigint NOT NULL,
    media_asset_id bigint NOT NULL,
    store text NOT NULL,
    rel_path text NOT NULL,
    state text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    generation bigint NOT NULL,
    CONSTRAINT media_asset_copies_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'active'::text, 'deleting'::text, 'deleted'::text]))),
    CONSTRAINT media_asset_copies_store_check CHECK ((store ~ '^[a-z0-9-]+$'::text))
);

--
-- Name: TABLE media_asset_copies; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.media_asset_copies IS 'Physical copies of logical media assets; an asset without a copy row has not been assigned by storage adopt';

--
-- Name: COLUMN media_asset_copies.generation; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.media_asset_copies.generation IS 'Content generation stored in this physical copy; immutable after insert';

--
-- Name: active_media_asset_copies(bigint, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.active_media_asset_copies(p_asset_id bigint, p_store text) RETURNS SETOF public.media_asset_copies
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

--
-- Name: FUNCTION active_media_asset_copies(p_asset_id bigint, p_store text); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.active_media_asset_copies(p_asset_id bigint, p_store text) IS 'active_media_asset_copies は、論理資産の derived_generation と一致する active コピーを返す。original の NULL 世代は 0 として扱う。p_store が NULL なら全置き場を対象にする。';

--
-- Name: active_present_media_asset_copies(bigint, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.active_present_media_asset_copies(p_asset_id bigint, p_store text) RETURNS SETOF public.media_asset_copies
    LANGUAGE sql STABLE
    AS $$
SELECT c.*
FROM active_media_asset_copies(p_asset_id, p_store) c
WHERE NOT EXISTS (
    SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = c.id
)
$$;

--
-- Name: FUNCTION active_present_media_asset_copies(p_asset_id bigint, p_store text); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.active_present_media_asset_copies(p_asset_id bigint, p_store text) IS 'active_present_media_asset_copies は、active_media_asset_copies のうち実体無しの観測（missing_media_assets）が無いコピーを返す。p_store が NULL なら全置き場を対象にする。実体無しを除く読み手はこの関数だけを読む。';

--
-- Name: array_is_canonical_set(text[]); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.array_is_canonical_set(a text[]) RETURNS boolean
    LANGUAGE sql IMMUTABLE STRICT
    AS $$
  SELECT array_position(a, NULL) IS NULL
     AND NOT ('' = ANY(a))
     AND a = (SELECT coalesce(array_agg(DISTINCT x ORDER BY x), '{}') FROM unnest(a) x)
$$;

--
-- Name: awaiting_review_profiles(text[]); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.awaiting_review_profiles(p_cut_profiles text[]) RETURNS TABLE(recording_id bigint, profile text)
    LANGUAGE sql STABLE
    AS $$
    SELECT u.recording_id, u.profile
    FROM unencoded_desired_profiles u
    JOIN encodable_recordings er ON er.recording_id = u.recording_id
    WHERE u.profile = ANY(p_cut_profiles)
      AND NOT EXISTS (
          SELECT 1 FROM recording_chapter_ownership o
          WHERE o.recording_id = u.recording_id
      )
      AND NOT EXISTS (
          SELECT 1 FROM recording_encode_attempts a
          WHERE a.recording_id = u.recording_id
            AND a.profile = u.profile
      )
      AND NOT EXISTS (
          SELECT 1 FROM unsatisfied_encode_rebuilds q
          WHERE q.recording_id = u.recording_id
            AND q.profile = u.profile
      )
$$;

--
-- Name: FUNCTION awaiting_review_profiles(p_cut_profiles text[]); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.awaiting_review_profiles(p_cut_profiles text[]) IS 'awaiting_review_profiles は、現在の cut 設定にある凍結済みプロファイルのうち、原本が active で章が未確認、active な encoded・試行行・未充足の作り直し要求が無い (recording_id, profile) を返す。';

--
-- Name: desired_media_store(text, jsonb, text, text, timestamp with time zone); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.desired_media_store(asset_kind text, original_rules jsonb, derived_store text, recording_site text, program_end_at timestamp with time zone) RETURNS text
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

--
-- Name: FUNCTION desired_media_store(asset_kind text, original_rules jsonb, derived_store text, recording_site text, program_end_at timestamp with time zone); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.desired_media_store(asset_kind text, original_rules jsonb, derived_store text, recording_site text, program_end_at timestamp with time zone) IS 'desired_media_store は original の規則を先勝ちで評価し、その他の資産には派生物の置き場を返す。older_than_us は固定の経過時間（マイクロ秒）として、放送終了と now() を厳密に比較する。';

--
-- Name: encodable_profile(bigint, text, text[], text[]); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.encodable_profile(rec_id bigint, profile_name text, known_profiles text[], cut_profiles text[]) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT profile_name <> ''
       AND profile_name = ANY(known_profiles)
       AND (
           NOT (profile_name = ANY(cut_profiles))
           OR EXISTS (
               SELECT 1 FROM recording_chapter_ownership o
               WHERE o.recording_id = rec_id
           )
       )
$$;

--
-- Name: FUNCTION encodable_profile(rec_id bigint, profile_name text, known_profiles text[], cut_profiles text[]); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.encodable_profile(rec_id bigint, profile_name text, known_profiles text[], cut_profiles text[]) IS 'encodable_profile は、録画 rec_id のプロファイル profile_name が現在の設定で作れるかを返す。cut プロファイルはチャプター確認済みを要求する。ただし作り直し要求（押した時点の keep が確認）には使わない。known_profiles に NULL を渡すと NULL を返す。';

--
-- Name: encode_attempts_exhausted(bigint, text, integer); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.encode_attempts_exhausted(rec_id bigint, profile_name text, attempt_limit integer) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT EXISTS (
        SELECT 1
        FROM recording_encode_attempts a
        WHERE a.recording_id = rec_id
          AND a.profile = profile_name
          AND a.state = 'failed'
          AND GREATEST(a.attempt_count, 1) >= attempt_limit
    )
$$;

--
-- Name: FUNCTION encode_attempts_exhausted(rec_id bigint, profile_name text, attempt_limit integer); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.encode_attempts_exhausted(rec_id bigint, profile_name text, attempt_limit integer) IS 'encode_attempts_exhausted は (recording_id, profile) の failed 試行が attempt_limit に達したかを返す。recording_encode_attempts.attempt_count が 0 の旧行は 1 回と数え、running 行は除外しない。';

--
-- Name: genre_lv1_of(jsonb); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.genre_lv1_of(g jsonb) RETURNS smallint[]
    LANGUAGE sql IMMUTABLE
    AS $_$
  SELECT coalesce(
    (SELECT array_agg(DISTINCT (e->>'lv1')::smallint ORDER BY (e->>'lv1')::smallint)
     FROM jsonb_array_elements(
       CASE WHEN jsonb_typeof(g) = 'array' THEN g ELSE '[]'::jsonb END
     ) e
     WHERE (e->>'lv1') ~ '^[0-9]+$'
       AND (e->>'lv1')::numeric BETWEEN 0 AND 15),
    '{}')::smallint[]
$_$;

--
-- Name: label_rule_winner(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.label_rule_winner(title text) RETURNS bigint
    LANGUAGE sql STABLE
    AS $$
  WITH norm AS MATERIALIZED (
    SELECT public.normalize_search_text(title) AS t
  )
  SELECT lr.id
  FROM public.label_rules lr, norm n
  WHERE n.t LIKE ('%' || lr.keyword_key || '%') ESCAPE '\'
  ORDER BY lr.priority DESC, lr.id ASC
  LIMIT 1
$$;

--
-- Name: like_escape(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.like_escape(s text) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT
    AS $$
  SELECT replace(replace(replace(s, '\', '\\'), '%', '\%'), '_', '\_')
$$;

--
-- Name: media_asset_copy_prunable(bigint, jsonb, text, timestamp with time zone); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.media_asset_copy_prunable(p_copy_id bigint, p_original_rules jsonb, p_derived_store text, p_overlap_cutoff timestamp with time zone) RETURNS boolean
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

--
-- Name: FUNCTION media_asset_copy_prunable(p_copy_id bigint, p_original_rules jsonb, p_derived_store text, p_overlap_cutoff timestamp with time zone); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.media_asset_copy_prunable(p_copy_id bigint, p_original_rules jsonb, p_derived_store text, p_overlap_cutoff timestamp with time zone) IS 'Returns true only when a present active copy in the desired store was created at or before the overlap cutoff.';

--
-- Name: media_asset_generation_from_rel_path(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.media_asset_generation_from_rel_path(rel_path text) RETURNS bigint
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
    AS $_$
    SELECT CASE
        WHEN generation IS NULL THEN NULL
        WHEN length(generation) < 19 THEN generation::bigint
        WHEN length(generation) = 19
         AND generation COLLATE "C" <= '9223372036854775807' COLLATE "C"
            THEN generation::bigint
        ELSE NULL
    END
    FROM (
        SELECT (regexp_match(rel_path, '\.[gu]([1-9][0-9]*)\.[^./]+$'))[1] AS generation
    ) AS parsed
$_$;

--
-- Name: media_asset_transfer_progress_abandon_upload(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.media_asset_transfer_progress_abandon_upload() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.upload_state IS NOT NULL THEN
        INSERT INTO media_asset_transfer_abandoned_uploads (store, upload_state)
        VALUES (OLD.upload_store, OLD.upload_state);
    END IF;
    RETURN OLD;
END
$$;

--
-- Name: normalize_search_text(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.normalize_search_text(t text) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT
    AS $$
  SELECT lower(
    translate(
      t,
      'ＡＢＣＤＥＦＧＨＩＪＫＬＭＮＯＰＱＲＳＴＵＶＷＸＹＺａｂｃｄｅｆｇｈｉｊｋｌｍｎｏｐｑｒｓｔｕｖｗｘｙｚ０１２３４５６７８９　',
      'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789 '
    )
  )
$$;

--
-- Name: preferred_active_media_asset_copy(bigint, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.preferred_active_media_asset_copy(p_asset_id bigint, p_store text) RETURNS SETOF public.media_asset_copies
    LANGUAGE sql STABLE
    AS $$
SELECT c.* FROM active_media_asset_copies(p_asset_id, p_store) c ORDER BY c.store LIMIT 1
$$;

--
-- Name: FUNCTION preferred_active_media_asset_copy(p_asset_id bigint, p_store text); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.preferred_active_media_asset_copy(p_asset_id bigint, p_store text) IS 'preferred_active_media_asset_copy は、active_media_asset_copies のうち store 名順で先頭の 1 件を返す。コピーを 1 件選ぶ読み手が使う。';

--
-- Name: preferred_readable_media_asset_copy(bigint, text, text[]); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.preferred_readable_media_asset_copy(p_asset_id bigint, p_preferred_store text, p_readable_stores text[]) RETURNS SETOF public.media_asset_copies
    LANGUAGE sql STABLE
    AS $$
SELECT c.*
FROM active_media_asset_copies(p_asset_id, NULL::text) c
WHERE p_readable_stores IS NULL OR c.store = ANY(p_readable_stores)
ORDER BY (c.store = p_preferred_store) DESC, c.store
LIMIT 1
$$;

--
-- Name: FUNCTION preferred_readable_media_asset_copy(p_asset_id bigint, p_preferred_store text, p_readable_stores text[]); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.preferred_readable_media_asset_copy(p_asset_id bigint, p_preferred_store text, p_readable_stores text[]) IS 'preferred_readable_media_asset_copy は、読める置き場の active コピーから p_preferred_store を優先し、無ければ置き場名順で 1 件選ぶ。p_readable_stores が NULL なら全置き場が読める扱い。読めないコピーを選ばない。';

--
-- Name: recordings_label_rule_sync(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.recordings_label_rule_sync() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    winner bigint;
BEGIN
    DELETE FROM public.label_rule_hits WHERE recording_id = NEW.id;
    winner := public.label_rule_winner(NEW.title);
    IF winner IS NOT NULL THEN
        INSERT INTO public.label_rule_hits (recording_id, label_rule_id)
        VALUES (NEW.id, winner);
    END IF;
    RETURN NULL;
END;
$$;

--
-- Name: rokuban_notify(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.rokuban_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('rokuban', TG_ARGV[0]);
    RETURN NULL;
END;
$$;

--
-- Name: series_key(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.series_key(t text) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT
    AS $$
  WITH a AS (
    SELECT normalize(t, NFKC) AS t
  ), b AS (
    SELECT regexp_replace(regexp_replace(t, '\[[^]]*\]', '', 'g'), '【[^】]*】', '', 'g') AS t FROM a
  ), c AS (
    SELECT substring(regexp_replace(t, '^[▽◇★☆〜「『◆■●○◎※#|]+', '')
                     FROM '^[^▽◇★☆〜「『』」◆■●○◎※#|]*') AS t FROM b
  ), d AS (
    SELECT regexp_replace(t, '第[0-9]+[話回週]|#[0-9]+|\([0-9]+\)|[0-9]+話', '', 'g') AS t FROM c
  ), e AS (
    -- 枠名（定数）を 1 語飛ばしてから最初の空白で切る。NFKC が全角空白を
    -- ASCII 空白へ畳むので、ここに来る時点で区切りは半角空白である。
    SELECT substring(regexp_replace(t, '^(アニメ|TVアニメ|日5) +', '') FROM '^[^ ]*') AS t FROM d
  )
  SELECT CASE WHEN regexp_replace(t, '[^[:alnum:]]', '', 'g') = '' THEN NULL
              ELSE nullif(btrim(t, ' 　、,・.。:：;；!！?？'), '') END FROM e
$$;

--
-- Name: storage_transfer_attempt_count(bigint, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.storage_transfer_attempt_count(p_media_asset_id bigint, p_desired_store text) RETURNS integer
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

--
-- Name: FUNCTION storage_transfer_attempt_count(p_media_asset_id bigint, p_desired_store text); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.storage_transfer_attempt_count(p_media_asset_id bigint, p_desired_store text) IS 'Returns the failure count for the current-generation destination copy.';

--
-- Name: storage_transfer_candidate(bigint, jsonb, text[], text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.storage_transfer_candidate(p_media_asset_id bigint, p_original_rules jsonb, p_fs_stores text[], p_derived_store text) RETURNS boolean
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

--
-- Name: FUNCTION storage_transfer_candidate(p_media_asset_id bigint, p_original_rules jsonb, p_fs_stores text[], p_derived_store text); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.storage_transfer_candidate(p_media_asset_id bigint, p_original_rules jsonb, p_fs_stores text[], p_derived_store text) IS 'Returns true for an active logical asset whose desired FS copy is absent and whose source is present in a configured FS store.';

--
-- Name: storage_transfer_destination(text, jsonb, text, text[], text, timestamp with time zone, timestamp with time zone); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.storage_transfer_destination(asset_kind text, original_rules jsonb, derived_store text, fs_stores text[], recording_site text, program_end_at timestamp with time zone, recording_deleted_at timestamp with time zone) RETURNS text
    LANGUAGE sql STABLE
    AS $$
    SELECT CASE
        WHEN recording_deleted_at IS NULL
         AND (
             desired_media_store(asset_kind, original_rules, derived_store, recording_site, program_end_at) = ANY(fs_stores)
             OR (
                 asset_kind <> 'original'
                 AND desired_media_store(asset_kind, original_rules, derived_store, recording_site, program_end_at) = derived_store
             )
         )
        THEN desired_media_store(asset_kind, original_rules, derived_store, recording_site, program_end_at)
    END
$$;

--
-- Name: FUNCTION storage_transfer_destination(asset_kind text, original_rules jsonb, derived_store text, fs_stores text[], recording_site text, program_end_at timestamp with time zone, recording_deleted_at timestamp with time zone); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.storage_transfer_destination(asset_kind text, original_rules jsonb, derived_store text, fs_stores text[], recording_site text, program_end_at timestamp with time zone, recording_deleted_at timestamp with time zone) IS 'Returns the store a transfer should write to: the desired store when it is an FS store in fs_stores or the derived store, or NULL when the recording is deleted or the desired store is neither.';

--
-- Name: storage_transfer_pending_copy_obsolete(bigint, jsonb, text[], text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.storage_transfer_pending_copy_obsolete(p_copy_id bigint, p_original_rules jsonb, p_fs_stores text[], p_derived_store text) RETURNS boolean
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

--
-- Name: FUNCTION storage_transfer_pending_copy_obsolete(p_copy_id bigint, p_original_rules jsonb, p_fs_stores text[], p_derived_store text); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.storage_transfer_pending_copy_obsolete(p_copy_id bigint, p_original_rules jsonb, p_fs_stores text[], p_derived_store text) IS 'Returns true only for a pending transfer copy whose recording, generation, desired placement, or until_encoded retention has made the reservation obsolete.';

--
-- Name: trash_deletable_recordings(timestamp with time zone); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.trash_deletable_recordings(grace_cutoff timestamp with time zone) RETURNS TABLE(recording_id bigint)
    LANGUAGE sql STABLE
    AS $$
    SELECT r.id
    FROM recordings r
    WHERE r.deleted_at IS NOT NULL
      AND (
        EXISTS (
          SELECT 1 FROM recording_purge_requests p WHERE p.recording_id = r.id
        )
        OR r.deleted_at <= grace_cutoff
      )
$$;

--
-- Name: until_encoded_deletable_originals(text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.until_encoded_deletable_originals(desired_store text) RETURNS TABLE(asset_id bigint, recording_id bigint)
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

--
-- Name: FUNCTION until_encoded_deletable_originals(desired_store text); Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON FUNCTION public.until_encoded_deletable_originals(desired_store text) IS 'until_encoded_deletable_originals は desired_store の active・実体ありの派生コピーが揃い、保持条件を満たす原本を返す。派生コピーの実体有無は active_present_media_asset_copies で判定する。原本自身の実体有無は見ない（実体無しの原本の削除は ErrNotExist から削除済みの確定まで進む）。';

--
-- Name: circuit_breakers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.circuit_breakers (
    scope text NOT NULL,
    name text NOT NULL,
    tripped_at timestamp with time zone DEFAULT now() NOT NULL,
    pending integer NOT NULL,
    threshold integer NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL
);

--
-- Name: cm_logos; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cm_logos (
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    lgd bytea NOT NULL,
    preview_png bytea,
    learned_at timestamp with time zone DEFAULT now() NOT NULL,
    learned_from bigint,
    coded_width integer NOT NULL,
    coded_height integer NOT NULL,
    CONSTRAINT cm_logos_coded_size_check CHECK (((coded_width > 0) AND (coded_height > 0)))
);

--
-- Name: recording_cm_attempts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_cm_attempts (
    recording_id bigint NOT NULL,
    state text NOT NULL,
    error text,
    attempted_at timestamp with time zone DEFAULT now() NOT NULL,
    stage text,
    attempt_count integer DEFAULT 1 NOT NULL,
    policy_generation bigint NOT NULL,
    CONSTRAINT recording_cm_attempts_attempt_count_check CHECK ((attempt_count >= 1)),
    CONSTRAINT recording_cm_attempts_stage_check CHECK (((stage IS NULL) OR (stage = ANY (ARRAY['setup'::text, 'probe'::text, 'area'::text, 'logo'::text, 'chapter'::text, 'join'::text, 'parse'::text, 'save'::text, 'stopped'::text, 'resolution'::text, 'match'::text, 'adopt'::text])))),
    CONSTRAINT recording_cm_attempts_state_check CHECK ((state = ANY (ARRAY['running'::text, 'retrying'::text, 'failed'::text])))
);

--
-- Name: recording_encode_policy; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_encode_policy (
    recording_id bigint NOT NULL,
    keep_original text NOT NULL,
    encode_profiles text[] NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    cm_detect boolean DEFAULT false NOT NULL,
    cm_detect_generation bigint DEFAULT 0 NOT NULL,
    CONSTRAINT recording_encode_policy_check CHECK (((keep_original <> 'until_encoded'::text) OR (cardinality(encode_profiles) > 0))),
    CONSTRAINT recording_encode_policy_keep_original_check CHECK ((keep_original = ANY (ARRAY['always'::text, 'until_encoded'::text])))
);

--
-- Name: current_cm_attempts; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.current_cm_attempts AS
 SELECT ca.recording_id,
    ca.state,
    ca.stage,
    ca.error,
    ca.attempted_at,
    ca.attempt_count,
    ca.policy_generation
   FROM (public.recording_cm_attempts ca
     JOIN public.recording_encode_policy p ON ((p.recording_id = ca.recording_id)))
  WHERE (p.cm_detect AND (ca.policy_generation = p.cm_detect_generation));

--
-- Name: media_assets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_assets (
    id bigint NOT NULL,
    recording_id bigint NOT NULL,
    kind text NOT NULL,
    profile text,
    size_bytes bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    ingest_generation bigint,
    derived_generation bigint,
    sha256 bytea,
    CONSTRAINT media_assets_check CHECK (((kind = 'encoded'::text) = (profile IS NOT NULL))),
    CONSTRAINT media_assets_derived_generation_check CHECK (((derived_generation IS NULL) OR ((kind <> 'original'::text) AND (derived_generation >= 0)))),
    CONSTRAINT media_assets_ingest_generation_kind_check CHECK (((kind = 'original'::text) OR (ingest_generation IS NULL))),
    CONSTRAINT media_assets_ingest_generation_positive_check CHECK (((ingest_generation IS NULL) OR (ingest_generation > 0))),
    CONSTRAINT media_assets_kind_check CHECK ((kind = ANY (ARRAY['original'::text, 'encoded'::text, 'thumbnail'::text, 'seek_tiles'::text]))),
    CONSTRAINT media_assets_sha256_length_check CHECK (((sha256 IS NULL) OR (octet_length(sha256) = 32)))
);

--
-- Name: COLUMN media_assets.ingest_generation; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.media_assets.ingest_generation IS 'Generation of the ingest temp whose bytes were committed as this original';

--
-- Name: COLUMN media_assets.derived_generation; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.media_assets.derived_generation IS 'Monotonic generation for derived output paths; NULL for original assets';

--
-- Name: COLUMN media_assets.sha256; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.media_assets.sha256 IS 'Observed SHA-256 of the logical asset bytes, stored once for all physical copies';

--
-- Name: recording_cm_detections; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_cm_detections (
    recording_id bigint NOT NULL,
    cm_ranges int8multirange NOT NULL,
    detected_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: recordings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recordings (
    id bigint NOT NULL,
    rule_id bigint,
    source text NOT NULL,
    site text NOT NULL,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    event_id integer NOT NULL,
    service_name text NOT NULL,
    channel_type text NOT NULL,
    channel text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    description text,
    extended jsonb,
    genres jsonb,
    is_free boolean DEFAULT true NOT NULL,
    program_start_at timestamp with time zone NOT NULL,
    program_duration_ms bigint NOT NULL,
    status text NOT NULL,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    quality_events jsonb DEFAULT '[]'::jsonb NOT NULL,
    deleted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    superseded_at timestamp with time zone,
    purged_at timestamp with time zone,
    genre_lv1 smallint[] GENERATED ALWAYS AS (public.genre_lv1_of(genres)) STORED,
    series_key text GENERATED ALWAYS AS (public.series_key(title)) STORED,
    CONSTRAINT recordings_channel_type_check CHECK ((channel_type = ANY (ARRAY['GR'::text, 'BS'::text, 'CS'::text, 'SKY'::text]))),
    CONSTRAINT recordings_source_check CHECK ((source = ANY (ARRAY['rule'::text, 'manual'::text, 'unattributed'::text]))),
    CONSTRAINT recordings_status_check CHECK ((status = ANY (ARRAY['recording'::text, 'finished'::text, 'canceled'::text, 'failed'::text])))
);

--
-- Name: cm_detection_desired; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.cm_detection_desired AS
 SELECT r.id AS recording_id
   FROM ((((public.recordings r
     JOIN public.recording_encode_policy p ON (((p.recording_id = r.id) AND p.cm_detect)))
     JOIN public.media_assets o ON (((o.recording_id = r.id) AND (o.kind = 'original'::text))))
     LEFT JOIN public.current_cm_attempts ca ON ((ca.recording_id = r.id)))
     LEFT JOIN public.cm_logos l ON (((l.network_id = r.network_id) AND (l.service_id = r.service_id))))
  WHERE ((r.deleted_at IS NULL) AND (EXISTS ( SELECT 1
           FROM public.active_present_media_asset_copies(o.id, NULL::text) active_present_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation))) AND (NOT (EXISTS ( SELECT 1
           FROM public.recording_cm_detections d
          WHERE (d.recording_id = r.id)))) AND ((ca.recording_id IS NULL) OR (ca.state <> 'failed'::text) OR ((l.learned_at IS NOT NULL) AND (ca.attempted_at < l.learned_at))));

--
-- Name: VIEW cm_detection_desired; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON VIEW public.cm_detection_desired IS 'cm_detection_desired は、cm_detect が有効で、CM 検出が未了かつ再試行の対象になる録画を表す。原本は実体の有る現行のコピーに限る（実体無しの原本は CM 検出の入力として読めないと観測されたものなので候補にしない）。';

--
-- Name: cm_logo_areas; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cm_logo_areas (
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    x integer NOT NULL,
    y integer NOT NULL,
    w integer NOT NULL,
    h integer NOT NULL,
    coded_width integer NOT NULL,
    coded_height integer NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT cm_logo_areas_rect_within_frame CHECK (((coded_width > 0) AND (coded_height > 0) AND (x >= 0) AND (y >= 0) AND (w > 0) AND (h > 0) AND ((x + w) <= coded_width) AND ((y + h) <= coded_height)))
);

--
-- Name: cm_logo_candidates; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cm_logo_candidates (
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    state text NOT NULL,
    stage text,
    error text,
    x integer NOT NULL,
    y integer NOT NULL,
    w integer NOT NULL,
    h integer NOT NULL,
    coded_width integer NOT NULL,
    coded_height integer NOT NULL,
    recording_id bigint,
    observed_area_updated_at timestamp with time zone NOT NULL,
    lgd bytea,
    preview_png bytea,
    attempted_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT cm_logo_candidates_rect_within_frame CHECK (((coded_width > 0) AND (coded_height > 0) AND (x >= 0) AND (y >= 0) AND (w > 0) AND (h > 0) AND ((x + w) <= coded_width) AND ((y + h) <= coded_height))),
    CONSTRAINT cm_logo_candidates_stage_check CHECK (((stage IS NULL) OR (stage = ANY (ARRAY['setup'::text, 'probe'::text, 'area'::text, 'logo'::text, 'match'::text, 'save'::text, 'stopped'::text])))),
    CONSTRAINT cm_logo_candidates_state_check CHECK ((state = ANY (ARRAY['running'::text, 'failed'::text, 'ready'::text])))
);

--
-- Name: cm_logo_candidate_desired; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.cm_logo_candidate_desired AS
 SELECT area.network_id,
    area.service_id,
    area.updated_at AS area_updated_at,
    ( SELECT r2.id
           FROM (public.recordings r2
             JOIN public.media_assets original ON (((original.recording_id = r2.id) AND (original.kind = 'original'::text))))
          WHERE ((r2.network_id = area.network_id) AND (r2.service_id = area.service_id) AND (r2.deleted_at IS NULL) AND (r2.purged_at IS NULL) AND (EXISTS ( SELECT 1
                   FROM public.active_present_media_asset_copies(original.id, NULL::text) active_present_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation))))
          ORDER BY r2.id DESC
         LIMIT 1) AS recording_id
   FROM ((public.cm_logo_areas area
     LEFT JOIN public.cm_logo_candidates candidate ON (((candidate.network_id = area.network_id) AND (candidate.service_id = area.service_id))))
     LEFT JOIN public.cm_logos logo ON (((logo.network_id = area.network_id) AND (logo.service_id = area.service_id))))
  WHERE ((candidate.network_id IS NULL) AND ((logo.network_id IS NULL) OR (logo.learned_at < area.updated_at)) AND (EXISTS ( SELECT 1
           FROM (public.recordings r
             JOIN public.media_assets original ON (((original.recording_id = r.id) AND (original.kind = 'original'::text))))
          WHERE ((r.network_id = area.network_id) AND (r.service_id = area.service_id) AND (r.deleted_at IS NULL) AND (r.purged_at IS NULL) AND (EXISTS ( SELECT 1
                   FROM public.active_present_media_asset_copies(original.id, NULL::text) active_present_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)))))));

--
-- Name: media_asset_ts_scans; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_asset_ts_scans (
    media_asset_id bigint NOT NULL,
    scanned_size_bytes bigint NOT NULL,
    CONSTRAINT media_asset_ts_scans_scanned_size_bytes_check CHECK ((scanned_size_bytes >= 0))
);

--
-- Name: current_ts_scanned_originals; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.current_ts_scanned_originals AS
 SELECT id AS media_asset_id
   FROM public.media_assets a
  WHERE ((kind = 'original'::text) AND (size_bytes = ( SELECT s.scanned_size_bytes
           FROM public.media_asset_ts_scans s
          WHERE (s.media_asset_id = a.id))));

--
-- Name: VIEW current_ts_scanned_originals; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON VIEW public.current_ts_scanned_originals IS 'CurrentTsScannedOriginal は、計測サイズが現在のコミット済みサイズと一致する原本を表す。state は削除済み原本の drop summary を保つため条件に含めず、active 限定が必要な呼び出し側で絞る。';

--
-- Name: drop_positions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.drop_positions (
    media_asset_id bigint NOT NULL,
    byte_offset bigint NOT NULL,
    pid integer NOT NULL,
    elapsed_ms bigint
);

--
-- Name: drop_stats; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.drop_stats (
    media_asset_id bigint NOT NULL,
    pid integer NOT NULL,
    packets bigint NOT NULL,
    drops bigint NOT NULL,
    errors bigint NOT NULL,
    scrambled bigint NOT NULL,
    pid_type text
);

--
-- Name: encodable_recordings; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.encodable_recordings AS
 SELECT id AS recording_id
   FROM public.recordings r
  WHERE ((deleted_at IS NULL) AND (EXISTS ( SELECT 1
           FROM (public.media_assets o
             JOIN public.media_asset_copies c ON ((c.media_asset_id = o.id)))
          WHERE ((o.recording_id = r.id) AND (o.kind = 'original'::text) AND (c.state = 'active'::text)))));

--
-- Name: VIEW encodable_recordings; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON VIEW public.encodable_recordings IS 'encodable_recordings は、ごみ箱になく active な原本コピーがある録画を表す。encode の不足の補完・待機列・作り直し要求の列挙が共有する。実体無しの原本も含める。単発の走査揺れで encode の入力選択を止めないため。encode の失敗は試行行に記録される。';

--
-- Name: encoded_asset_rebuild_requests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.encoded_asset_rebuild_requests (
    recording_id bigint NOT NULL,
    profile text NOT NULL,
    requested_keep int8multirange NOT NULL,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT encoded_asset_rebuild_requests_requested_keep_check CHECK ((NOT isempty(requested_keep)))
);

--
-- Name: TABLE encoded_asset_rebuild_requests; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.encoded_asset_rebuild_requests IS '利用者が押した時点の keep 区間（量子化済み）。満たされても残る desired で、消すのは api だけ。満たされたかは unsatisfied_encode_rebuilds が導出する。catalog には含めない。';

--
-- Name: encoded_asset_removal_requests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.encoded_asset_removal_requests (
    recording_id bigint NOT NULL,
    profile text NOT NULL,
    requested_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: epg_programs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.epg_programs (
    site text NOT NULL,
    program_id bigint NOT NULL,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    event_id integer NOT NULL,
    start_at timestamp with time zone NOT NULL,
    duration_ms bigint NOT NULL,
    end_at timestamp with time zone NOT NULL,
    is_free boolean DEFAULT true NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    extended jsonb,
    genres jsonb,
    video jsonb,
    audios jsonb,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    genre_lv1 smallint[] GENERATED ALWAYS AS (public.genre_lv1_of(genres)) STORED,
    series_key text GENERATED ALWAYS AS (public.series_key(name)) STORED
)
WITH (autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

--
-- Name: label_rules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.label_rules (
    id bigint NOT NULL,
    key text NOT NULL,
    value text NOT NULL,
    keyword text NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    value_key text GENERATED ALWAYS AS (public.series_key(value)) STORED,
    keyword_key text GENERATED ALWAYS AS (public.normalize_search_text(public.like_escape(keyword))) STORED,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT label_rules_key_check CHECK ((key = 'series'::text)),
    CONSTRAINT label_rules_keyword_check CHECK ((btrim(keyword) <> ''::text)),
    CONSTRAINT label_rules_value_key_check CHECK ((value_key IS NOT NULL))
);

--
-- Name: epg_program_series; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.epg_program_series AS
 SELECT p.site,
    p.program_id,
    lr.value_key AS value
   FROM (public.epg_programs p
     JOIN public.label_rules lr ON (((public.normalize_search_text(p.name) ~~ like_escape((('%'::text || lr.keyword_key) || '%'::text), '\'::text)) AND (public.label_rule_winner(p.name) = lr.id))))
UNION ALL
 SELECT p.site,
    p.program_id,
    p.series_key AS value
   FROM public.epg_programs p
  WHERE (public.label_rule_winner(p.name) IS NULL);

--
-- Name: epg_services; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.epg_services (
    site text NOT NULL,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    type integer NOT NULL,
    logo_id integer NOT NULL,
    remote_control_key_id integer NOT NULL,
    name text NOT NULL,
    channel_type text NOT NULL,
    channel text NOT NULL,
    has_logo_data boolean DEFAULT false NOT NULL,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT epg_services_channel_type_check CHECK ((channel_type = ANY (ARRAY['GR'::text, 'BS'::text, 'CS'::text, 'SKY'::text])))
);

--
-- Name: ingest_temp_generation_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.ingest_temp_generation_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: label_rule_hits; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.label_rule_hits (
    recording_id bigint NOT NULL,
    label_rule_id bigint NOT NULL
);

--
-- Name: label_rules_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.label_rules ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.label_rules_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

--
-- Name: media_asset_copies_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.media_asset_copies ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.media_asset_copies_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

--
-- Name: media_asset_copy_transfer_progress; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_asset_copy_transfer_progress (
    copy_id bigint NOT NULL,
    lease_owner uuid NOT NULL,
    lease_expires_at timestamp with time zone NOT NULL,
    generation bigint NOT NULL,
    handed_off boolean DEFAULT false NOT NULL,
    offset_bytes bigint DEFAULT 0 NOT NULL,
    hash_state bytea,
    upload_store text,
    upload_state bytea,
    verifying_source boolean DEFAULT false NOT NULL,
    CONSTRAINT media_asset_copy_transfer_progress_generation_check CHECK ((generation > 0)),
    CONSTRAINT media_asset_copy_transfer_progress_offset_bytes_check CHECK ((offset_bytes >= 0)),
    CONSTRAINT media_asset_copy_transfer_progress_upload_pair CHECK (((upload_store IS NULL) = (upload_state IS NULL))),
    CONSTRAINT media_asset_copy_transfer_progress_verifying_exclusive CHECK ((NOT (verifying_source AND (upload_state IS NOT NULL))))
);

--
-- Name: media_asset_cuts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_asset_cuts (
    media_asset_id bigint NOT NULL,
    keep_ranges int8multirange NOT NULL,
    CONSTRAINT media_asset_cuts_keep_ranges_check CHECK ((NOT isempty(keep_ranges)))
);

--
-- Name: media_asset_thumbnail_seeks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_asset_thumbnail_seeks (
    media_asset_id bigint NOT NULL,
    seek_ms bigint NOT NULL,
    CONSTRAINT media_asset_thumbnail_seeks_seek_ms_check CHECK ((seek_ms >= 0))
);

--
-- Name: media_asset_transfer_abandoned_uploads; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_asset_transfer_abandoned_uploads (
    id bigint NOT NULL,
    store text NOT NULL,
    upload_state bytea NOT NULL,
    abandoned_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE media_asset_transfer_abandoned_uploads; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.media_asset_transfer_abandoned_uploads IS 'Upload states no longer used by any transfer; a row exists until the store has aborted that upload.';

--
-- Name: media_asset_transfer_abandoned_uploads_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.media_asset_transfer_abandoned_uploads_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: media_asset_transfer_abandoned_uploads_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.media_asset_transfer_abandoned_uploads_id_seq OWNED BY public.media_asset_transfer_abandoned_uploads.id;

--
-- Name: media_asset_transfer_attempts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_asset_transfer_attempts (
    copy_id bigint NOT NULL,
    attempt_count integer NOT NULL,
    last_error text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT media_asset_transfer_attempts_attempt_count_check CHECK ((attempt_count > 0))
);

--
-- Name: TABLE media_asset_transfer_attempts; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.media_asset_transfer_attempts IS 'Failure observations for one destination media asset copy; successful transfers clear the row.';

--
-- Name: media_asset_transfer_objects; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_asset_transfer_objects (
    copy_id bigint NOT NULL,
    rel_path text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE media_asset_transfer_objects; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.media_asset_transfer_objects IS 'Ownership evidence for destination object keys created by an in-flight media asset transfer.';

--
-- Name: media_assets_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.media_assets ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.media_assets_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

--
-- Name: missing_media_assets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.missing_media_assets (
    first_seen timestamp with time zone DEFAULT now() NOT NULL,
    media_asset_copy_id bigint NOT NULL
);

--
-- Name: TABLE missing_media_assets; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.missing_media_assets IS 'Physical media copies that a filesystem scan failed to observe';

--
-- Name: never_scheduled_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.never_scheduled_events (
    site text NOT NULL,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    event_id integer NOT NULL,
    observed_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: old_generation_media_asset_copies; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.old_generation_media_asset_copies AS
 SELECT old_copy.id,
    old_copy.media_asset_id,
    old_copy.store,
    old_copy.rel_path,
    old_copy.state,
    old_copy.created_at,
    old_copy.deleted_at,
    old_copy.generation,
    ( SELECT min(current_copy.created_at) AS min
           FROM public.media_asset_copies current_copy
          WHERE ((current_copy.media_asset_id = old_copy.media_asset_id) AND (current_copy.generation = COALESCE(asset.derived_generation, (0)::bigint)))) AS generation_started_at
   FROM (public.media_asset_copies old_copy
     JOIN public.media_assets asset ON ((asset.id = old_copy.media_asset_id)))
  WHERE ((asset.kind <> 'original'::text) AND (old_copy.generation < COALESCE(asset.derived_generation, (0)::bigint)) AND (EXISTS ( SELECT 1
           FROM public.active_present_media_asset_copies(old_copy.media_asset_id, NULL::text) active_present_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation))));

--
-- Name: VIEW old_generation_media_asset_copies; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON VIEW public.old_generation_media_asset_copies IS 'old_generation_media_asset_copies は、論理資産の derived_generation より古い派生コピーのうち、実体無しでない現行の active コピーがどこかの置き場にあるものを返す。generation_started_at は現行世代の最初のコピー行の created_at で、呼び出し側が overlap の猶予を判定する。';

--
-- Name: orphan_files; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.orphan_files (
    rel_path text NOT NULL,
    first_seen timestamp with time zone DEFAULT now() NOT NULL,
    store text DEFAULT ''::text NOT NULL
);

--
-- Name: COLUMN orphan_files.store; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.orphan_files.store IS 'Store where the orphan path was observed; empty until storage adopt';

--
-- Name: program_intents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.program_intents (
    site text NOT NULL,
    program_id bigint NOT NULL,
    action text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT program_intents_action_check CHECK ((action = ANY (ARRAY['record'::text, 'skip'::text])))
);

--
-- Name: program_overrides; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.program_overrides (
    site text NOT NULL,
    program_id bigint NOT NULL,
    overrides jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: program_investments; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.program_investments AS
 SELECT program_intents.site,
    program_intents.program_id
   FROM public.program_intents
  WHERE (program_intents.action = 'record'::text)
UNION
 SELECT program_overrides.site,
    program_overrides.program_id
   FROM public.program_overrides;

--
-- Name: program_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.program_snapshots (
    site text NOT NULL,
    program_id bigint NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    start_at timestamp with time zone NOT NULL,
    duration_ms bigint NOT NULL,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    channel_type text NOT NULL,
    channel text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    event_id integer NOT NULL,
    service_name text NOT NULL,
    CONSTRAINT program_snapshots_channel_type_check CHECK ((channel_type = ANY (ARRAY['GR'::text, 'BS'::text, 'CS'::text, 'SKY'::text])))
);

--
-- Name: record_sweep_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.record_sweep_snapshots (
    site text NOT NULL,
    last_success_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: record_sync; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.record_sync (
    site text NOT NULL,
    record_id text NOT NULL,
    recording_id bigint,
    program_id bigint NOT NULL,
    status text NOT NULL,
    content_path text,
    content_length bigint,
    tags text[] DEFAULT '{}'::text[] NOT NULL,
    failed_reason jsonb,
    observed_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: recording_chapter_ownership; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_chapter_ownership (
    recording_id bigint NOT NULL,
    adopted_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: recording_chapter_spans; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_chapter_spans (
    recording_id bigint NOT NULL,
    span int8range NOT NULL,
    label text,
    cut boolean NOT NULL,
    CONSTRAINT recording_chapter_spans_check CHECK (((label IS NOT NULL) OR cut)),
    CONSTRAINT recording_chapter_spans_span_check CHECK ((NOT isempty(span)))
);

--
-- Name: recording_encode_attempts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_encode_attempts (
    recording_id bigint NOT NULL,
    profile text NOT NULL,
    state text NOT NULL,
    error text,
    attempted_at timestamp with time zone DEFAULT now() NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    CONSTRAINT recording_encode_attempts_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT recording_encode_attempts_state_check CHECK ((state = ANY (ARRAY['running'::text, 'failed'::text])))
);

--
-- Name: recording_ingest_checkpoints; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_ingest_checkpoints (
    recording_id bigint NOT NULL,
    lease_owner uuid NOT NULL,
    lease_expires_at timestamp with time zone NOT NULL,
    writer_generation bigint NOT NULL,
    handed_off boolean DEFAULT false NOT NULL,
    offset_bytes bigint DEFAULT 0 NOT NULL,
    hash_state bytea,
    canonical_rel_path text,
    CONSTRAINT recording_ingest_checkpoints_check CHECK (((NOT handed_off) OR (hash_state IS NOT NULL))),
    CONSTRAINT recording_ingest_checkpoints_offset_bytes_check CHECK ((offset_bytes >= 0)),
    CONSTRAINT recording_ingest_checkpoints_writer_generation_check CHECK ((writer_generation > 0))
);

--
-- Name: recording_ingest_progress; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_ingest_progress (
    recording_id bigint NOT NULL,
    written_bytes bigint NOT NULL,
    expected_bytes bigint,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    temp_rel_path text NOT NULL,
    temp_generation bigint NOT NULL,
    temp_store text DEFAULT ''::text NOT NULL,
    CONSTRAINT recording_ingest_progress_expected_bytes_check CHECK (((expected_bytes IS NULL) OR (expected_bytes >= 0))),
    CONSTRAINT recording_ingest_progress_temp_generation_check CHECK ((temp_generation > 0)),
    CONSTRAINT recording_ingest_progress_written_bytes_check CHECK ((written_bytes >= 0))
);

--
-- Name: COLUMN recording_ingest_progress.temp_rel_path; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.recording_ingest_progress.temp_rel_path IS 'Media-root-relative path of the ingest temp associated with this transfer';

--
-- Name: COLUMN recording_ingest_progress.temp_generation; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.recording_ingest_progress.temp_generation IS 'Generation of the current ingest temp file';

--
-- Name: COLUMN recording_ingest_progress.temp_store; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.recording_ingest_progress.temp_store IS 'Store containing the current ingest temporary file';

--
-- Name: recording_playback_positions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_playback_positions (
    recording_id bigint NOT NULL,
    position_ms bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT recording_playback_positions_position_ms_check CHECK ((position_ms >= 2000))
);

--
-- Name: recording_purge_requests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_purge_requests (
    recording_id bigint NOT NULL,
    requested_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: recording_series; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.recording_series AS
 SELECT r.id AS recording_id,
    COALESCE(lr.value_key, r.series_key) AS value
   FROM ((public.recordings r
     LEFT JOIN public.label_rule_hits h ON ((h.recording_id = r.id)))
     LEFT JOIN public.label_rules lr ON ((lr.id = h.label_rule_id)));

--
-- Name: recording_watched; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_watched (
    recording_id bigint NOT NULL,
    watched_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: recordings_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.recordings ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.recordings_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

--
-- Name: removed_encoded_assets; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.removed_encoded_assets AS
 SELECT a.id AS asset_id,
    a.recording_id
   FROM ((public.media_assets a
     JOIN public.encoded_asset_removal_requests q ON (((q.recording_id = a.recording_id) AND (q.profile = a.profile))))
     JOIN public.recording_encode_policy p ON ((p.recording_id = a.recording_id)))
  WHERE ((a.kind = 'encoded'::text) AND (NOT (a.profile = ANY (p.encode_profiles))) AND (EXISTS ( SELECT 1
           FROM public.media_assets other
          WHERE ((other.recording_id = a.recording_id) AND (((other.kind = 'original'::text) AND (EXISTS ( SELECT 1
                   FROM public.active_media_asset_copies(other.id, NULL::text) active_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)))) OR ((other.kind = 'encoded'::text) AND (NOT (EXISTS ( SELECT 1
                   FROM public.encoded_asset_removal_requests oq
                  WHERE ((oq.recording_id = other.recording_id) AND (oq.profile = other.profile))))) AND (EXISTS ( SELECT 1
                   FROM public.active_media_asset_copies(other.id, NULL::text) active_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)))))))));

--
-- Name: VIEW removed_encoded_assets; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON VIEW public.removed_encoded_assets IS 'removed_encoded_assets は、ユーザーが外した encoded 版のうち、他の版が active で残るものを表す。実体無しを除かない。削除側は unlink の ErrNotExist から削除済みの確定まで進むので、除くと実体無しの行が残り続ける。他の版の判定も実体無しを除かないため、実体無しの原本も「残る版」として数える（この判定を実体で見るべきかは未決）。';

--
-- Name: reservations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reservations (
    site text NOT NULL,
    program_id bigint NOT NULL,
    rule_id bigint,
    base jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    dedup_match_recording_id bigint,
    dedup_similarity real,
    CONSTRAINT reservations_dedup_evidence_check CHECK (((dedup_match_recording_id IS NULL) = (dedup_similarity IS NULL)))
);

--
-- Name: rule_channel_types; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule_channel_types (
    rule_id bigint NOT NULL,
    channel_type text NOT NULL,
    CONSTRAINT rule_channel_types_channel_type_check CHECK ((channel_type = ANY (ARRAY['GR'::text, 'BS'::text, 'CS'::text, 'SKY'::text])))
);

--
-- Name: rule_genres; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule_genres (
    rule_id bigint NOT NULL,
    genre_lv1 smallint NOT NULL,
    CONSTRAINT rule_genres_genre_lv1_check CHECK (((genre_lv1 >= 0) AND (genre_lv1 <= 15)))
);

--
-- Name: rule_services; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule_services (
    rule_id bigint NOT NULL,
    network_id integer NOT NULL,
    service_id integer NOT NULL
);

--
-- Name: rule_sites; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule_sites (
    rule_id bigint NOT NULL,
    site text NOT NULL,
    CONSTRAINT rule_sites_site_check CHECK ((site <> ''::text))
);

--
-- Name: rule_text_matches; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule_text_matches (
    rule_id bigint NOT NULL,
    seq integer NOT NULL,
    target text NOT NULL,
    mode text NOT NULL,
    value text NOT NULL,
    case_sensitive boolean DEFAULT false NOT NULL,
    negate boolean DEFAULT false NOT NULL,
    CONSTRAINT rule_text_matches_mode_check CHECK ((mode = ANY (ARRAY['keyword'::text, 'regex'::text]))),
    CONSTRAINT rule_text_matches_seq_check CHECK ((seq >= 0)),
    CONSTRAINT rule_text_matches_target_check CHECK ((target = ANY (ARRAY['name'::text, 'description'::text, 'extended'::text]))),
    CONSTRAINT rule_text_matches_value_check CHECK ((value <> ''::text))
);

--
-- Name: rule_times; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rule_times (
    rule_id bigint NOT NULL,
    seq integer NOT NULL,
    weekdays integer NOT NULL,
    start_sec integer NOT NULL,
    end_sec integer NOT NULL,
    CONSTRAINT rule_times_end_sec_check CHECK (((end_sec >= 0) AND (end_sec <= 86400))),
    CONSTRAINT rule_times_seq_check CHECK ((seq >= 0)),
    CONSTRAINT rule_times_start_sec_check CHECK (((start_sec >= 0) AND (start_sec <= 86400))),
    CONSTRAINT rule_times_weekdays_check CHECK (((weekdays >= 1) AND (weekdays <= 127)))
);

--
-- Name: ruler_input_revisions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ruler_input_revisions (
    site text NOT NULL,
    revision bigint NOT NULL,
    CONSTRAINT ruler_input_revisions_revision_check CHECK ((revision >= 0))
);

--
-- Name: ruler_pass_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ruler_pass_snapshots (
    site text NOT NULL,
    last_success_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: rules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rules (
    id bigint NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    priority integer DEFAULT 10 NOT NULL,
    is_free boolean,
    duration_min_ms bigint,
    duration_max_ms bigint,
    period_start_at timestamp with time zone,
    period_end_at timestamp with time zone,
    dedupe_enabled boolean DEFAULT false NOT NULL,
    dedupe_threshold real,
    dedupe_window interval,
    keep_original text DEFAULT 'always'::text NOT NULL,
    encode_profiles text[] DEFAULT '{}'::text[] NOT NULL,
    filename_template text DEFAULT ''::text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT rules_check CHECK (((duration_min_ms IS NULL) OR (duration_max_ms IS NULL) OR (duration_min_ms <= duration_max_ms))),
    CONSTRAINT rules_check1 CHECK (((dedupe_enabled = false) OR (dedupe_threshold IS NOT NULL))),
    CONSTRAINT rules_check2 CHECK (((keep_original <> 'until_encoded'::text) OR (cardinality(encode_profiles) > 0))),
    CONSTRAINT rules_dedupe_threshold_range CHECK (((dedupe_threshold IS NULL) OR ((dedupe_threshold > (0)::double precision) AND (dedupe_threshold <= (1)::double precision)))),
    CONSTRAINT rules_dedupe_window_positive CHECK (((dedupe_window IS NULL) OR (dedupe_window > '00:00:00'::interval))),
    CONSTRAINT rules_encode_profiles_check CHECK (public.array_is_canonical_set(encode_profiles)),
    CONSTRAINT rules_keep_original_check CHECK ((keep_original = ANY (ARRAY['always'::text, 'until_encoded'::text])))
);

--
-- Name: rules_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.rules ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.rules_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

--
-- Name: schedule_sync; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schedule_sync (
    site text NOT NULL,
    program_id bigint NOT NULL,
    state text NOT NULL,
    options jsonb NOT NULL,
    tags text[] DEFAULT '{}'::text[] NOT NULL,
    failed_reason jsonb,
    observed_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: schedule_sync_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schedule_sync_snapshots (
    site text NOT NULL,
    snapshot_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: storage_sync; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_sync (
    root text NOT NULL,
    path text NOT NULL,
    total_bytes bigint NOT NULL,
    used_bytes bigint NOT NULL,
    available_bytes bigint NOT NULL,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    store text DEFAULT ''::text NOT NULL,
    CONSTRAINT storage_sync_root_check CHECK ((root = ANY (ARRAY['media'::text, 'scratch'::text])))
);

--
-- Name: COLUMN storage_sync.store; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_sync.store IS 'Store whose filesystem root was observed; empty until storage adopt';

--
-- Name: transfer_backlog_snapshots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.transfer_backlog_snapshots (
    store text NOT NULL,
    assets bigint NOT NULL,
    counted_at timestamp with time zone NOT NULL,
    CONSTRAINT transfer_backlog_snapshots_assets_check CHECK ((assets >= 0))
);

--
-- Name: tuner_sync; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tuner_sync (
    site text NOT NULL,
    tuner_index integer NOT NULL,
    name text NOT NULL,
    types text[] NOT NULL,
    is_available boolean NOT NULL,
    is_fault boolean NOT NULL,
    observed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tuner_sync_types_check CHECK ((types <@ ARRAY['GR'::text, 'BS'::text, 'CS'::text, 'SKY'::text]))
);

--
-- Name: unencoded_desired_profiles; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.unencoded_desired_profiles AS
 SELECT p.recording_id,
    want.profile,
    want.ordinality
   FROM (public.recording_encode_policy p
     CROSS JOIN LATERAL unnest(p.encode_profiles) WITH ORDINALITY want(profile, ordinality))
  WHERE (NOT (EXISTS ( SELECT 1
           FROM public.media_assets e
          WHERE ((e.recording_id = p.recording_id) AND (e.kind = 'encoded'::text) AND (e.profile = want.profile) AND (EXISTS ( SELECT 1
                   FROM public.active_media_asset_copies(e.id, NULL::text) active_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)))))));

--
-- Name: VIEW unencoded_desired_profiles; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON VIEW public.unencoded_desired_profiles IS 'unencoded_desired_profiles は、凍結済みの desired のうち current active encoded コピーが無い (recording_id, profile) を表す。現在の設定では絞らない。実体無しの encoded も「有る」と数える（実体無しを作り直しの根拠にしない）。encode の不足の補完・待機列と、until_encoded の削除判定が共有する。';

--
-- Name: unsatisfied_encode_rebuilds; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.unsatisfied_encode_rebuilds AS
 SELECT q.recording_id,
    q.profile,
    q.requested_keep
   FROM (public.encoded_asset_rebuild_requests q
     JOIN public.encodable_recordings er ON ((er.recording_id = q.recording_id)))
  WHERE (NOT (EXISTS ( SELECT 1
           FROM (public.media_assets e
             JOIN public.media_asset_cuts c ON ((c.media_asset_id = e.id)))
          WHERE ((e.recording_id = q.recording_id) AND (e.kind = 'encoded'::text) AND (e.profile = q.profile) AND (c.keep_ranges = q.requested_keep) AND (EXISTS ( SELECT 1
                   FROM public.active_media_asset_copies(e.id, NULL::text) active_media_asset_copies(id, media_asset_id, store, rel_path, state, created_at, deleted_at, generation)))))));

--
-- Name: VIEW unsatisfied_encode_rebuilds; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON VIEW public.unsatisfied_encode_rebuilds IS 'unsatisfied_encode_rebuilds は、requested_keep と一致する凍結区間の active な encoded がまだ無い作り直し要求を表す。ごみ箱・原本が active でない録画は含まない。実体無しの encoded は「有る」と数える（unencoded_desired_profiles と同じ理由）。';

--
-- Name: media_asset_transfer_abandoned_uploads id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_transfer_abandoned_uploads ALTER COLUMN id SET DEFAULT nextval('public.media_asset_transfer_abandoned_uploads_id_seq'::regclass);

--
-- Name: circuit_breakers circuit_breakers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.circuit_breakers
    ADD CONSTRAINT circuit_breakers_pkey PRIMARY KEY (scope, name);

--
-- Name: cm_logo_areas cm_logo_areas_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cm_logo_areas
    ADD CONSTRAINT cm_logo_areas_pkey PRIMARY KEY (network_id, service_id);

--
-- Name: cm_logo_candidates cm_logo_candidates_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cm_logo_candidates
    ADD CONSTRAINT cm_logo_candidates_pkey PRIMARY KEY (network_id, service_id);

--
-- Name: cm_logos cm_logos_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cm_logos
    ADD CONSTRAINT cm_logos_pkey PRIMARY KEY (network_id, service_id);

--
-- Name: drop_positions drop_positions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.drop_positions
    ADD CONSTRAINT drop_positions_pkey PRIMARY KEY (media_asset_id, byte_offset);

--
-- Name: drop_stats drop_stats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.drop_stats
    ADD CONSTRAINT drop_stats_pkey PRIMARY KEY (media_asset_id, pid);

--
-- Name: encoded_asset_rebuild_requests encoded_asset_rebuild_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.encoded_asset_rebuild_requests
    ADD CONSTRAINT encoded_asset_rebuild_requests_pkey PRIMARY KEY (recording_id, profile);

--
-- Name: encoded_asset_removal_requests encoded_asset_removal_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.encoded_asset_removal_requests
    ADD CONSTRAINT encoded_asset_removal_requests_pkey PRIMARY KEY (recording_id, profile);

--
-- Name: epg_programs epg_programs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.epg_programs
    ADD CONSTRAINT epg_programs_pkey PRIMARY KEY (site, program_id);

--
-- Name: epg_services epg_services_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.epg_services
    ADD CONSTRAINT epg_services_pkey PRIMARY KEY (site, network_id, service_id);

--
-- Name: label_rule_hits label_rule_hits_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.label_rule_hits
    ADD CONSTRAINT label_rule_hits_pkey PRIMARY KEY (recording_id);

--
-- Name: label_rules label_rules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.label_rules
    ADD CONSTRAINT label_rules_pkey PRIMARY KEY (id);

--
-- Name: media_asset_copies media_asset_copies_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_copies
    ADD CONSTRAINT media_asset_copies_pkey PRIMARY KEY (id);

--
-- Name: media_asset_copy_transfer_progress media_asset_copy_transfer_progress_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_copy_transfer_progress
    ADD CONSTRAINT media_asset_copy_transfer_progress_pkey PRIMARY KEY (copy_id);

--
-- Name: media_asset_cuts media_asset_cuts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_cuts
    ADD CONSTRAINT media_asset_cuts_pkey PRIMARY KEY (media_asset_id);

--
-- Name: media_asset_thumbnail_seeks media_asset_thumbnail_seeks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_thumbnail_seeks
    ADD CONSTRAINT media_asset_thumbnail_seeks_pkey PRIMARY KEY (media_asset_id);

--
-- Name: media_asset_transfer_abandoned_uploads media_asset_transfer_abandoned_uploads_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_transfer_abandoned_uploads
    ADD CONSTRAINT media_asset_transfer_abandoned_uploads_pkey PRIMARY KEY (id);

--
-- Name: media_asset_transfer_attempts media_asset_transfer_attempts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_transfer_attempts
    ADD CONSTRAINT media_asset_transfer_attempts_pkey PRIMARY KEY (copy_id);

--
-- Name: media_asset_transfer_objects media_asset_transfer_objects_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_transfer_objects
    ADD CONSTRAINT media_asset_transfer_objects_pkey PRIMARY KEY (copy_id, rel_path);

--
-- Name: media_asset_ts_scans media_asset_ts_scans_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_ts_scans
    ADD CONSTRAINT media_asset_ts_scans_pkey PRIMARY KEY (media_asset_id);

--
-- Name: media_assets media_assets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_assets
    ADD CONSTRAINT media_assets_pkey PRIMARY KEY (id);

--
-- Name: media_assets media_assets_recording_id_kind_profile_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_assets
    ADD CONSTRAINT media_assets_recording_id_kind_profile_key UNIQUE NULLS NOT DISTINCT (recording_id, kind, profile);

--
-- Name: missing_media_assets missing_media_assets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.missing_media_assets
    ADD CONSTRAINT missing_media_assets_pkey PRIMARY KEY (media_asset_copy_id);

--
-- Name: never_scheduled_events never_scheduled_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.never_scheduled_events
    ADD CONSTRAINT never_scheduled_events_pkey PRIMARY KEY (site, network_id, service_id, event_id);

--
-- Name: orphan_files orphan_files_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.orphan_files
    ADD CONSTRAINT orphan_files_pkey PRIMARY KEY (store, rel_path);

--
-- Name: program_intents program_intents_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.program_intents
    ADD CONSTRAINT program_intents_pkey PRIMARY KEY (site, program_id);

--
-- Name: program_overrides program_overrides_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.program_overrides
    ADD CONSTRAINT program_overrides_pkey PRIMARY KEY (site, program_id);

--
-- Name: program_snapshots program_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.program_snapshots
    ADD CONSTRAINT program_snapshots_pkey PRIMARY KEY (site, program_id);

--
-- Name: record_sweep_snapshots record_sweep_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.record_sweep_snapshots
    ADD CONSTRAINT record_sweep_snapshots_pkey PRIMARY KEY (site);

--
-- Name: record_sync record_sync_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.record_sync
    ADD CONSTRAINT record_sync_pkey PRIMARY KEY (site, record_id);

--
-- Name: recording_chapter_ownership recording_chapter_ownership_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_chapter_ownership
    ADD CONSTRAINT recording_chapter_ownership_pkey PRIMARY KEY (recording_id);

--
-- Name: recording_chapter_spans recording_chapter_spans_recording_id_span_excl; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_chapter_spans
    ADD CONSTRAINT recording_chapter_spans_recording_id_span_excl EXCLUDE USING gist (recording_id WITH =, span WITH &&);

--
-- Name: recording_cm_attempts recording_cm_attempts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_cm_attempts
    ADD CONSTRAINT recording_cm_attempts_pkey PRIMARY KEY (recording_id);

--
-- Name: recording_cm_detections recording_cm_detections_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_cm_detections
    ADD CONSTRAINT recording_cm_detections_pkey PRIMARY KEY (recording_id);

--
-- Name: recording_encode_attempts recording_encode_attempts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_encode_attempts
    ADD CONSTRAINT recording_encode_attempts_pkey PRIMARY KEY (recording_id, profile);

--
-- Name: recording_encode_policy recording_encode_policy_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_encode_policy
    ADD CONSTRAINT recording_encode_policy_pkey PRIMARY KEY (recording_id);

--
-- Name: recording_ingest_checkpoints recording_ingest_checkpoints_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_ingest_checkpoints
    ADD CONSTRAINT recording_ingest_checkpoints_pkey PRIMARY KEY (recording_id);

--
-- Name: recording_ingest_progress recording_ingest_progress_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_ingest_progress
    ADD CONSTRAINT recording_ingest_progress_pkey PRIMARY KEY (recording_id);

--
-- Name: recording_playback_positions recording_playback_positions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_playback_positions
    ADD CONSTRAINT recording_playback_positions_pkey PRIMARY KEY (recording_id);

--
-- Name: recording_purge_requests recording_purge_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_purge_requests
    ADD CONSTRAINT recording_purge_requests_pkey PRIMARY KEY (recording_id);

--
-- Name: recording_watched recording_watched_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_watched
    ADD CONSTRAINT recording_watched_pkey PRIMARY KEY (recording_id);

--
-- Name: recordings recordings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recordings
    ADD CONSTRAINT recordings_pkey PRIMARY KEY (id);

--
-- Name: reservations reservations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_pkey PRIMARY KEY (site, program_id);

--
-- Name: rule_channel_types rule_channel_types_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_channel_types
    ADD CONSTRAINT rule_channel_types_pkey PRIMARY KEY (rule_id, channel_type);

--
-- Name: rule_genres rule_genres_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_genres
    ADD CONSTRAINT rule_genres_pkey PRIMARY KEY (rule_id, genre_lv1);

--
-- Name: rule_services rule_services_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_services
    ADD CONSTRAINT rule_services_pkey PRIMARY KEY (rule_id, network_id, service_id);

--
-- Name: rule_sites rule_sites_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_sites
    ADD CONSTRAINT rule_sites_pkey PRIMARY KEY (rule_id, site);

--
-- Name: rule_text_matches rule_text_matches_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_text_matches
    ADD CONSTRAINT rule_text_matches_pkey PRIMARY KEY (rule_id, seq);

--
-- Name: rule_times rule_times_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_times
    ADD CONSTRAINT rule_times_pkey PRIMARY KEY (rule_id, seq);

--
-- Name: ruler_input_revisions ruler_input_revisions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ruler_input_revisions
    ADD CONSTRAINT ruler_input_revisions_pkey PRIMARY KEY (site);

--
-- Name: ruler_pass_snapshots ruler_pass_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ruler_pass_snapshots
    ADD CONSTRAINT ruler_pass_snapshots_pkey PRIMARY KEY (site);

--
-- Name: rules rules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rules
    ADD CONSTRAINT rules_pkey PRIMARY KEY (id);

--
-- Name: schedule_sync schedule_sync_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schedule_sync
    ADD CONSTRAINT schedule_sync_pkey PRIMARY KEY (site, program_id);

--
-- Name: schedule_sync_snapshots schedule_sync_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schedule_sync_snapshots
    ADD CONSTRAINT schedule_sync_snapshots_pkey PRIMARY KEY (site);

--
-- Name: storage_sync storage_sync_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_sync
    ADD CONSTRAINT storage_sync_pkey PRIMARY KEY (store, root);

--
-- Name: transfer_backlog_snapshots transfer_backlog_snapshots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.transfer_backlog_snapshots
    ADD CONSTRAINT transfer_backlog_snapshots_pkey PRIMARY KEY (store);

--
-- Name: tuner_sync tuner_sync_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tuner_sync
    ADD CONSTRAINT tuner_sync_pkey PRIMARY KEY (site, tuner_index);

--
-- Name: epg_programs_description_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_programs_description_idx ON public.epg_programs USING gin (description public.gin_trgm_ops);

--
-- Name: epg_programs_description_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_programs_description_trgm ON public.epg_programs USING gin (public.normalize_search_text(description) public.gin_trgm_ops);

--
-- Name: epg_programs_end_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_programs_end_at_idx ON public.epg_programs USING btree (end_at);

--
-- Name: epg_programs_genre_lv1_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_programs_genre_lv1_idx ON public.epg_programs USING gin (genre_lv1);

--
-- Name: epg_programs_name_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_programs_name_idx ON public.epg_programs USING gin (name public.gin_trgm_ops);

--
-- Name: epg_programs_name_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_programs_name_trgm ON public.epg_programs USING gin (public.normalize_search_text(name) public.gin_trgm_ops);

--
-- Name: epg_programs_series_key_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_programs_series_key_idx ON public.epg_programs USING btree (series_key);

--
-- Name: epg_programs_site_network_id_service_id_start_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_programs_site_network_id_service_id_start_at_idx ON public.epg_programs USING btree (site, network_id, service_id, start_at);

--
-- Name: epg_programs_site_start_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_programs_site_start_at_idx ON public.epg_programs USING btree (site, start_at);

--
-- Name: epg_services_site_channel_type_remote_control_key_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX epg_services_site_channel_type_remote_control_key_id_idx ON public.epg_services USING btree (site, channel_type, remote_control_key_id);

--
-- Name: media_asset_copies_active_asset_store_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX media_asset_copies_active_asset_store_idx ON public.media_asset_copies USING btree (media_asset_id, store) WHERE (state = 'active'::text);

--
-- Name: media_asset_copies_asset_store_generation_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX media_asset_copies_asset_store_generation_idx ON public.media_asset_copies USING btree (media_asset_id, store, generation) WHERE (state <> 'deleted'::text);

--
-- Name: media_asset_copies_media_asset_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX media_asset_copies_media_asset_id_idx ON public.media_asset_copies USING btree (media_asset_id);

--
-- Name: media_asset_copies_store_rel_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX media_asset_copies_store_rel_path_idx ON public.media_asset_copies USING btree (store, rel_path);

--
-- Name: media_asset_transfer_abandoned_uploads_store_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX media_asset_transfer_abandoned_uploads_store_idx ON public.media_asset_transfer_abandoned_uploads USING btree (store, id);

--
-- Name: media_assets_recording_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX media_assets_recording_id_idx ON public.media_assets USING btree (recording_id);

--
-- Name: record_sync_recording_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX record_sync_recording_id_idx ON public.record_sync USING btree (recording_id);

--
-- Name: record_sync_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX record_sync_status_idx ON public.record_sync USING btree (status);

--
-- Name: recording_playback_positions_updated_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recording_playback_positions_updated_at_idx ON public.recording_playback_positions USING btree (updated_at DESC, recording_id DESC);

--
-- Name: recordings_broadcast_event_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_broadcast_event_idx ON public.recordings USING btree (network_id, service_id, program_start_at);

--
-- Name: recordings_deleted_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_deleted_at_idx ON public.recordings USING btree (deleted_at) WHERE (deleted_at IS NOT NULL);

--
-- Name: recordings_description_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_description_trgm ON public.recordings USING gin (public.normalize_search_text(description) public.gin_trgm_ops);

--
-- Name: recordings_genre_lv1_gin; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_genre_lv1_gin ON public.recordings USING gin (genre_lv1);

--
-- Name: recordings_network_id_service_id_event_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_network_id_service_id_event_id_idx ON public.recordings USING btree (network_id, service_id, event_id);

--
-- Name: recordings_program_start_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_program_start_at_id_idx ON public.recordings USING btree (program_start_at DESC, id DESC);

--
-- Name: recordings_purged_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_purged_at_idx ON public.recordings USING btree (purged_at) WHERE (purged_at IS NULL);

--
-- Name: recordings_rule_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_rule_id_idx ON public.recordings USING btree (rule_id) WHERE (rule_id IS NOT NULL);

--
-- Name: recordings_title_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_title_trgm ON public.recordings USING gin (public.normalize_search_text(title) public.gin_trgm_ops);

--
-- Name: recordings_unique_active_event; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX recordings_unique_active_event ON public.recordings USING btree (site, network_id, service_id, event_id, program_start_at) WHERE ((deleted_at IS NULL) AND (superseded_at IS NULL));

--
-- Name: reservations_rule_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX reservations_rule_id_idx ON public.reservations USING btree (rule_id);

--
-- Name: circuit_breakers circuit_breakers_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER circuit_breakers_notify AFTER INSERT OR DELETE OR UPDATE ON public.circuit_breakers FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('breakers');

--
-- Name: label_rules label_rules_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER label_rules_notify AFTER INSERT OR DELETE OR UPDATE ON public.label_rules FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('recordings');

--
-- Name: media_asset_copy_transfer_progress media_asset_copy_transfer_progress_abandon_upload; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER media_asset_copy_transfer_progress_abandon_upload BEFORE DELETE ON public.media_asset_copy_transfer_progress FOR EACH ROW EXECUTE FUNCTION public.media_asset_transfer_progress_abandon_upload();

--
-- Name: media_assets media_assets_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER media_assets_notify AFTER INSERT OR DELETE OR UPDATE ON public.media_assets FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('recordings');

--
-- Name: program_intents program_intents_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER program_intents_notify AFTER INSERT OR DELETE OR UPDATE ON public.program_intents FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('reservations');

--
-- Name: program_overrides program_overrides_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER program_overrides_notify AFTER INSERT OR DELETE OR UPDATE ON public.program_overrides FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('reservations');

--
-- Name: recording_ingest_progress recording_ingest_progress_generation_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER recording_ingest_progress_generation_notify AFTER UPDATE OF temp_generation ON public.recording_ingest_progress FOR EACH ROW WHEN ((old.temp_generation IS DISTINCT FROM new.temp_generation)) EXECUTE FUNCTION public.rokuban_notify('recordings');

--
-- Name: recording_ingest_progress recording_ingest_progress_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER recording_ingest_progress_notify AFTER INSERT OR DELETE ON public.recording_ingest_progress FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('recordings');

--
-- Name: recordings recordings_label_rule_sync_insert; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER recordings_label_rule_sync_insert AFTER INSERT ON public.recordings FOR EACH ROW EXECUTE FUNCTION public.recordings_label_rule_sync();

--
-- Name: recordings recordings_label_rule_sync_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER recordings_label_rule_sync_update AFTER UPDATE OF title ON public.recordings FOR EACH ROW WHEN ((old.title IS DISTINCT FROM new.title)) EXECUTE FUNCTION public.recordings_label_rule_sync();

--
-- Name: recordings recordings_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER recordings_notify AFTER INSERT OR DELETE OR UPDATE ON public.recordings FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('recordings');

--
-- Name: reservations reservations_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER reservations_notify AFTER INSERT OR DELETE OR UPDATE ON public.reservations FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('reservations');

--
-- Name: rules rules_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER rules_notify AFTER INSERT OR DELETE OR UPDATE ON public.rules FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('rules');

--
-- Name: cm_logo_candidates cm_logo_candidates_network_id_service_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cm_logo_candidates
    ADD CONSTRAINT cm_logo_candidates_network_id_service_id_fkey FOREIGN KEY (network_id, service_id) REFERENCES public.cm_logo_areas(network_id, service_id) ON DELETE CASCADE;

--
-- Name: cm_logo_candidates cm_logo_candidates_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cm_logo_candidates
    ADD CONSTRAINT cm_logo_candidates_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE SET NULL;

--
-- Name: cm_logos cm_logos_learned_from_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cm_logos
    ADD CONSTRAINT cm_logos_learned_from_fkey FOREIGN KEY (learned_from) REFERENCES public.recordings(id) ON DELETE SET NULL;

--
-- Name: drop_positions drop_positions_media_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.drop_positions
    ADD CONSTRAINT drop_positions_media_asset_id_fkey FOREIGN KEY (media_asset_id) REFERENCES public.media_assets(id);

--
-- Name: drop_stats drop_stats_media_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.drop_stats
    ADD CONSTRAINT drop_stats_media_asset_id_fkey FOREIGN KEY (media_asset_id) REFERENCES public.media_assets(id);

--
-- Name: encoded_asset_rebuild_requests encoded_asset_rebuild_requests_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.encoded_asset_rebuild_requests
    ADD CONSTRAINT encoded_asset_rebuild_requests_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: encoded_asset_removal_requests encoded_asset_removal_requests_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.encoded_asset_removal_requests
    ADD CONSTRAINT encoded_asset_removal_requests_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: label_rule_hits label_rule_hits_label_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.label_rule_hits
    ADD CONSTRAINT label_rule_hits_label_rule_id_fkey FOREIGN KEY (label_rule_id) REFERENCES public.label_rules(id) ON DELETE CASCADE;

--
-- Name: label_rule_hits label_rule_hits_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.label_rule_hits
    ADD CONSTRAINT label_rule_hits_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: media_asset_copies media_asset_copies_media_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_copies
    ADD CONSTRAINT media_asset_copies_media_asset_id_fkey FOREIGN KEY (media_asset_id) REFERENCES public.media_assets(id) ON DELETE CASCADE;

--
-- Name: media_asset_copy_transfer_progress media_asset_copy_transfer_progress_copy_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_copy_transfer_progress
    ADD CONSTRAINT media_asset_copy_transfer_progress_copy_id_fkey FOREIGN KEY (copy_id) REFERENCES public.media_asset_copies(id) ON DELETE CASCADE;

--
-- Name: media_asset_cuts media_asset_cuts_media_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_cuts
    ADD CONSTRAINT media_asset_cuts_media_asset_id_fkey FOREIGN KEY (media_asset_id) REFERENCES public.media_assets(id) ON DELETE CASCADE;

--
-- Name: media_asset_thumbnail_seeks media_asset_thumbnail_seeks_media_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_thumbnail_seeks
    ADD CONSTRAINT media_asset_thumbnail_seeks_media_asset_id_fkey FOREIGN KEY (media_asset_id) REFERENCES public.media_assets(id) ON DELETE CASCADE;

--
-- Name: media_asset_transfer_attempts media_asset_transfer_attempts_copy_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_transfer_attempts
    ADD CONSTRAINT media_asset_transfer_attempts_copy_id_fkey FOREIGN KEY (copy_id) REFERENCES public.media_asset_copies(id) ON DELETE CASCADE;

--
-- Name: media_asset_transfer_objects media_asset_transfer_objects_copy_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_transfer_objects
    ADD CONSTRAINT media_asset_transfer_objects_copy_id_fkey FOREIGN KEY (copy_id) REFERENCES public.media_asset_copies(id) ON DELETE CASCADE;

--
-- Name: media_asset_ts_scans media_asset_ts_scans_media_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_asset_ts_scans
    ADD CONSTRAINT media_asset_ts_scans_media_asset_id_fkey FOREIGN KEY (media_asset_id) REFERENCES public.media_assets(id) ON DELETE CASCADE;

--
-- Name: media_assets media_assets_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_assets
    ADD CONSTRAINT media_assets_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id);

--
-- Name: missing_media_assets missing_media_assets_media_asset_copy_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.missing_media_assets
    ADD CONSTRAINT missing_media_assets_media_asset_copy_id_fkey FOREIGN KEY (media_asset_copy_id) REFERENCES public.media_asset_copies(id) ON DELETE CASCADE;

--
-- Name: program_intents program_intents_program_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.program_intents
    ADD CONSTRAINT program_intents_program_fkey FOREIGN KEY (site, program_id) REFERENCES public.program_snapshots(site, program_id) ON DELETE CASCADE;

--
-- Name: program_overrides program_overrides_program_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.program_overrides
    ADD CONSTRAINT program_overrides_program_fkey FOREIGN KEY (site, program_id) REFERENCES public.program_snapshots(site, program_id) ON DELETE CASCADE;

--
-- Name: record_sync record_sync_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.record_sync
    ADD CONSTRAINT record_sync_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE SET NULL;

--
-- Name: recording_chapter_ownership recording_chapter_ownership_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_chapter_ownership
    ADD CONSTRAINT recording_chapter_ownership_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: recording_chapter_spans recording_chapter_spans_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_chapter_spans
    ADD CONSTRAINT recording_chapter_spans_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recording_chapter_ownership(recording_id) ON DELETE CASCADE;

--
-- Name: recording_cm_attempts recording_cm_attempts_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_cm_attempts
    ADD CONSTRAINT recording_cm_attempts_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: recording_cm_detections recording_cm_detections_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_cm_detections
    ADD CONSTRAINT recording_cm_detections_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: recording_encode_attempts recording_encode_attempts_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_encode_attempts
    ADD CONSTRAINT recording_encode_attempts_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: recording_encode_policy recording_encode_policy_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_encode_policy
    ADD CONSTRAINT recording_encode_policy_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: recording_ingest_checkpoints recording_ingest_checkpoints_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_ingest_checkpoints
    ADD CONSTRAINT recording_ingest_checkpoints_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: recording_ingest_progress recording_ingest_progress_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_ingest_progress
    ADD CONSTRAINT recording_ingest_progress_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: recording_playback_positions recording_playback_positions_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_playback_positions
    ADD CONSTRAINT recording_playback_positions_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id);

--
-- Name: recording_purge_requests recording_purge_requests_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_purge_requests
    ADD CONSTRAINT recording_purge_requests_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;

--
-- Name: recording_watched recording_watched_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_watched
    ADD CONSTRAINT recording_watched_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id);

--
-- Name: recordings recordings_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recordings
    ADD CONSTRAINT recordings_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.rules(id) ON DELETE SET NULL;

--
-- Name: reservations reservations_program_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_program_fkey FOREIGN KEY (site, program_id) REFERENCES public.program_snapshots(site, program_id) ON DELETE CASCADE;

--
-- Name: reservations reservations_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.rules(id) ON DELETE SET NULL;

--
-- Name: rule_channel_types rule_channel_types_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_channel_types
    ADD CONSTRAINT rule_channel_types_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.rules(id) ON DELETE CASCADE;

--
-- Name: rule_genres rule_genres_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_genres
    ADD CONSTRAINT rule_genres_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.rules(id) ON DELETE CASCADE;

--
-- Name: rule_services rule_services_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_services
    ADD CONSTRAINT rule_services_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.rules(id) ON DELETE CASCADE;

--
-- Name: rule_sites rule_sites_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_sites
    ADD CONSTRAINT rule_sites_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.rules(id) ON DELETE CASCADE;

--
-- Name: rule_text_matches rule_text_matches_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_text_matches
    ADD CONSTRAINT rule_text_matches_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.rules(id) ON DELETE CASCADE;

--
-- Name: rule_times rule_times_rule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rule_times
    ADD CONSTRAINT rule_times_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES public.rules(id) ON DELETE CASCADE;

--
-- PostgreSQL database dump complete
--

