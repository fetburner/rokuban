package worker

import (
	"context"
	"strings"
	"testing"
)

// TestNewWorkersRegistersNoRiverBuilder は worker.NewWorkers が River の builder を 1 つも
// 預けないことを固定する。River の登録は internal/jobq/riverbind が後から預ける。
// NewWorkers が riveradapter.Register に戻ると、どの kind にも builder があるので
// RegisterAdapterWorkers の「builder が無い」検査が通らなくなり、このテストが落ちる。
func TestNewWorkersRegistersNoRiverBuilder(t *testing.T) {
	t.Parallel()
	registry := NewWorkers(&Deps{NotificationPruner: fakeRiverCheckPruner{}})
	kinds := map[string]bool{}
	for _, args := range registry.ZeroArgs() {
		kinds[args.Kind()] = true
	}
	if len(kinds) == 0 {
		t.Fatal("NewWorkers registered no kinds")
	}
	// 1 つでも River の builder があれば、その kind は missing に載らない。全 kind が
	// missing に載ることを要求するので、部分的な登録も落ちる。
	err := registry.RegisterAdapterWorkers("river", nil)
	if err == nil || !strings.Contains(err.Error(), "without an adapter") {
		t.Fatalf("RegisterAdapterWorkers(river) = %v, want a missing-builder error", err)
	}
	msg := err.Error()
	missing := map[string]bool{}
	for _, kind := range strings.Split(msg[strings.LastIndex(msg, ": ")+2:], ", ") {
		missing[kind] = true
	}
	for kind := range kinds {
		if !missing[kind] {
			t.Errorf("kind %q has a River builder in NewWorkers; want none", kind)
		}
	}
}

// fakeRiverCheckPruner は NotifyPrune worker を登録させるための store.NotificationPruner の空実装。
type fakeRiverCheckPruner struct{}

func (fakeRiverCheckPruner) PruneNotifications(context.Context) (int64, error) { return 0, nil }
