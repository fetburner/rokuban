package store

import "context"

// NotificationPruner は通知表から保持期間を過ぎた行を削除する契約。
// 通知表は MySQL だけが持つ（PostgreSQL は LISTEN/NOTIFY を使い、表を持たない）。
// 呼び出しは定期ジョブが行い、notifier には置かない。トリガーは notifier の有無と無関係に書き続けるため、
// notifier を止めた構成でも掃除が要る。
type NotificationPruner interface {
	// PruneNotifications は保持期間を過ぎた通知を削除し、削除した行数を返す。
	PruneNotifications(ctx context.Context) (int64, error)
}
