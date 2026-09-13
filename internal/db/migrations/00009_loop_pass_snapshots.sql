-- +goose Up

-- ruler_pass_snapshots は ruler の site 単位のパスが正常に完了した最後の時刻を
-- 持つ。ruler の導出出力とは寿命も書き手も違うため、reservations には列を足さず
-- 専用の衛星表にする。
CREATE TABLE public.ruler_pass_snapshots (
    site            text PRIMARY KEY,
    last_success_at timestamptz NOT NULL DEFAULT now()
);

-- record_sweep_snapshots は record_sweep の site 単位のパスが正常に完了した最後の
-- 時刻を持つ。record_sync / recordings は watcher の観測結果そのものなので、
-- ループの鮮度をそれらの行の有無から導出しない。
CREATE TABLE public.record_sweep_snapshots (
    site            text PRIMARY KEY,
    last_success_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE public.record_sweep_snapshots;
DROP TABLE public.ruler_pass_snapshots;
