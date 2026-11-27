package worker

import (
	"context"
	"testing"
)

// TestNewWorkersRegistersNoRiverBuilder は worker.NewWorkers が River の builder を 1 つも
// 預けないことを固定する。River の登録は internal/worker/riverbind が後から預ける。
// 各 kind に "river" の builder を 1 つ足せるかを試す。AddAdapterWorker は同じ adapter・kind の
// 2 つ目を拒むので、NewWorkers が 1 つでも預けていれば、ここで落ちる。
func TestNewWorkersRegistersNoRiverBuilder(t *testing.T) {
	t.Parallel()
	registry := NewWorkers(&Deps{NotificationPruner: fakeRiverCheckPruner{}})
	kinds := registry.ZeroArgs()
	if len(kinds) == 0 {
		t.Fatal("NewWorkers registered no kinds")
	}
	for _, args := range kinds {
		kind := args.Kind()
		if err := registry.AddAdapterWorker("river", kind, func(any) error { return nil }); err != nil {
			t.Errorf("kind %q already has a River builder from NewWorkers: %v", kind, err)
		}
	}
}

// fakeRiverCheckPruner は NotifyPrune worker を登録させるための store.NotificationPruner の空実装。
type fakeRiverCheckPruner struct{}

func (fakeRiverCheckPruner) PruneNotifications(context.Context) (int64, error) { return 0, nil }
