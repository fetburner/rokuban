package riverbind

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/worker"
)

type stubPruner struct{}

func (stubPruner) PruneNotifications(context.Context) (int64, error) { return 0, nil }

// TestBindAllCoversEveryWorkerKind は、worker.NewWorkers が登録する全 kind に River の builder を
// 預けられることを検査する。一覧から 1 つ消すと、NewAdapter が「kinds registered without an
// adapter "river" worker」で拒み、ここが落ちる。DB は使わない（nil の pool で client を組む）。
// notify_prune は NotificationPruner がある構成と無い構成の両方で検査する。
// nil の pool は契約内である。riverpgxv5.New の doc が「The database pool may be nil」と明記し、
// その場合 client は組めて Start だけがエラーになると定めている（go doc で確認）。
func TestBindAllCoversEveryWorkerKind(t *testing.T) {
	t.Parallel()
	for name, deps := range map[string]*worker.Deps{
		"without notification pruner": {},
		"with notification pruner":    {NotificationPruner: stubPruner{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			registry := worker.NewWorkers(deps)
			if err := BindAll(registry); err != nil {
				t.Fatalf("BindAll: %v", err)
			}
			if _, err := riveradapter.NewAdapter(nil, registry, jobs.PhysicalQueueFor, riveradapter.Config{}); err != nil {
				t.Fatalf("NewAdapter after BindAll: %v", err)
			}
		})
	}
}
