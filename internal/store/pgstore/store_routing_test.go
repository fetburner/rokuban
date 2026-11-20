package pgstore

import (
	"context"
	"errors"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 録画の原本のコピーがある置き場は、Media の束縛によらず返る。複数の置き場にあれば名前順で先頭。
// コピーが消えた録画は結果に含まれない（OriginalStore は ErrNotFound）。
func TestOriginalStoresReturnsTheStoreHoldingTheActiveCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	onlyTakamatsu := dbtest.Recording(t, pool)
	both := dbtest.Recording(t, pool)
	gone := dbtest.Recording(t, pool)
	assets := map[int64]int64{}
	for i, id := range []int64{onlyTakamatsu, both, gone} {
		assets[id] = dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) { p.RelPath = []string{"a.m2ts", "b.m2ts", "c.m2ts"}[i] })
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`UPDATE media_asset_copies SET store = 'takamatsu' WHERE media_asset_id = $1`, assets[onlyTakamatsu])
	exec(`UPDATE media_asset_copies SET store = 'tokyo' WHERE media_asset_id = $1`, assets[both])
	exec(`INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
          SELECT media_asset_id, 'osaka', 'osaka/b.m2ts', generation, 'active' FROM media_asset_copies WHERE media_asset_id = $1`, assets[both])
	exec(`UPDATE media_asset_copies SET store = 'tokyo', state = 'deleted', deleted_at = now() WHERE media_asset_id = $1`, assets[gone])

	for _, media := range []*Media{NewMedia(pool, "", ""), NewMedia(pool, "tokyo", "")} {
		got, err := media.OriginalStores(ctx, []int64{onlyTakamatsu, both, gone})
		if err != nil {
			t.Fatalf("OriginalStores: %v", err)
		}
		if len(got) != 2 || got[onlyTakamatsu] != "takamatsu" || got[both] != "osaka" {
			t.Fatalf("OriginalStores = %v, want literal {onlyTakamatsu: takamatsu, both: osaka (first by name)} and no entry for the deleted copy", got)
		}
		if one, err := media.OriginalStore(ctx, onlyTakamatsu); err != nil || one != "takamatsu" {
			t.Fatalf("OriginalStore = %q, %v; want takamatsu", one, err)
		}
		if _, err := media.OriginalStore(ctx, gone); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("OriginalStore for a recording without an active copy: error = %v, want ErrNotFound", err)
		}
	}
	if got, err := NewMedia(pool, "", "").OriginalStores(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("OriginalStores(nil) = %v, %v; want an empty map", got, err)
	}
}

// 置き場ごとの URL の配信は、その置き場にコピーがある原本だけを返す。
func TestOriginalServingInStoreRequiresACopyInThatStore(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	id := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, id, func(p *dbtest.MediaAssetParams) { p.RelPath = "tk/x.m2ts" })
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'takamatsu' WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatal(err)
	}
	media := NewMedia(pool, "", "")
	got, err := media.GetOriginalMediaAssetForServingInStore(ctx, id, "takamatsu")
	if err != nil || got.Store != "takamatsu" || got.RelPath != "tk/x.m2ts" {
		t.Fatalf("serving from takamatsu = %+v, %v; want the takamatsu copy", got, err)
	}
	if _, err := media.GetOriginalMediaAssetForServingInStore(ctx, id, "tokyo"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("serving from a store without the copy: error = %v, want ErrNotFound", err)
	}
}

// ingest の置き場の選択は SQL の関数 desired_media_store に任せ、年齢 0 で評価する。
// older_than の規則は一致せず、site の規則と無条件の規則だけが残る。
func TestDesiredOriginalStoreAtIngestEvaluatesRulesAtAgeZero(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	storage := config.StorageConfig{
		Stores: map[string]config.StoreConfig{"tokyo": {Kind: "fs"}, "takamatsu": {Kind: "fs"}, "nagoya": {Kind: "fs"}},
		Originals: []config.OriginalStoreRule{
			{Match: &config.OriginalStoreMatch{Site: "takamatsu", OlderThan: "30d"}, Store: "nagoya"},
			{Match: &config.OriginalStoreMatch{Site: "takamatsu"}, Store: "takamatsu"},
			{Store: "tokyo"},
		},
		Derived: "tokyo",
	}
	rules, err := storage.OriginalRulesJSON()
	if err != nil {
		t.Fatal(err)
	}
	media := NewMedia(pool, "", "")
	for site, want := range map[string]string{"takamatsu": "takamatsu", "home": "tokyo"} {
		got, err := media.DesiredOriginalStoreAtIngest(ctx, rules, "tokyo", site)
		if err != nil || got != want {
			t.Errorf("site %q: store = %q, %v; want literal %q", site, got, err, want)
		}
	}
	if got, err := media.DesiredOriginalStoreAtIngest(ctx, []byte("[]"), "tokyo", "home"); err != nil || got != "" {
		t.Errorf("no rules: store = %q, %v; want empty", got, err)
	}
}
