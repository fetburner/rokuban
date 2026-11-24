-- +goose Up
-- loop_success_markers は監視向けの「ループが最後に成功・完走した時刻」を持つ。
-- Postgres 版（internal/db/schema.sql）の最終形に揃える。loop の許容値も同じ。
-- 行の存在は「その scope が一度は成功した」だけを主張する。PRIMARY KEY が狙う一意キーで、
-- ほかに一意キーが無いので ON DUPLICATE KEY UPDATE はこの組の衝突でしか発火しない。
CREATE TABLE loop_success_markers (
    `loop` VARCHAR(64) NOT NULL,
    scope VARCHAR(255) NOT NULL,
    last_success_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (`loop`, scope),
    CONSTRAINT loop_success_markers_loop_check CHECK (`loop` IN ('ruler', 'record_sweep', 'reconcile_pass', 'delete_reconcile', 'reconciler', 'epg_sync', 'tuner_sync', 'storage_transfer'))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_as_cs;

-- +goose Down
DROP TABLE loop_success_markers;
