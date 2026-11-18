-- +goose Up

-- loop_success_markers は監視向けの「ループが最後に成功・完走した時刻」を 1 表に持つ。
-- 行は loop と scope の組で識別する。scope は loop ごとに意味が決まる（ruler / record_sweep は site、
-- delete_reconcile は置き場の名前、reconcile_pass は全件パスの名前）。
-- 行の存在は「その scope が一度は成功した」だけを主張する。未成功の行は作らない。
--
-- encode / thumbnail / ts_scan は置き場を持たない。scope に pass 名を入れて loop を reconcile_pass に
-- まとめるので、空文字の番兵は使わない。4 表の行はどれも「成功のたびに上書きされ、次の成功まで残る」
-- 同じ寿命なので、1 表にしても寿命は混ざらない。
--
-- この表の読み手は監視の collector だけで、API・readyz・UI は読まない。ScaledJob の --once で動く
-- プロセスの成功時刻は、この表から常駐プロセスの scrape で読む。
CREATE TABLE public.loop_success_markers (
    loop            text NOT NULL,
    scope           text NOT NULL,
    last_success_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (loop, scope),
    CONSTRAINT loop_success_markers_loop_check CHECK (loop IN ('ruler', 'record_sweep', 'reconcile_pass', 'delete_reconcile'))
);

INSERT INTO public.loop_success_markers (loop, scope, last_success_at)
SELECT 'ruler', site, last_success_at FROM public.ruler_pass_snapshots
UNION ALL
SELECT 'record_sweep', site, last_success_at FROM public.record_sweep_snapshots
UNION ALL
SELECT 'reconcile_pass', pass, last_success_at FROM public.reconcile_pass_snapshots
UNION ALL
SELECT 'delete_reconcile', store, last_success_at FROM public.delete_reconcile_snapshots;

DROP TABLE public.delete_reconcile_snapshots;
DROP TABLE public.reconcile_pass_snapshots;
DROP TABLE public.record_sweep_snapshots;
DROP TABLE public.ruler_pass_snapshots;

-- +goose Down

CREATE TABLE public.ruler_pass_snapshots (
    site            text PRIMARY KEY,
    last_success_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.record_sweep_snapshots (
    site            text PRIMARY KEY,
    last_success_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.reconcile_pass_snapshots (
    pass            text PRIMARY KEY,
    last_success_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.delete_reconcile_snapshots (
    store           text PRIMARY KEY,
    last_success_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO public.ruler_pass_snapshots (site, last_success_at)
SELECT scope, last_success_at FROM public.loop_success_markers WHERE loop = 'ruler';

INSERT INTO public.record_sweep_snapshots (site, last_success_at)
SELECT scope, last_success_at FROM public.loop_success_markers WHERE loop = 'record_sweep';

INSERT INTO public.reconcile_pass_snapshots (pass, last_success_at)
SELECT scope, last_success_at FROM public.loop_success_markers WHERE loop = 'reconcile_pass';

INSERT INTO public.delete_reconcile_snapshots (store, last_success_at)
SELECT scope, last_success_at FROM public.loop_success_markers WHERE loop = 'delete_reconcile';

DROP TABLE public.loop_success_markers;
