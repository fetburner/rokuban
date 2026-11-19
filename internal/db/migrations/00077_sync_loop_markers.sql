-- +goose Up

-- EPG 同期・チューナー同期・物理転送の成功時刻を loop_success_markers に置くため、loop の許容値を増やす。
-- ScaledJob で動くこれらの書き手は常駐プロセスのゲージに値を残せないので、監視は DB の印を読む。
-- scope の決め方:
--   epg_sync / tuner_sync は site（1 ジョブが 1 site を処理する）。EPG のラベルなし系列は、
--     設定された site の行の最大値として読む（最新の site の時刻）。
--   storage_transfer は全置き場で 1 行（scope = 'all'）。系列がラベルを持たず、どの置き場の成功も
--     同じ「最後に成功した物理コピー」なので、置き場の一覧を読み側に持たせる必要がない。
-- 物理転送の書き込みは公開の確定（media_asset_copies の active 化）と同じトランザクションで行う。
-- 別の文で書くと、active 化の後・印の書き込みの前に落ちたとき、再試行は no-op になって時刻を失う。
ALTER TABLE public.loop_success_markers DROP CONSTRAINT loop_success_markers_loop_check;
ALTER TABLE public.loop_success_markers ADD CONSTRAINT loop_success_markers_loop_check
    CHECK (loop IN ('ruler', 'record_sweep', 'reconcile_pass', 'delete_reconcile', 'reconciler', 'epg_sync', 'tuner_sync', 'storage_transfer'));

-- +goose Down

-- 旧い版の制約は新しい loop の行を許さないので、先に消す。行は監視の印で、消えても次の成功で戻る。
DELETE FROM public.loop_success_markers WHERE loop IN ('epg_sync', 'tuner_sync', 'storage_transfer');

ALTER TABLE public.loop_success_markers DROP CONSTRAINT loop_success_markers_loop_check;
ALTER TABLE public.loop_success_markers ADD CONSTRAINT loop_success_markers_loop_check
    CHECK (loop IN ('ruler', 'record_sweep', 'reconcile_pass', 'delete_reconcile', 'reconciler'));
