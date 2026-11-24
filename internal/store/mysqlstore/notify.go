package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// maxNotificationBytes は通知 payload の上限。Postgres の pg_notify の上限に合わせる。
const maxNotificationBytes = 8000

// Execer は tx と DB のどちらでも通知を書けるようにする。
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// NotifyTopic は通知表に payload を 1 行書く（Postgres の NotifyTopic に相当する）。
// データを変えたメソッドは自身の tx を渡し、データと同じコミットで通知を確定させる。
// 検査は TestNotifyTopicWritesRow と TestNotifyTopicRollsBackWithTransaction。
func NotifyTopic(ctx context.Context, exec Execer, payload string) error {
	if len(payload) > maxNotificationBytes {
		return fmt.Errorf("notification payload is %d bytes, limit is %d", len(payload), maxNotificationBytes)
	}
	if _, err := exec.ExecContext(ctx, `INSERT INTO notifications (payload) VALUES (?)`, payload); err != nil {
		return fmt.Errorf("inserting notification: %w", err)
	}
	return nil
}

// PruneNotifications は created_at が retention より古い通知を削除し、削除した行数を返す。
// notifier の有無と無関係に呼ばれる前提で、notifier 側には置かない。
// 検査は TestPruneNotificationsRemovesOnlyOldRows。
func PruneNotifications(ctx context.Context, db *sql.DB, retention time.Duration) (int64, error) {
	result, err := db.ExecContext(ctx, `DELETE FROM notifications WHERE created_at < NOW(6) - INTERVAL ? MICROSECOND`, retention.Microseconds())
	if err != nil {
		return 0, fmt.Errorf("pruning notifications: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting pruned notifications: %w", err)
	}
	return deleted, nil
}
