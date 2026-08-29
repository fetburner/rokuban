-- +goose Up

-- baseline: 旧 00001〜00041（40 本、issue #435）を 1 本に集約したスキーマ v1 の
-- 最終形。運用中の DB がまだ無く（このリポジトリで一度も本番稼働していない）、
-- goose の版管理表を持つ既存 DB も無い時点でのみ可能な集約であり、以後は
-- 通常どおり増分マイグレーションを積む。個々の制約・列がなぜその形になったかの
-- 経緯は git log（`internal/db/migrations/` の旧ファイル）と GitHub issue に残る。
--
-- 本体（CREATE TABLE 以下）は `pg_dump --schema-only` で機械的に取り出したもので、
-- 手で書き写していない。goose のバージョン管理表 (goose_db_version) は goose 自身が
-- 作るのでここには含めない。

-- pg_dump の既定どおり、関数本体は作成時に検証しない（trash_deletable_recordings
-- が参照する recordings 等はこの後の CREATE TABLE で作られるため、検証を有効の
-- ままにすると生成順序の都合で存在しないテーブルへの参照として弾かれる）。
SET LOCAL check_function_bodies = false;

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;


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
-- Name: rokuban_notify(); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.rokuban_notify() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('rokuban', TG_ARGV[0]);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd


--
-- Name: trash_deletable_recordings(timestamptz); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.trash_deletable_recordings(grace_cutoff timestamptz) RETURNS TABLE(recording_id bigint)
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


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: circuit_breakers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.circuit_breakers (
    site text NOT NULL,
    name text NOT NULL,
    tripped_at timestamptz DEFAULT now() NOT NULL,
    pending integer NOT NULL,
    threshold integer NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL
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
-- Name: epg_programs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.epg_programs (
    site text NOT NULL,
    program_id bigint NOT NULL,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    event_id integer NOT NULL,
    start_at timestamptz NOT NULL,
    duration_ms bigint NOT NULL,
    end_at timestamptz NOT NULL,
    is_free boolean DEFAULT true NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    genre_lv1 smallint[] DEFAULT '{}'::smallint[] NOT NULL,
    extended jsonb,
    genres jsonb,
    video jsonb,
    audios jsonb,
    observed_at timestamptz DEFAULT now() NOT NULL
)
WITH (autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');


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
    observed_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT epg_services_channel_type_check CHECK ((channel_type = ANY (ARRAY['GR'::text, 'BS'::text, 'CS'::text, 'SKY'::text])))
);


--
-- Name: media_assets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.media_assets (
    id bigint NOT NULL,
    recording_id bigint NOT NULL,
    kind text NOT NULL,
    profile text,
    rel_path text NOT NULL,
    size_bytes bigint NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    deleted_at timestamptz,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT media_assets_check CHECK (((kind = 'encoded'::text) = (profile IS NOT NULL))),
    CONSTRAINT media_assets_kind_check CHECK ((kind = ANY (ARRAY['original'::text, 'encoded'::text, 'thumbnail'::text]))),
    CONSTRAINT media_assets_state_check CHECK ((state = ANY (ARRAY['active'::text, 'deleting'::text, 'deleted'::text])))
);


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
    media_asset_id bigint NOT NULL,
    first_seen timestamptz DEFAULT now() NOT NULL
);


--
-- Name: never_scheduled_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.never_scheduled_events (
    site text NOT NULL,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    event_id integer NOT NULL,
    observed_at timestamptz DEFAULT now() NOT NULL
);


--
-- Name: orphan_files; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.orphan_files (
    rel_path text NOT NULL,
    first_seen timestamptz DEFAULT now() NOT NULL
);


--
-- Name: program_intents; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.program_intents (
    site text NOT NULL,
    program_id bigint NOT NULL,
    action text NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT program_intents_action_check CHECK ((action = ANY (ARRAY['record'::text, 'skip'::text])))
);


