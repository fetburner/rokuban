-- issue #101 決定: program_snapshots のチャンネル識別 6 列
-- (network_id / service_id / channel_type / channel / event_id / service_name)
-- を NOT NULL 化する。
--
-- nullable だった理由（00017 / 00025 のコメント）は「00009 以前の残骸を
-- 救えず nullable のままの行がありうる」「射影から既に消えていて backfill
-- できなかった行がありうる」だったが、この理由は行の寿命によって失効している:
--
--   * この表の行寿命は放送 + epg.retention_grace（既定 24h）。移行時に NULL
--     だった行はとっくに GC 済み（DeleteEndedProgramSnapshots が
--     start_at + duration_ms < now() - retention_grace で刈る）
--   * 新規書き込みの 2 経路 --- api の GetProgramSnapshotSource、ruler の
--     UpsertProgramSnapshotsFromProjection（internal/db/queries/epg.sql /
--     program_snapshots.sql）--- はどちらも epg_programs と epg_services への
--     INNER JOIN で、6 列すべての出所の列（epg_services.network_id /
--     service_id / channel_type / channel / name、epg_programs.event_id。
--     00004_epg.sql）が NOT NULL なので、NULL を書く経路が存在しない
--
-- したがって「移行時の残骸だけが NULL」という構造は 4 列（#27。00017）と
-- 2 列（#98。00025）のどちらにも等しく当てはまる。
--
-- program_snapshots は導出テーブル（reservations と同じく ruler / api が
-- EPG 射影から毎回作り直せる。CLAUDE.md 不変条件 11）なので、この churn は
-- ほぼ無害。00008 / 00010 / 00012 / 00017 と同じく Down を片道にする。
--
-- +goose Up

-- NULL 行の DELETE。program_snapshots への FK は reservations /
-- program_intents / program_overrides が ON DELETE CASCADE で持つため、
-- 対象行に予約・意図・上書きがあれば巻き添えで消える。実運用では上記の
-- 理由により対象 0 行のはずだが、経路としては存在するので件数をログに残す。
-- +goose StatementBegin
DO $$
DECLARE
    deleted_count integer;
BEGIN
    DELETE FROM program_snapshots
    WHERE network_id IS NULL
       OR service_id IS NULL
       OR channel_type IS NULL
       OR channel IS NULL
       OR event_id IS NULL
       OR service_name IS NULL;
    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RAISE NOTICE
        '00026_program_snapshots_channel_not_null: deleted % program_snapshots row(s) with a NULL channel/event identity column (cascades to reservations/program_intents/program_overrides via FK)',
        deleted_count;
END;
$$;
-- +goose StatementEnd

ALTER TABLE program_snapshots
    ALTER COLUMN network_id   SET NOT NULL,
    ALTER COLUMN service_id   SET NOT NULL,
    ALTER COLUMN channel_type SET NOT NULL,
    ALTER COLUMN channel      SET NOT NULL,
    ALTER COLUMN event_id     SET NOT NULL,
    ALTER COLUMN service_name SET NOT NULL;

-- CHECK は「NULL または列挙」から列挙だけに単純化する。NULL 分岐が
-- 表現可能である必要がなくなったため（不変条件 10「あってはいけない組み合わせは
-- 表現不可能にする」の裏返し --- ここでは「あり得る組み合わせ」を CHECK の
-- 表現力から削るだけで安全に狭くできる）。
ALTER TABLE program_snapshots
    DROP CONSTRAINT program_snapshots_channel_type_check,
    ADD CONSTRAINT program_snapshots_channel_type_check
        CHECK (channel_type IN ('GR', 'BS', 'CS', 'SKY'));

-- +goose Down

-- **片道であることを明示的に許容する**（導出テーブルなので再構築できる。
-- CLAUDE.md 不変条件 11、00008/00010/00012/00017/00025 と同じ前例）。
--
-- 復元できないものは 1 つ: Up で DELETE した NULL 行そのもの（実運用では
-- 0 行のはずだが、開発環境で NULL 行を作ってから Up した場合は失われる）。
-- 逆方向の backfill は書かない --- 消えた理由は「識別できない残骸」だった
-- ので、Down しても NULL を復元する元データが無い。

ALTER TABLE program_snapshots
    DROP CONSTRAINT program_snapshots_channel_type_check,
    ADD CONSTRAINT program_snapshots_channel_type_check
        CHECK (channel_type IS NULL OR channel_type IN ('GR', 'BS', 'CS', 'SKY'));

ALTER TABLE program_snapshots
    ALTER COLUMN network_id   DROP NOT NULL,
    ALTER COLUMN service_id   DROP NOT NULL,
    ALTER COLUMN channel_type DROP NOT NULL,
    ALTER COLUMN channel      DROP NOT NULL,
    ALTER COLUMN event_id     DROP NOT NULL,
    ALTER COLUMN service_name DROP NOT NULL;
