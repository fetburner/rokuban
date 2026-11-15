-- +goose Up
-- 旧 worker のクエリは旧列名 generation を参照するため、改名後は失敗する。
-- 適用前に旧 worker をすべて停止し、移行後は新版だけを起動する。
ALTER TABLE media_asset_copy_transfer_progress RENAME COLUMN generation TO writer_generation;
ALTER TABLE media_asset_copy_transfer_progress RENAME CONSTRAINT media_asset_copy_transfer_progress_generation_check TO media_asset_copy_transfer_progress_writer_generation_check;

-- +goose Down
ALTER TABLE media_asset_copy_transfer_progress RENAME CONSTRAINT media_asset_copy_transfer_progress_writer_generation_check TO media_asset_copy_transfer_progress_generation_check;
ALTER TABLE media_asset_copy_transfer_progress RENAME COLUMN writer_generation TO generation;