--
-- Name: program_overrides; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.program_overrides (
    site text NOT NULL,
    program_id bigint NOT NULL,
    overrides jsonb NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL
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
    start_at timestamptz NOT NULL,
    duration_ms bigint NOT NULL,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    channel_type text NOT NULL,
    channel text NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    event_id integer NOT NULL,
    service_name text NOT NULL,
    CONSTRAINT program_snapshots_channel_type_check CHECK ((channel_type = ANY (ARRAY['GR'::text, 'BS'::text, 'CS'::text, 'SKY'::text])))
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
    observed_at timestamptz DEFAULT now() NOT NULL
);


--
-- Name: recording_encode_attempts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_encode_attempts (
    recording_id bigint NOT NULL,
    profile text NOT NULL,
    state text NOT NULL,
    error text,
    attempted_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT recording_encode_attempts_state_check CHECK ((state = ANY (ARRAY['running'::text, 'failed'::text])))
);


--
-- Name: recording_encode_policy; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_encode_policy (
    recording_id bigint NOT NULL,
    keep_original text NOT NULL,
    encode_profiles text[] NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT recording_encode_policy_check CHECK (((keep_original <> 'until_encoded'::text) OR (cardinality(encode_profiles) > 0))),
    CONSTRAINT recording_encode_policy_keep_original_check CHECK ((keep_original = ANY (ARRAY['always'::text, 'until_encoded'::text])))
);


--
-- Name: recording_ingest_progress; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_ingest_progress (
    recording_id bigint NOT NULL,
    written_bytes bigint NOT NULL,
    expected_bytes bigint,
    observed_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT recording_ingest_progress_expected_bytes_check CHECK (((expected_bytes IS NULL) OR (expected_bytes >= 0))),
    CONSTRAINT recording_ingest_progress_written_bytes_check CHECK ((written_bytes >= 0))
);


--
-- Name: recording_purge_requests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.recording_purge_requests (
    recording_id bigint NOT NULL,
    requested_at timestamptz DEFAULT now() NOT NULL
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
    program_start_at timestamptz NOT NULL,
    program_duration_ms bigint NOT NULL,
    status text NOT NULL,
    started_at timestamptz,
    ended_at timestamptz,
    quality_events jsonb DEFAULT '[]'::jsonb NOT NULL,
    deleted_at timestamptz,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    superseded_at timestamptz,
    purged_at timestamptz,
    genre_lv1 smallint[] GENERATED ALWAYS AS (public.genre_lv1_of(genres)) STORED,
    CONSTRAINT recordings_channel_type_check CHECK ((channel_type = ANY (ARRAY['GR'::text, 'BS'::text, 'CS'::text, 'SKY'::text]))),
    CONSTRAINT recordings_source_check CHECK ((source = ANY (ARRAY['rule'::text, 'manual'::text]))),
    CONSTRAINT recordings_status_check CHECK ((status = ANY (ARRAY['recording'::text, 'finished'::text, 'canceled'::text, 'failed'::text])))
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
-- Name: reservations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.reservations (
    id bigint NOT NULL,
    site text NOT NULL,
    program_id bigint NOT NULL,
    rule_id bigint,
    base jsonb,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    dedup_match_recording_id bigint,
    dedup_similarity real,
    CONSTRAINT reservations_dedup_evidence_check CHECK (((dedup_match_recording_id IS NULL) = (dedup_similarity IS NULL)))
);


--
-- Name: reservations_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.reservations ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.reservations_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
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
    period_start_at timestamptz,
    period_end_at timestamptz,
    dedupe_enabled boolean DEFAULT false NOT NULL,
    dedupe_threshold real,
    dedupe_window interval,
    keep_original text DEFAULT 'always'::text NOT NULL,
    encode_profiles text[] DEFAULT '{}'::text[] NOT NULL,
    filename_template text DEFAULT ''::text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
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
    observed_at timestamptz DEFAULT now() NOT NULL
);


--
-- Name: schema_info; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_info (
    key text NOT NULL,
    value text NOT NULL
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
    observed_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT storage_sync_root_check CHECK ((root = ANY (ARRAY['media'::text, 'scratch'::text])))
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
    observed_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT tuner_sync_types_check CHECK ((types <@ ARRAY['GR'::text, 'BS'::text, 'CS'::text, 'SKY'::text]))
);


