package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestWorkerDepsCarryStoragePolicyFromConfig は、worker ロールの起動経路（newWorkerDeps）が
// 設定から一度だけ作った置き場の方針を Deps に載せることを確かめる。期待値は設定から手で書いた値である。
// 載らないと worker は零値の方針で動き、転送と削除の判断が黙って空になる。
func TestWorkerDepsCarryStoragePolicyFromConfig(t *testing.T) {
	pool := testutil.SetupDB(t)
	cfg := &config.Config{Mirakcs: []config.MirakcSite{{Site: "home", URL: "http://127.0.0.1:1"}}, Storage: config.StorageConfig{
		Stores: map[string]config.StoreConfig{
			"tokyo":  {Kind: "fs", Root: t.TempDir()},
			"nagoya": {Kind: "fs", Root: t.TempDir()},
		},
		Originals: []config.OriginalStoreRule{
			{Match: &config.OriginalStoreMatch{Site: "takamatsu", OlderThan: "30d"}, Store: "nagoya"},
			{Store: "tokyo"},
		},
		Derived: "tokyo",
	}}
	roots := map[string]string{"tokyo": cfg.Storage.Stores["tokyo"].Root, "nagoya": cfg.Storage.Stores["nagoya"].Root}

	deps, _, err := newWorkerDeps(context.Background(), cfg, nil, []string{"nagoya", "tokyo"}, roots, riveradapter.Config{Queues: []string{"ruler"}}, pool, nil, false)
	testutil.RequireNoError(t, err, "building worker deps")

	want := store.StoragePolicy{
		Originals: []store.OriginalRule{
			{Site: "takamatsu", OlderThan: 30 * 24 * time.Hour, Store: "nagoya"},
			{Store: "tokyo"},
		},
		FSStores: []string{"nagoya", "tokyo"},
		Derived:  "tokyo",
	}
	if !reflect.DeepEqual(deps.StoragePolicy, want) {
		t.Fatalf("worker Deps.StoragePolicy = %+v, want %+v", deps.StoragePolicy, want)
	}
}
