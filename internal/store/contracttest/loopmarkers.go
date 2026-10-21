package contracttest

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

// LoopMarkerFixture は loop marker store をまとめる。
type LoopMarkerFixture struct {
	Store                  store.LoopMarkers
	FailRulerMarkerForSite func(t *testing.T, site string)
}

// NewLoopMarkerStore は loop marker 契約の実装を作る。
type NewLoopMarkerStore func(t *testing.T) LoopMarkerFixture

// RunLoopMarkers は ruler と record_sweep の成功 marker 契約を実装に対して実行する。
func RunLoopMarkers(t *testing.T, newStore NewLoopMarkerStore) {
	t.Helper()
	f := newStore(t)
	if f.Store == nil || f.FailRulerMarkerForSite == nil {
		t.Fatal("loop marker store and failure setup are required")
	}
	ctx := context.Background()
	const site = "contract-loop-markers"
	initial, err := f.Store.ReadLoopMarkers(ctx, site)
	if err != nil {
		t.Fatalf("ReadLoopMarkers(initial): %v", err)
	}
	if initial.RulerLastSuccessAt != nil || initial.RecordSweepLastSuccessAt != nil {
		t.Fatalf("initial markers = %+v, want absent markers", initial)
	}
	if err := f.Store.MarkRulerPasses(ctx, []string{site, site + "-other"}); err != nil {
		t.Fatalf("MarkRulerPasses: %v", err)
	}
	marked, err := f.Store.ReadLoopMarkers(ctx, site)
	if err != nil {
		t.Fatalf("ReadLoopMarkers(after ruler): %v", err)
	}
	if marked.RulerLastSuccessAt == nil || marked.RecordSweepLastSuccessAt != nil {
		t.Fatalf("markers after ruler pass = %+v, want ruler only", marked)
	}
	other, err := f.Store.ReadLoopMarkers(ctx, site+"-other")
	if err != nil {
		t.Fatalf("ReadLoopMarkers(after multi-site ruler pass): %v", err)
	}
	if other.RulerLastSuccessAt == nil || other.RecordSweepLastSuccessAt != nil {
		t.Fatalf("markers for second site = %+v, want ruler only", other)
	}
	if err := f.Store.MarkRecordSweep(ctx, site); err != nil {
		t.Fatalf("MarkRecordSweep: %v", err)
	}
	complete, err := f.Store.ReadLoopMarkers(ctx, site)
	if err != nil {
		t.Fatalf("ReadLoopMarkers(after record sweep): %v", err)
	}
	if complete.RulerLastSuccessAt == nil || complete.RecordSweepLastSuccessAt == nil {
		t.Fatalf("markers after both passes = %+v, want both timestamps", complete)
	}
	if err := f.Store.MarkRulerPasses(ctx, nil); err != nil {
		t.Fatalf("MarkRulerPasses(empty): %v, want no-op", err)
	}

	t.Run("multi-site marker failure rolls back every site", func(t *testing.T) {
		f := newStore(t)
		f.FailRulerMarkerForSite(t, site+"-other")
		if err := f.Store.MarkRulerPasses(ctx, []string{site, site + "-other"}); err == nil {
			t.Fatal("MarkRulerPasses(failing second site) = nil, want error")
		}
		for _, target := range []string{site, site + "-other"} {
			markers, err := f.Store.ReadLoopMarkers(ctx, target)
			if err != nil {
				t.Fatalf("ReadLoopMarkers(after rollback, site %q): %v", target, err)
			}
			if markers.RulerLastSuccessAt != nil {
				t.Fatalf("ruler marker for site %q after rollback = %v, want absent", target, markers.RulerLastSuccessAt)
			}
		}
	})
}
