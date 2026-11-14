-- +goose Up

-- status で絞る読み手は、status と site で絞る静的クエリ
-- GetUningestedRecordBacklog と、status と recording_id で絞る動的 SQL の EXISTS
-- 2 件である。前者は主キー (site, record_id)、後者は
-- record_sync_recording_id_idx で record_sync を引ける。本番計測では
-- record_sync_status_idx の idx_scan は 0 で、record_sync は 3 行だった。
-- GetUningestedRecordBacklog の実行計画は Seq Scan だった。現行の読み手に
-- 使われていないため削除する。
DROP INDEX public.record_sync_status_idx;

-- +goose Down

CREATE INDEX record_sync_status_idx ON public.record_sync (status);
