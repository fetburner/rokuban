-- +goose Up

-- MarkPurgedRecordings は purge 前のごみ箱行を読み、purged_at IS NULL を条件にする。
-- 2026-11-14 16:40 UTC までの約 53.6 時間の本番統計では、
-- recordings_purged_at_idx と recordings_deleted_at_idx の idx_scan は各 214 で、
-- delete-reconcile の CronJob（*/15）の実行回数と一致した。全 254 行中、未 purge は
-- 177 行、purge 済みは 77 行、ごみ箱未 purge は 15 行だった。MarkPurgedRecordings
-- 相当の SELECT は BitmapAnd（deleted_at 索引 100 行、purged_at 索引 180 行）で
-- 15 行を得ていた（shared hit 71、1.087 ms）。purged_at 索引のキーは述語により
-- 全行 NULL になるが、述語 purged_at IS NULL は MarkPurgedRecordings の
-- r.purged_at IS NULL が含意するため保ち、キーを結合で使う id にする。
-- recordings_deleted_at_idx はこの変更では扱わない。
--
-- ローカルの PostgreSQL 17 で purge 済み 100,000 行のクエリを変更せずに測ると、
-- Index Only Scan using recordings_unpurged_id_idx（0.114 ms、shared hit 537）を選び、
-- 同じ規模の Index Scan using recordings_purged_at_idx（0.114 ms、shared hit 537）と
-- 同等だった。新索引がなく deleted_at キーの部分索引だけがある場合は Seq Scan
-- （8.35 ms、shared hit 2576）だった。
-- purge 済み 77 行と 1,000 行では Seq Scan と Hash Join になり、索引は使われなかった。
-- recordings_unpurged_id_idx はごみ箱一覧（deleted_at IS NOT NULL AND purged_at IS NULL
-- ORDER BY program_start_at DESC, id DESC LIMIT 50）には効かない。本番の実行計画は
-- Seq Scan（0.262 ms）だった。
DROP INDEX public.recordings_purged_at_idx;

CREATE INDEX recordings_unpurged_id_idx ON public.recordings (id)
WHERE purged_at IS NULL;

-- +goose Down

DROP INDEX public.recordings_unpurged_id_idx;

CREATE INDEX recordings_purged_at_idx ON public.recordings (purged_at)
WHERE purged_at IS NULL;
