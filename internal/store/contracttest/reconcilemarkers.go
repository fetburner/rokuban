package contracttest

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// ReconcileMarkerStore は全件パスと削除 reconcile の完走時刻を両方持つ実装である。
type ReconcileMarkerStore interface {
	store.ReconcilePassMarkers
	store.DeleteReconcileMarkers
}

// ReconcileMarkerFixture は reconcile marker store と、書き込みを失敗させる準備をまとめる。
type ReconcileMarkerFixture struct {
	Store ReconcileMarkerStore
	// FailPassMarkerFor は pass の書き込みを次回から失敗させる。
	FailPassMarkerFor func(t *testing.T, pass string)
	// FailDeleteMarkerFor は置き場 store の書き込みを次回から失敗させる。
	FailDeleteMarkerFor func(t *testing.T, store string)
}

// NewReconcileMarkerStore は reconcile marker 契約の実装を作る。
type NewReconcileMarkerStore func(t *testing.T) ReconcileMarkerFixture

// RunReconcileMarkers は全件パスと削除 reconcile の完走時刻の契約を実装に対して実行する。
func RunReconcileMarkers(t *testing.T, newStore NewReconcileMarkerStore) {
	t.Helper()
	f := newStore(t)
	if f.Store == nil || f.FailPassMarkerFor == nil || f.FailDeleteMarkerFor == nil {
		t.Fatal("reconcile marker store and failure setup are required")
	}
	ctx := context.Background()
	const site = "contract-reconcile-markers"

	initialPasses, err := f.Store.ReadReconcilePassMarkers(ctx)
	if err != nil {
		t.Fatalf("ReadReconcilePassMarkers(initial): %v", err)
	}
	if len(initialPasses) != 0 {
		t.Fatalf("initial pass markers = %v, want none", initialPasses)
	}
	initialStores, err := f.Store.ReadDeleteReconcileMarkers(ctx)
	if err != nil {
		t.Fatalf("ReadDeleteReconcileMarkers(initial): %v", err)
	}
	if len(initialStores) != 0 {
		t.Fatalf("initial delete markers = %v, want none", initialStores)
	}

	before := time.Now().Add(-time.Minute)
	if err := f.Store.MarkReconcilePass(ctx, store.ReconcilePassEncode); err != nil {
		t.Fatalf("MarkReconcilePass(encode): %v", err)
	}
	passes, err := f.Store.ReadReconcilePassMarkers(ctx)
	if err != nil {
		t.Fatalf("ReadReconcilePassMarkers(after encode): %v", err)
	}
	if at, ok := passes[store.ReconcilePassEncode]; !ok || at.Before(before) {
		t.Fatalf("encode marker = %v (present %v), want a recent time", at, ok)
	}
	if _, ok := passes[store.ReconcilePassThumbnail]; ok {
		t.Fatalf("thumbnail marker present after only the encode pass completed: %v", passes)
	}

	if err := f.Store.MarkDeleteReconcile(ctx, site); err != nil {
		t.Fatalf("MarkDeleteReconcile(%q): %v", site, err)
	}
	stores, err := f.Store.ReadDeleteReconcileMarkers(ctx)
	if err != nil {
		t.Fatalf("ReadDeleteReconcileMarkers(after mark): %v", err)
	}
	if at, ok := stores[site]; !ok || at.Before(before) {
		t.Fatalf("delete marker for %q = %v (present %v), want a recent time", site, at, ok)
	}
	if len(stores) != 1 {
		t.Fatalf("delete markers = %v, want only %q", stores, site)
	}
	passesAfterDelete, err := f.Store.ReadReconcilePassMarkers(ctx)
	if err != nil {
		t.Fatalf("ReadReconcilePassMarkers(after delete mark): %v", err)
	}
	if len(passesAfterDelete) != 1 {
		t.Fatalf("pass markers = %v after a delete-reconcile mark, want only the encode pass", passesAfterDelete)
	}

	t.Run("failed pass marker is not recorded", func(t *testing.T) {
		f := newStore(t)
		f.FailPassMarkerFor(t, store.ReconcilePassTSScan)
		if err := f.Store.MarkReconcilePass(ctx, store.ReconcilePassTSScan); err == nil {
			t.Fatal("MarkReconcilePass(failing pass) = nil, want error")
		}
		passes, err := f.Store.ReadReconcilePassMarkers(ctx)
		if err != nil {
			t.Fatalf("ReadReconcilePassMarkers(after failure): %v", err)
		}
		if _, ok := passes[store.ReconcilePassTSScan]; ok {
			t.Fatalf("ts_scan marker after a failed write = %v, want absent", passes)
		}
	})

	t.Run("failed delete marker is not recorded", func(t *testing.T) {
		f := newStore(t)
		f.FailDeleteMarkerFor(t, site)
		if err := f.Store.MarkDeleteReconcile(ctx, site); err == nil {
			t.Fatal("MarkDeleteReconcile(failing store) = nil, want error")
		}
		stores, err := f.Store.ReadDeleteReconcileMarkers(ctx)
		if err != nil {
			t.Fatalf("ReadDeleteReconcileMarkers(after failure): %v", err)
		}
		if _, ok := stores[site]; ok {
			t.Fatalf("delete marker after a failed write = %v, want absent", stores)
		}
	})
}
