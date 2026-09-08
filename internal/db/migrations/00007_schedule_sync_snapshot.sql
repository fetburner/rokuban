-- +goose Up

-- schedule_sync_snapshots は schedule_sync のサイト単位の全量観測が、
-- upsert と stale 削除を含めて正常にコミットされた最後の時刻を持つ。
-- schedule_sync の行だけでは、空のスナップショットと観測ループ停止を
-- 区別できないため、サイトごとに 1 行のマーカーを別表で保持する。
CREATE TABLE public.schedule_sync_snapshots (
    site         text PRIMARY KEY,
    snapshot_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE public.schedule_sync_snapshots;
