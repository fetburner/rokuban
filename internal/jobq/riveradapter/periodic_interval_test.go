package riveradapter

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	riveradaptertest "github.com/fetburner/rokuban/internal/jobq/riveradapter/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

// jobq.PeriodicJobSpec の契約（Start 直後に 1 回、その後は Interval ごと）と、
// 仕様の Interval が写ることを実 DB で固定する。
//
//   - 周期が 1 時間のジョブは、このテストの間に 1 行だけ入る（Start 直後の 1 回。
//     RunOnStart を外すと 0 行で落ちる）
//   - 周期が 300ms のジョブは 3 行以上入る（Interval を写し損ねて 1 時間に化けると落ちる）
func TestPeriodicJobsRunOnStartAndEveryInterval(t *testing.T) {
	pool := testutil.SetupDB(t)
	registry := jobq.NewRegistry()
	Register(registry, nopWorker[jobs.LabelRuleReconcileArgs]{})
	Register(registry, nopWorker[jobs.DeleteReconcileArgs]{})
	adapter, err := NewAdapter(pool, registry, testQueueFor, Config{
		Spec: jobq.WorkerSpec{
			Queues: []jobq.QueueSpec{plainQueue(jobs.CleanupQueue, 1), storeQueue(jobs.OriginalQueue, 1, "test-store")},
			PeriodicJobs: []jobq.PeriodicJobSpec{
				{Interval: 300 * time.Millisecond, Args: jobs.LabelRuleReconcileArgs{}},
				{Interval: time.Hour, Args: jobs.DeleteReconcileArgs{Store: "test-store"}},
			},
		},
		SchedulePeriodicJobs: true,
		TestOnly:             true,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := adapter.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	deadline := time.Now().Add(20 * time.Second)
	for {
		rows := riveradaptertest.MustListRiverJobsOfKind(t, context.Background(), pool, "label_rule_reconcile")
		if len(rows) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("label_rule_reconcile rows = %d after 20s, want >= 3 for a 300ms interval", len(rows))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rows := riveradaptertest.MustListRiverJobsOfKind(t, context.Background(), pool, "delete_reconcile"); len(rows) != 1 {
		t.Errorf("delete_reconcile rows = %d, want exactly 1 (run on start only for a 1h interval)", len(rows))
	}
}
