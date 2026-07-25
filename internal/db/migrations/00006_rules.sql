-- +goose Up

-- M2-1: rules スキーマ（issue #3 / #24）
-- 条件とオプションは型付き列 + 子テーブル。jsonb は metadata のみ。

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- encode_profiles の正規集合チェック（重複・空文字・NULL・非正規順を拒否）
CREATE FUNCTION array_is_canonical_set(a text[]) RETURNS boolean
IMMUTABLE STRICT LANGUAGE sql AS $$
  SELECT array_position(a, NULL) IS NULL
     AND NOT ('' = ANY(a))
     AND a = (SELECT coalesce(array_agg(DISTINCT x ORDER BY x), '{}') FROM unnest(a) x)
$$;

CREATE TABLE rules (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name              text    NOT NULL,
    description       text    NOT NULL DEFAULT '',
    enabled           boolean NOT NULL DEFAULT true,
    priority          integer NOT NULL DEFAULT 10,
    -- 条件のうち単一値（NULL = 問わない）
    is_free           boolean,
    duration_min_ms   bigint,
    duration_max_ms   bigint,
    period_start_at   timestamptz,
    period_end_at     timestamptz,
    -- 重複排除（M2-6 で使用。列は M2-1 で確保）
    dedupe_enabled    boolean NOT NULL DEFAULT false,
    dedupe_threshold  real,
    dedupe_window     interval,
    -- base の材料
    keep_original     text    NOT NULL DEFAULT 'always'
                              CHECK (keep_original IN ('always', 'until_encoded')),
    encode_profiles   text[]  NOT NULL DEFAULT '{}'
                              CHECK (array_is_canonical_set(encode_profiles)),
    filename_template text    NOT NULL DEFAULT '',
    metadata          jsonb   NOT NULL DEFAULT '{}',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (duration_min_ms IS NULL OR duration_max_ms IS NULL
           OR duration_min_ms <= duration_max_ms),
    CHECK (dedupe_enabled = false OR dedupe_threshold IS NOT NULL),
    CHECK (keep_original <> 'until_encoded' OR cardinality(encode_profiles) > 0)
);

-- テキスト条件（キーワード / 正規表現）。seq は評価順・表示順。
-- target: name / description / extended
-- mode: keyword（部分一致）/ regex（POSIX ARE ~）
CREATE TABLE rule_text_matches (
    rule_id         bigint  NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    seq             integer NOT NULL CHECK (seq >= 0),
    target          text    NOT NULL CHECK (target IN ('name', 'description', 'extended')),
    mode            text    NOT NULL CHECK (mode IN ('keyword', 'regex')),
    value           text    NOT NULL CHECK (value <> ''),
    case_sensitive  boolean NOT NULL DEFAULT false,
    negate          boolean NOT NULL DEFAULT false,
    PRIMARY KEY (rule_id, seq)
);

CREATE TABLE rule_services (
    rule_id    bigint  NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    network_id integer NOT NULL,
    service_id integer NOT NULL,
    PRIMARY KEY (rule_id, network_id, service_id)
);

CREATE TABLE rule_channel_types (
    rule_id       bigint NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    channel_type  text   NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    PRIMARY KEY (rule_id, channel_type)
);

CREATE TABLE rule_genres (
    rule_id   bigint   NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    genre_lv1 smallint NOT NULL CHECK (genre_lv1 BETWEEN 0 AND 15),
    PRIMARY KEY (rule_id, genre_lv1)
);

-- weekdays: bit0=月 … bit6=日（1..127）。start_sec/end_sec は 0..86400（end は翌日跨ぎ可）
CREATE TABLE rule_times (
    rule_id    bigint  NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    seq        integer NOT NULL CHECK (seq >= 0),
    weekdays   integer NOT NULL CHECK (weekdays BETWEEN 1 AND 127),
    start_sec  integer NOT NULL CHECK (start_sec BETWEEN 0 AND 86400),
    end_sec    integer NOT NULL CHECK (end_sec BETWEEN 0 AND 86400),
    PRIMARY KEY (rule_id, seq)
);

-- 指定なし = 全サイト。site は設定レジストリ由来（FK なし）
CREATE TABLE rule_sites (
    rule_id bigint NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    site    text   NOT NULL CHECK (site <> ''),
    PRIMARY KEY (rule_id, site)
);

-- 導出トレーサビリティ（ruler が毎パス書き換え）。両側 CASCADE
CREATE TABLE reservation_rule_matches (
    reservation_id bigint NOT NULL REFERENCES reservations (id) ON DELETE CASCADE,
    rule_id        bigint NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
    PRIMARY KEY (reservation_id, rule_id)
);

CREATE INDEX ON reservation_rule_matches (rule_id);

-- M1 で確保した rule_id に FK を付ける（削除時は参照を NULL に）
ALTER TABLE reservations
    ADD CONSTRAINT reservations_rule_id_fkey
    FOREIGN KEY (rule_id) REFERENCES rules (id) ON DELETE SET NULL;

ALTER TABLE recordings
    ADD CONSTRAINT recordings_rule_id_fkey
    FOREIGN KEY (rule_id) REFERENCES rules (id) ON DELETE SET NULL;

-- SSE ヒント（rules 変更で UI を invalidate）
CREATE TRIGGER rules_notify
    AFTER INSERT OR UPDATE OR DELETE ON rules
    FOR EACH ROW EXECUTE FUNCTION rokuban_notify('rules');

-- +goose Down

DROP TRIGGER IF EXISTS rules_notify ON rules;

ALTER TABLE recordings DROP CONSTRAINT IF EXISTS recordings_rule_id_fkey;
ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_rule_id_fkey;

DROP TABLE IF EXISTS reservation_rule_matches;
DROP TABLE IF EXISTS rule_sites;
DROP TABLE IF EXISTS rule_times;
DROP TABLE IF EXISTS rule_genres;
DROP TABLE IF EXISTS rule_channel_types;
DROP TABLE IF EXISTS rule_services;
DROP TABLE IF EXISTS rule_text_matches;
DROP TABLE IF EXISTS rules;

DROP FUNCTION IF EXISTS array_is_canonical_set(text[]);

-- pg_trgm は他用途でも使う可能性があるので Down では残す
