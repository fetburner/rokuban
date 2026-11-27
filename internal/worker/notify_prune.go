package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

// NotifyPruneWorker は MySQL の通知表から保持期間を過ぎた行を削除する。
//
// 掃除を notifier に置かないのは、notifier を置かない構成でもトリガーが通知表への書き込みを続けるからである。
// 定期投入の間隔は保持期間以下にする（cmd/rokuban の workerPeriodicJobSpecs）。
type NotifyPruneWorker struct {
	Store store.NotificationPruner
}

// Work は store に掃除を委ねる。削除した行数は debug ログに出す。
func (w *NotifyPruneWorker) Work(ctx context.Context, _ jobs.NotifyPruneArgs) error {
	deleted, err := w.Store.PruneNotifications(ctx)
	if err != nil {
		return fmt.Errorf("pruning notifications: %w", err)
	}
	slog.Debug("pruned notifications", "rows", deleted)
	return nil
}
