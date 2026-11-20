package main

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// policyWiringStorage は site で原本の置き場が分かれる設定を返す。年齢 0 の評価で、
// takamatsu は nagoya、それ以外は tokyo になる。空の方針を束ねた store は "" を返す。
func policyWiringStorage(t *testing.T, derived string, extra map[string]config.StoreConfig) config.StorageConfig {
	t.Helper()
	stores := map[string]config.StoreConfig{
		"tokyo":  {Kind: "fs", Root: t.TempDir()},
		"nagoya": {Kind: "fs", Root: t.TempDir()},
	}
	for name, sc := range extra {
		stores[name] = sc
	}
	return config.StorageConfig{
		Stores: stores,
		Originals: []config.OriginalStoreRule{
			{Match: &config.OriginalStoreMatch{Site: "takamatsu"}, Store: "nagoya"},
			{Store: "tokyo"},
		},
		Derived: derived,
	}
}

// requireBoundOriginalRules は、store に束ねた方針の原本の規則を年齢 0 で評価し、設定どおりの置き場が返ることを確かめる。
func requireBoundOriginalRules(t *testing.T, label string, chooser store.IngestStoreChooser) {
	t.Helper()
	for site, want := range map[string]string{"takamatsu": "nagoya", "home": "tokyo"} {
		got, err := chooser.DesiredOriginalStoreAtIngest(context.Background(), site)
		testutil.RequireNoError(t, err, "evaluating the bound original rules")
		if got != want {
			t.Fatalf("%s: desired original store for site %q = %q, want %q", label, site, got, want)
		}
	}
}

// TestWorkerDepsBindStoragePolicyToUnscopedMedia は、worker の起動経路（newWorkerDeps）が置き場に束ねない
// Media に設定の方針を束ねることを確かめる。この Media は転送の reconcile・転送・encode の背圧・ingest の
// 置き場選びが共有する。空の方針を束ねると、desired の置き場が決まらず転送が黙って止まる。
func TestWorkerDepsBindStoragePolicyToUnscopedMedia(t *testing.T) {
	pool := testutil.SetupDB(t)
	cfg := &config.Config{Mirakcs: []config.MirakcSite{{Site: "home", URL: "http://127.0.0.1:1"}}, Storage: policyWiringStorage(t, "tokyo", nil)}
	policy, err := cfg.Storage.BuildStorePolicy()
	testutil.RequireNoError(t, err, "building the storage policy")
	roots := map[string]string{"tokyo": cfg.Storage.Stores["tokyo"].Root, "nagoya": cfg.Storage.Stores["nagoya"].Root}

	deps, _, err := newWorkerDeps(context.Background(), cfg, policy, nil, []string{"nagoya", "tokyo"}, roots, riveradapter.Config{Queues: []string{"ruler"}}, pool, nil, false)
	testutil.RequireNoError(t, err, "building worker deps")

	requireBoundOriginalRules(t, "Deps.IngestStores", deps.IngestStores)
	transfer, ok := deps.UnscopedStorageTransfer.(store.IngestStoreChooser)
	if !ok {
		t.Fatalf("Deps.UnscopedStorageTransfer (%T) does not evaluate original rules", deps.UnscopedStorageTransfer)
	}
	requireBoundOriginalRules(t, "Deps.UnscopedStorageTransfer", transfer)
}

// TestStoreScopedDerivedMediaBindsStoragePolicy は、派生物の置き場が S3 のとき、置き場ごとの依存
// （newStoreScopedDeps）が派生物の Media に設定の方針を束ねることを確かめる。この Media は S3 の削除パスの
// 配置判定に使われ、空の方針を束ねると desired から外れたコピーを消さなくなる。
func TestStoreScopedDerivedMediaBindsStoragePolicy(t *testing.T) {
	pool := testutil.SetupDB(t)
	cfg := &config.Config{Mirakcs: []config.MirakcSite{{Site: "home"}}, Storage: policyWiringStorage(t, "archive", map[string]config.StoreConfig{"archive": {Kind: "s3"}})}
	policy, err := cfg.Storage.BuildStorePolicy()
	testutil.RequireNoError(t, err, "building the storage policy")
	derivedObjects, err := fsstore.New(t.TempDir())
	testutil.RequireNoError(t, err, "creating derived objects")

	// 名前順で最初の FS 置き場（nagoya）が S3 の削除パスを持つ。
	scoped := mustNewStoreScopedDeps(t, pool, cfg, policy, "nagoya", t.TempDir(), derivedObjects)
	if scoped.DerivedObjectDeleteStores == nil {
		t.Fatal("nagoya has no derived object delete stores")
	}
	derivedDelete, ok := scoped.DerivedObjectDeleteStores.MediaDelete().(store.IngestStoreChooser)
	if !ok {
		t.Fatalf("derived MediaDelete (%T) does not evaluate original rules", scoped.DerivedObjectDeleteStores.MediaDelete())
	}
	requireBoundOriginalRules(t, "DerivedObjectDeleteStores.MediaDelete", derivedDelete)
}
