-- +goose Up
-- 通知表。書き手はトリガーと mysqlstore の NotifyTopic、消すのは掃除だけである。
-- 行の存在は「まだ掃除されていない通知」を意味する。
-- created_at は文の実行時刻なので、コミット順と id の順は一致しない。
CREATE TABLE notifications (
    id BIGINT NOT NULL AUTO_INCREMENT,
    payload VARCHAR(8000) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    INDEX notifications_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE notifications;
