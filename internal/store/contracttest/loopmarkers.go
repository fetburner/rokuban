package contracttest

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// LoopMarkerFixture は loop marker store と、特定の loop の scope への書き込みを失敗させる準備をまとめる。
type LoopMarkerFixture struct {
	Store store.LoopMarkers
	// FailMarkFor は loop の scope への書き込みを次回から失敗させる。読み取りには影響しない。
	FailMarkFor func(t *testing.T, loop, scope string)
}

// NewLoopMarkerStore は loop marker 契約の実装を作る。
type NewLoopMarkerStore func(t *testing.T) LoopMarkerFixture

// RunLoopMarkers は成功時刻の書き先と読み取りの契約を実装に対して実行する。
// 各 loop の scope 集合を完全一致で見るので、書き先の loop を取り違えると別の loop の集合に現れて落ちる。
func RunLoopMarkers(t *testing.T, newStore NewLoopMarkerStore) {
	t.Helper()
	f := newStore(t)
	if f.Store == nil || f.FailMarkFor == nil {
		t.Fatal("loop marker store and failure setup are required")
	}
	ctx := context.Background()
	const (
		site        = "contract-loop-markers"
		otherSite   = "contract-loop-markers-other"
		deleteStore = "contract-loop-markers-store"
		pass        = store.ReconcilePassEncode
	)
	allLoops := []string{store.LoopRuler, store.LoopRecordSweep, store.LoopReconcilePass, store.LoopDeleteReconcile, store.LoopReconciler, store.LoopEPGSync, store.LoopTunerSync}

	for _, loop := range allLoops {
		assertLoopScopes(t, f, loop)
	}
	before := time.Now().Add(-time.Minute)

	if err := f.Store.MarkRulerPasses(ctx, []string{site, otherSite}); err != nil {
		t.Fatalf("MarkRulerPasses: %v", err)
	}
	assertLoopScopes(t, f, store.LoopRuler, site, otherSite)
	assertLoopScopes(t, f, store.LoopRecordSweep)
	assertLoopScopes(t, f, store.LoopReconcilePass)
	assertLoopScopes(t, f, store.LoopDeleteReconcile)

	if err := f.Store.MarkRecordSweep(ctx, site); err != nil {
		t.Fatalf("MarkRecordSweep: %v", err)
	}
	assertLoopScopes(t, f, store.LoopRecordSweep, site)
	assertLoopScopes(t, f, store.LoopRuler, site, otherSite)

	if err := f.Store.MarkReconcilePass(ctx, pass); err != nil {
		t.Fatalf("MarkReconcilePass(%s): %v", pass, err)
	}
	assertLoopScopes(t, f, store.LoopReconcilePass, pass)
	assertLoopScopes(t, f, store.LoopRuler, site, otherSite)
	assertLoopScopes(t, f, store.LoopRecordSweep, site)

	if err := f.Store.MarkDeleteReconcile(ctx, deleteStore); err != nil {
		t.Fatalf("MarkDeleteReconcile(%q): %v", deleteStore, err)
	}
	assertLoopScopes(t, f, store.LoopDeleteReconcile, deleteStore)
	assertLoopScopes(t, f, store.LoopReconcilePass, pass)

	if err := f.Store.MarkReconciler(ctx, site); err != nil {
		t.Fatalf("MarkReconciler(%q): %v", site, err)
	}
	assertLoopScopes(t, f, store.LoopReconciler, site)

	if err := f.Store.MarkEPGSync(ctx, site); err != nil {
		t.Fatalf("MarkEPGSync(%q): %v", site, err)
	}
	assertLoopScopes(t, f, store.LoopEPGSync, site)
	assertLoopScopes(t, f, store.LoopTunerSync)

	if err := f.Store.MarkTunerSync(ctx, site); err != nil {
		t.Fatalf("MarkTunerSync(%q): %v", site, err)
	}
	assertLoopScopes(t, f, store.LoopTunerSync, site)
	assertLoopScopes(t, f, store.LoopEPGSync, site)
	assertLoopScopes(t, f, store.LoopRuler, site, otherSite)
	assertLoopScopes(t, f, store.LoopRecordSweep, site)
	assertLoopScopes(t, f, store.LoopReconcilePass, pass)
	assertLoopScopes(t, f, store.LoopDeleteReconcile, deleteStore)

	for _, m := range []struct{ loop, scope string }{
		{store.LoopRuler, site},
		{store.LoopRecordSweep, site},
		{store.LoopReconcilePass, pass},
		{store.LoopDeleteReconcile, deleteStore},
		{store.LoopReconciler, site},
		{store.LoopEPGSync, site},
		{store.LoopTunerSync, site},
	} {
		got, err := f.Store.ReadLoopMarkers(ctx, m.loop)
		if err != nil {
			t.Fatalf("ReadLoopMarkers(%s): %v", m.loop, err)
		}
		if at, ok := got[m.scope]; !ok || at.Before(before) {
			t.Fatalf("%s marker for %q = %v (present %v), want a recent time", m.loop, m.scope, at, ok)
		}
	}

	if err := f.Store.MarkRulerPasses(ctx, nil); err != nil {
		t.Fatalf("MarkRulerPasses(empty): %v, want no-op", err)
	}

	// 同じ scope への 2 回目の Mark は時刻を進める。upsert を DO NOTHING に変えると、
	// 最初の時刻で凍り、監視の鮮度が止まって見える。
	for _, m := range []struct {
		loop, scope string
		mark        func() error
	}{
		{store.LoopRecordSweep, site, func() error { return f.Store.MarkRecordSweep(ctx, site) }},
		{store.LoopReconcilePass, pass, func() error { return f.Store.MarkReconcilePass(ctx, pass) }},
		{store.LoopReconciler, site, func() error { return f.Store.MarkReconciler(ctx, site) }},
		{store.LoopEPGSync, site, func() error { return f.Store.MarkEPGSync(ctx, site) }},
		{store.LoopTunerSync, site, func() error { return f.Store.MarkTunerSync(ctx, site) }},
	} {
		first, err := f.Store.ReadLoopMarkers(ctx, m.loop)
		if err != nil {
			t.Fatalf("ReadLoopMarkers(%s, first): %v", m.loop, err)
		}
		if err := m.mark(); err != nil {
			t.Fatalf("second mark of %s/%q: %v", m.loop, m.scope, err)
		}
		second, err := f.Store.ReadLoopMarkers(ctx, m.loop)
		if err != nil {
			t.Fatalf("ReadLoopMarkers(%s, second): %v", m.loop, err)
		}
		if !second[m.scope].After(first[m.scope]) {
			t.Fatalf("%s marker for %q after a second mark = %v, want after %v", m.loop, m.scope, second[m.scope], first[m.scope])
		}
	}

	runLoopMarkerFailureCases(t, newStore, site, otherSite, deleteStore)
}

