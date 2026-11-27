package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/fetburner/rokuban/internal/jobs"
)

type fakeNotificationPruner struct {
	calls int
	err   error
}

func (f *fakeNotificationPruner) PruneNotifications(context.Context) (int64, error) {
	f.calls++
	return 3, f.err
}

// TestNotifyPruneWorkerCallsStoreAndWrapsError は、掃除が store に委ねられ、失敗が文脈付きで返ることを確かめる。
func TestNotifyPruneWorkerCallsStoreAndWrapsError(t *testing.T) {
	store := &fakeNotificationPruner{}
	if err := (&NotifyPruneWorker{Store: store}).Work(context.Background(), jobs.NotifyPruneArgs{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("PruneNotifications calls = %d, want 1", store.calls)
	}

	boom := errors.New("boom")
	store = &fakeNotificationPruner{err: boom}
	err := (&NotifyPruneWorker{Store: store}).Work(context.Background(), jobs.NotifyPruneArgs{})
	if !errors.Is(err, boom) {
		t.Fatalf("Work error = %v, want it to wrap %v", err, boom)
	}
}
