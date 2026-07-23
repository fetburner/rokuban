-- +goose Up

-- reservations: 予約（desired state）
CREATE TABLE reservations (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    site                text   NOT NULL,
    program_id          bigint NOT NULL,
    source              text   NOT NULL CHECK (source IN ('rule', 'manual')),
    rule_id             bigint,
    state               text   NOT NULL DEFAULT 'active'
                               CHECK (state IN ('active', 'detached', 'orphaned')),
    base                jsonb,
    overrides           jsonb  NOT NULL DEFAULT '{}',
    title               text   NOT NULL DEFAULT '',
    program_start_at    timestamptz NOT NULL,
    program_duration_ms bigint NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (site, program_id)
);

CREATE INDEX ON reservations (state);
CREATE INDEX ON reservations (rule_id);

-- schedule_sync: mirakc schedule の観測（observed state）
CREATE TABLE schedule_sync (
    site           text   NOT NULL,
    program_id     bigint NOT NULL,
    reservation_id bigint REFERENCES reservations (id) ON DELETE SET NULL,
    state          text   NOT NULL,
    options        jsonb  NOT NULL,
    tags           text[] NOT NULL DEFAULT '{}',
    failed_reason  jsonb,
    observed_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);

CREATE INDEX ON schedule_sync (reservation_id);

-- recordings: 録画履歴（永続資産）
CREATE TABLE recordings (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    reservation_id      bigint REFERENCES reservations (id) ON DELETE SET NULL,
    rule_id             bigint,
    source              text   NOT NULL CHECK (source IN ('rule', 'manual')),
    site                text   NOT NULL,
    network_id          integer NOT NULL,
    service_id          integer NOT NULL,
    event_id            integer NOT NULL,
    service_name        text    NOT NULL,
    channel_type        text    NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    channel             text    NOT NULL,
    title               text    NOT NULL DEFAULT '',
    description         text,
    extended            jsonb,
    genres              jsonb,
    is_free             boolean NOT NULL DEFAULT true,
    program_start_at    timestamptz NOT NULL,
    program_duration_ms bigint NOT NULL,
    status              text NOT NULL CHECK (status IN ('recording', 'finished', 'failed')),
    started_at          timestamptz,
    ended_at            timestamptz,
    keep_original       text NOT NULL DEFAULT 'always'
                             CHECK (keep_original IN ('always', 'until_encoded')),
    encode_profiles     text[] NOT NULL DEFAULT '{}',
    quality_events      jsonb NOT NULL DEFAULT '[]',
    deleted_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON recordings (reservation_id);
CREATE INDEX ON recordings (program_start_at DESC);
CREATE INDEX ON recordings (network_id, service_id, event_id);
CREATE INDEX ON recordings (deleted_at) WHERE deleted_at IS NOT NULL;

-- record_sync: mirakc record の観測（observed state）
CREATE TABLE record_sync (
    site           text   NOT NULL,
    record_id      text   NOT NULL,
    recording_id   bigint REFERENCES recordings (id) ON DELETE SET NULL,
    program_id     bigint NOT NULL,
    status         text   NOT NULL,
    content_path   text,
    content_length bigint,
    tags           text[] NOT NULL DEFAULT '{}',
    failed_reason  jsonb,
    observed_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, record_id)
);

CREATE INDEX ON record_sync (recording_id);
CREATE INDEX ON record_sync (status);

-- media_assets: メディアアセット（永続資産）
CREATE TABLE media_assets (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    recording_id bigint NOT NULL REFERENCES recordings (id),
    kind         text   NOT NULL CHECK (kind IN ('original', 'encoded', 'thumbnail')),
    profile      text,
    CHECK ((kind = 'encoded') = (profile IS NOT NULL)),
    rel_path     text   NOT NULL,
    size_bytes   bigint NOT NULL,
    state        text NOT NULL DEFAULT 'active'
                      CHECK (state IN ('active', 'deleting', 'deleted')),
    deleted_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (recording_id, kind, profile)
);

CREATE INDEX ON media_assets (recording_id);
CREATE INDEX ON media_assets (kind, state);
CREATE UNIQUE INDEX ON media_assets (rel_path) WHERE state <> 'deleted';

-- drop_stats: PID 別ドロップ統計
CREATE TABLE drop_stats (
    media_asset_id bigint  NOT NULL REFERENCES media_assets (id),
    pid            integer NOT NULL,
    packets        bigint  NOT NULL,
    drops          bigint  NOT NULL,
    errors         bigint  NOT NULL,
    scrambled      bigint  NOT NULL,
    PRIMARY KEY (media_asset_id, pid)
);

-- +goose Down
DROP TABLE IF EXISTS drop_stats;
DROP TABLE IF EXISTS media_assets;
DROP TABLE IF EXISTS record_sync;
DROP TABLE IF EXISTS recordings;
DROP TABLE IF EXISTS schedule_sync;
DROP TABLE IF EXISTS reservations;
