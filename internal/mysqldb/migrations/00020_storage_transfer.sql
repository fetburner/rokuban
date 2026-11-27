-- +goose Up
-- 転送の回収待ちとバックログの保存表。列と制約は Postgres の最終形に揃える。
CREATE TABLE media_asset_transfer_abandoned_uploads (
    id BIGINT NOT NULL AUTO_INCREMENT,
    store VARCHAR(64) NOT NULL CHECK (store REGEXP '^[a-z0-9-]+$'),
    upload_state LONGBLOB NOT NULL,
    abandoned_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY media_asset_transfer_abandoned_uploads_store_id (store, id)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- 明示 DELETE の全経路を記録するため、進捗表のトリガーで掃除待ちへ移す。
-- この表の外部キーは RESTRICT で、コピー側の DELETE は進捗を先に明示 DELETE する。
-- InnoDB は FK CASCADE による DELETE でトリガーを発火しないため、CASCADE は使わない。
-- +goose StatementBegin
CREATE TRIGGER media_asset_copy_transfer_progress_abandon_upload
BEFORE DELETE ON media_asset_copy_transfer_progress FOR EACH ROW
BEGIN
    IF OLD.upload_state IS NOT NULL THEN
        INSERT INTO media_asset_transfer_abandoned_uploads (store, upload_state)
        VALUES (OLD.upload_store, OLD.upload_state);
    END IF;
END
-- +goose StatementEnd

CREATE TABLE transfer_backlog_snapshots (
    store VARCHAR(64) NOT NULL,
    assets BIGINT NOT NULL CHECK (assets >= 0),
    counted_at DATETIME(6) NOT NULL,
    PRIMARY KEY (store)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- RecordTransferBacklog の tx 単位の直列化に使う固定名の行。
INSERT INTO named_locks (name) VALUES ('transfer_backlog_snapshots');

-- +goose Down
DELETE FROM named_locks WHERE name = 'transfer_backlog_snapshots';
DROP TABLE transfer_backlog_snapshots;
DROP TRIGGER media_asset_copy_transfer_progress_abandon_upload;
DROP TABLE media_asset_transfer_abandoned_uploads;
