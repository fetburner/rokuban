-- +goose Up

-- EPG プロジェクションは「使い捨てキャッシュ」。真実は常に mirakc であり、
-- レベルトリガーでいつでも全量再構築できる（issue #3）。
-- 永続資産（reservations / recordings / media_assets）とは寿命が違うため
-- スキーマ v1 から分離し、ローリングウィンドウで刈り取る。

-- pg_trgm: 番組名・説明の部分一致と正規表現マッチを GIN で加速する（issue #3）
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- epg_services: サービス（チャンネル）のプロジェクション
CREATE TABLE epg_services (
    site                  text    NOT NULL,
    network_id            integer NOT NULL,
    service_id            integer NOT NULL,
    type                  integer NOT NULL,
    logo_id               integer NOT NULL,
    remote_control_key_id integer NOT NULL,
    name                  text    NOT NULL,
    channel_type          text    NOT NULL CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY')),
    channel               text    NOT NULL,
    has_logo_data         boolean NOT NULL DEFAULT false,
    observed_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, network_id, service_id)
);

-- リモコン番号順のチャンネル並びは UI の既定ソート
CREATE INDEX ON epg_services (site, channel_type, remote_control_key_id);

-- epg_programs: 番組のプロジェクション（UI 完全形）
-- クエリ軸（サービス / 時間範囲 / ジャンル / 無料）は型付きカラム、
-- 詳細ペイロードは jsonb（issue #3 の線引き）。
CREATE TABLE epg_programs (
    site        text    NOT NULL,
    program_id  bigint  NOT NULL,
    network_id  integer NOT NULL,
    service_id  integer NOT NULL,
    event_id    integer NOT NULL,
    start_at    timestamptz NOT NULL,
    duration_ms bigint  NOT NULL,
    -- end_at は start_at + duration_ms。timestamptz + interval が STABLE のため
    -- 生成列にできず、同期時にアプリが計算して書く。刈り取りとグリッド描画の軸。
    end_at      timestamptz NOT NULL,
    is_free     boolean NOT NULL DEFAULT true,
    name        text    NOT NULL DEFAULT '',
    description text    NOT NULL DEFAULT '',
    -- genre_lv1 はジャンル絞り込みのクエリ軸。詳細（lv2 / un1 / un2）は genres jsonb 側。
    genre_lv1   smallint[] NOT NULL DEFAULT '{}',
    extended    jsonb,
    genres      jsonb,
    video       jsonb,
    audios      jsonb,
    observed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site, program_id)
);

-- 番組表グリッド・サービス別一覧
CREATE INDEX ON epg_programs (site, network_id, service_id, start_at);
-- 時間窓での横断取得と、放送済み番組の刈り取り
CREATE INDEX ON epg_programs (site, start_at);
CREATE INDEX ON epg_programs (end_at);
CREATE INDEX ON epg_programs USING GIN (genre_lv1);
-- 部分一致・正規表現検索（UI 検索と M2 の ruler 評価で同一エンジンを使う）
CREATE INDEX ON epg_programs USING GIN (name gin_trgm_ops);
CREATE INDEX ON epg_programs USING GIN (description gin_trgm_ops);

-- observed_at には意図的にインデックスを張らない。毎パスで全行が更新される列なので、
-- インデックスがあると HOT update が効かなくなりブロートが悪化する。stale スイープの
-- seq scan より、更新経路を HOT に保つ方が churn の総量では有利。

-- 全量 upsert を繰り返すため churn が大きい。autovacuum を既定より積極的にする（issue #3）。
ALTER TABLE epg_programs SET (
    autovacuum_vacuum_scale_factor  = 0.05,
    autovacuum_analyze_scale_factor = 0.02
);

-- +goose Down
DROP TABLE IF EXISTS epg_programs;
DROP TABLE IF EXISTS epg_services;
