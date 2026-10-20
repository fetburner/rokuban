package contracttest

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// NeverScheduledFixture は欠測観測の契約とその状態を作るフックをまとめる。
type NeverScheduledFixture struct {
	Store store.NeverScheduledEvents
	// AgeObservation は記録済みの観測行の observed_at を、実装の時計で age だけ過去へ動かす。
	AgeObservation func(t *testing.T, site string, event store.BroadcastEventKey, age time.Duration)
}

// NewNeverScheduledStore は契約テスト用の欠測観測 store を作る。
type NewNeverScheduledStore func(t *testing.T) NeverScheduledFixture

// RunNeverScheduled は欠測観測の冪等記録と、horizon による削除の契約を実行する。
func RunNeverScheduled(t *testing.T, newStore NewNeverScheduledStore) {
	t.Helper()
	f := newStore(t)
	s := f.Store
	ctx := context.Background()
	const site, otherSite = "contract-never-scheduled", "contract-never-scheduled-other"
	event := store.BroadcastEventKey{NetworkID: 32678, ServiceID: 5168, EventID: 12345}
	created, err := s.RecordNeverScheduled(ctx, site, event)
	if err != nil || !created {
		t.Fatalf("RecordNeverScheduled(first) = (%t, %v), want (true, nil)", created, err)
	}
	created, err = s.RecordNeverScheduled(ctx, site, event)
	if err != nil || created {
		t.Fatalf("RecordNeverScheduled(retry) = (%t, %v), want (false, nil)", created, err)
	}
	created, err = s.RecordNeverScheduled(ctx, otherSite, event)
	if err != nil || !created {
		t.Fatalf("RecordNeverScheduled(other site) = (%t, %v), want (true, nil)", created, err)
	}
	created, err = s.RecordNeverScheduled(ctx, otherSite, event)
	if err != nil || created {
		t.Fatalf("RecordNeverScheduled(other site retry) = (%t, %v), want (false, nil)", created, err)
	}

	f.AgeObservation(t, site, event, 2*time.Hour)
	deleted, err := s.PruneNeverScheduled(ctx, time.Hour)
	if err != nil || deleted != 1 {
		t.Fatalf("PruneNeverScheduled(1h) = (%d, %v), want (1, nil)", deleted, err)
	}
	created, err = s.RecordNeverScheduled(ctx, otherSite, event)
	if err != nil || created {
		t.Fatalf("RecordNeverScheduled(row newer than horizon) = (%t, %v), want (false, nil): row must survive", created, err)
	}
	created, err = s.RecordNeverScheduled(ctx, site, event)
	if err != nil || !created {
		t.Fatalf("RecordNeverScheduled(row older than horizon) = (%t, %v), want (true, nil): row must be deleted", created, err)
	}
}