// assertLoopScopes は loop に記録された scope の集合が want と一致することを見る。
func assertLoopScopes(t *testing.T, f LoopMarkerFixture, loop string, want ...string) {
	t.Helper()
	got, err := f.Store.ReadLoopMarkers(context.Background(), loop)
	if err != nil {
		t.Fatalf("ReadLoopMarkers(%s): %v", loop, err)
	}
	gotScopes := slices.Sorted(maps.Keys(got))
	slices.Sort(want)
	if !slices.Equal(gotScopes, want) {
		t.Fatalf("ReadLoopMarkers(%s) scopes = %v, want %v", loop, gotScopes, want)
	}
}

// runLoopMarkerFailureCases は書き込みが失敗した印が残らないことを見る。失敗の形は書き込みの側に作る。
func runLoopMarkerFailureCases(t *testing.T, newStore NewLoopMarkerStore, site, otherSite, deleteStore string) {
	t.Helper()
	ctx := context.Background()
	t.Run("multi-site ruler failure rolls back every site", func(t *testing.T) {
		f := newStore(t)
		f.FailMarkFor(t, store.LoopRuler, otherSite)
		if err := f.Store.MarkRulerPasses(ctx, []string{site, otherSite}); err == nil {
			t.Fatal("MarkRulerPasses(failing second site) = nil, want error")
		}
		assertLoopScopes(t, f, store.LoopRuler)
	})

	t.Run("failed pass marker is not recorded", func(t *testing.T) {
		f := newStore(t)
		f.FailMarkFor(t, store.LoopReconcilePass, store.ReconcilePassTSScan)
		if err := f.Store.MarkReconcilePass(ctx, store.ReconcilePassTSScan); err == nil {
			t.Fatal("MarkReconcilePass(failing pass) = nil, want error")
		}
		assertLoopScopes(t, f, store.LoopReconcilePass)
	})

	t.Run("failed delete marker is not recorded", func(t *testing.T) {
		f := newStore(t)
		f.FailMarkFor(t, store.LoopDeleteReconcile, deleteStore)
		if err := f.Store.MarkDeleteReconcile(ctx, deleteStore); err == nil {
			t.Fatal("MarkDeleteReconcile(failing store) = nil, want error")
		}
		assertLoopScopes(t, f, store.LoopDeleteReconcile)
	})

	t.Run("failed epg sync marker is not recorded", func(t *testing.T) {
		f := newStore(t)
		f.FailMarkFor(t, store.LoopEPGSync, site)
		if err := f.Store.MarkEPGSync(ctx, site); err == nil {
			t.Fatal("MarkEPGSync(failing site) = nil, want error")
		}
		assertLoopScopes(t, f, store.LoopEPGSync)
	})

	t.Run("failed tuner sync marker is not recorded", func(t *testing.T) {
		f := newStore(t)
		f.FailMarkFor(t, store.LoopTunerSync, site)
		if err := f.Store.MarkTunerSync(ctx, site); err == nil {
			t.Fatal("MarkTunerSync(failing site) = nil, want error")
		}
		assertLoopScopes(t, f, store.LoopTunerSync)
	})
}