--
-- Name: until_encoded_deletable_originals; Type: VIEW; Schema: public; Owner: -
--

CREATE VIEW public.until_encoded_deletable_originals AS
 SELECT a.id AS asset_id,
    a.recording_id,
    a.rel_path,
    a.size_bytes,
    a.state
   FROM ((public.media_assets a
     JOIN public.recordings r ON ((r.id = a.recording_id)))
     JOIN public.recording_encode_policy p ON ((p.recording_id = r.id)))
  WHERE ((a.kind = 'original'::text) AND (p.keep_original = 'until_encoded'::text) AND (r.deleted_at IS NULL) AND (cardinality(p.encode_profiles) > 0) AND (NOT (EXISTS ( SELECT 1
           FROM unnest(p.encode_profiles) want(profile)
          WHERE (NOT (EXISTS ( SELECT 1
                   FROM public.media_assets e
                  WHERE ((e.recording_id = r.id) AND (e.kind = 'encoded'::text) AND (e.state = 'active'::text) AND (e.profile = want.profile)))))))) AND (EXISTS ( SELECT 1
           FROM public.media_assets t
          WHERE ((t.recording_id = r.id) AND (t.kind = 'thumbnail'::text) AND (t.state = 'active'::text)))));


--
-- Name: circuit_breakers circuit_breakers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.circuit_breakers
    ADD CONSTRAINT circuit_breakers_pkey PRIMARY KEY (site, name);


--
-- Name: drop_stats drop_stats_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.drop_stats
    ADD CONSTRAINT drop_stats_pkey PRIMARY KEY (media_asset_id, pid);


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
    ADD CONSTRAINT missing_media_assets_pkey PRIMARY KEY (media_asset_id);


--
-- Name: never_scheduled_events never_scheduled_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.never_scheduled_events
    ADD CONSTRAINT never_scheduled_events_pkey PRIMARY KEY (site, network_id, service_id, event_id);


--
-- Name: orphan_files orphan_files_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.orphan_files
    ADD CONSTRAINT orphan_files_pkey PRIMARY KEY (rel_path);


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
-- Name: record_sync record_sync_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.record_sync
    ADD CONSTRAINT record_sync_pkey PRIMARY KEY (site, record_id);


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
-- Name: recording_ingest_progress recording_ingest_progress_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_ingest_progress
    ADD CONSTRAINT recording_ingest_progress_pkey PRIMARY KEY (recording_id);


--
-- Name: recording_purge_requests recording_purge_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_purge_requests
    ADD CONSTRAINT recording_purge_requests_pkey PRIMARY KEY (recording_id);


--
-- Name: recordings recordings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recordings
    ADD CONSTRAINT recordings_pkey PRIMARY KEY (id);


--
-- Name: reservations reservations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_pkey PRIMARY KEY (id);


--
-- Name: reservations reservations_site_program_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_site_program_id_key UNIQUE (site, program_id);


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
-- Name: schema_info schema_info_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schema_info
    ADD CONSTRAINT schema_info_pkey PRIMARY KEY (key);


--
-- Name: storage_sync storage_sync_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_sync
    ADD CONSTRAINT storage_sync_pkey PRIMARY KEY (root);


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
-- Name: media_assets_kind_state_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX media_assets_kind_state_idx ON public.media_assets USING btree (kind, state);


--
-- Name: media_assets_recording_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX media_assets_recording_id_idx ON public.media_assets USING btree (recording_id);


--
-- Name: media_assets_rel_path_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX media_assets_rel_path_idx ON public.media_assets USING btree (rel_path) WHERE (state <> 'deleted'::text);


--
-- Name: record_sync_recording_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX record_sync_recording_id_idx ON public.record_sync USING btree (recording_id);


--
-- Name: record_sync_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX record_sync_status_idx ON public.record_sync USING btree (status);


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
-- Name: recordings_title_trgm; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX recordings_title_trgm ON public.recordings USING gin (public.normalize_search_text(title) public.gin_trgm_ops);


