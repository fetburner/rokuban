-- +goose Up

-- reconciler の完走時刻を loop_success_markers に置くため、loop の許容値に 'reconciler' を足す。
-- scope は site（1 ジョブが 1 site を処理する）。表の形は変えない。
ALTER TABLE public.loop_success_markers DROP CONSTRAINT loop_success_markers_loop_check;
ALTER TABLE public.loop_success_markers ADD CONSTRAINT loop_success_markers_loop_check
    CHECK (loop IN ('ruler', 'record_sweep', 'reconcile_pass', 'delete_reconcile', 'reconciler'));

-- +goose Down

-- 旧い版の制約は 'reconciler' の行を許さないので、先に消す。行は監視の印で、消えても次の完走で戻る。
DELETE FROM public.loop_success_markers WHERE loop = 'reconciler';

ALTER TABLE public.loop_success_markers DROP CONSTRAINT loop_success_markers_loop_check;
ALTER TABLE public.loop_success_markers ADD CONSTRAINT loop_success_markers_loop_check
    CHECK (loop IN ('ruler', 'record_sweep', 'reconcile_pass', 'delete_reconcile'));