--
-- Name: recordings_unique_active_event; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX recordings_unique_active_event ON public.recordings USING btree (site, network_id, service_id, event_id) WHERE ((deleted_at IS NULL) AND (superseded_at IS NULL));


--
-- Name: reservations_rule_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX reservations_rule_id_idx ON public.reservations USING btree (rule_id);


--
-- Name: circuit_breakers circuit_breakers_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER circuit_breakers_notify AFTER INSERT OR DELETE OR UPDATE ON public.circuit_breakers FOR EACH ROW EXECUTE FUNCTION public.rokuban_notify('breakers');


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
-- Name: drop_stats drop_stats_media_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.drop_stats
    ADD CONSTRAINT drop_stats_media_asset_id_fkey FOREIGN KEY (media_asset_id) REFERENCES public.media_assets(id);


--
-- Name: media_assets media_assets_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.media_assets
    ADD CONSTRAINT media_assets_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id);


--
-- Name: missing_media_assets missing_media_assets_media_asset_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.missing_media_assets
    ADD CONSTRAINT missing_media_assets_media_asset_id_fkey FOREIGN KEY (media_asset_id) REFERENCES public.media_assets(id) ON DELETE CASCADE;


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
-- Name: recording_encode_attempts recording_encode_attempts_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_encode_attempts
    ADD CONSTRAINT recording_encode_attempts_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;


--
-- Name: recording_encode_policy recording_encode_policy_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_encode_policy
    ADD CONSTRAINT recording_encode_policy_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id);


--
-- Name: recording_ingest_progress recording_ingest_progress_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_ingest_progress
    ADD CONSTRAINT recording_ingest_progress_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;


--
-- Name: recording_purge_requests recording_purge_requests_recording_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.recording_purge_requests
    ADD CONSTRAINT recording_purge_requests_recording_id_fkey FOREIGN KEY (recording_id) REFERENCES public.recordings(id) ON DELETE CASCADE;


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


-- schema_info は現状 1 行だけの定数（互換性を壊す変更を跨いだときの目印）。
-- pg_dump --schema-only はデータを含まないので、この 1 行だけ明示的に INSERT する。
INSERT INTO schema_info (key, value) VALUES ('version', '1');


-- +goose Down

-- 運用中の DB がまだ無いため、Down は「元に戻す」丁寧さを持たない。列単位で
-- 戻さず、このマイグレーションが作ったオブジェクトを丸ごと落とすだけで足りる。
--
-- DROP SCHEMA public CASCADE; CREATE SCHEMA public; は使わない ---
-- goose_db_version は goose 自身がこの public スキーマに作るブックキーピング表
-- なので、CASCADE で一緒に落ちると goose が Down 直後に同じトランザクション内で
-- 試みる「バージョンを記録する」書き込み（DELETE FROM goose_db_version ...）が
-- 「relation "goose_db_version" does not exist」で失敗し、トランザクション全体が
-- ロールバックする（DROP 自体も巻き戻り、何も起きなかったことになる）。実際に
-- 試して確認した崩れ方で、これを避けて goose_db_version 以外だけを落とす。
DROP TABLE IF EXISTS
    circuit_breakers, drop_stats, epg_programs, epg_services, media_assets,
    missing_media_assets, never_scheduled_events, orphan_files, program_intents,
    program_overrides, program_snapshots, record_sync, recording_encode_attempts,
    recording_encode_policy, recording_ingest_progress, recording_purge_requests,
    recordings, reservations, rule_channel_types, rule_genres, rule_services,
    rule_sites, rule_text_matches, rule_times, rules, schedule_sync, schema_info,
    storage_sync, tuner_sync
    CASCADE;

DROP FUNCTION IF EXISTS trash_deletable_recordings(timestamptz);
DROP FUNCTION IF EXISTS rokuban_notify();
DROP FUNCTION IF EXISTS normalize_search_text(text);
DROP FUNCTION IF EXISTS genre_lv1_of(jsonb);
DROP FUNCTION IF EXISTS array_is_canonical_set(text[]);
-- pg_trgm は他用途でも使う可能性があるので Down でも残す。
