// このファイルは Postgres でだけ残す削除の検査を置く。残す理由は 2 つ。view・関数を名前で直接読むこと、
// 配信の読み手の構成（NewMediaForStreamer）が MySQL に無いこと。

package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

const otherStoreName = "other-store"

var storeScopeEventID atomic.Int32

type storeScopeEnv struct {
	pool *pgxpool.Pool
	// media は dbtest.StoreName の置き場を担当する worker の Media。
	media *Media
}

func newStoreScopeEnv(t *testing.T) storeScopeEnv {
	t.Helper()
	pool := testutil.SetupDB(t)
	return storeScopeEnv{pool: pool, media: testMedia(pool, dbtest.StoreName, dbtest.StoreName)}
}

func (e storeScopeEnv) recording(t *testing.T) int64 {
	t.Helper()
	return dbtest.Recording(t, e.pool, func(p *dbtest.RecordingParams) {
		p.EventID = storeScopeEventID.Add(1)
		p.ProgramDurationMs = 60_000
	})
}

func (e storeScopeEnv) asset(t *testing.T, recordingID int64, kind string, profile *string) int64 {
	t.Helper()
	return dbtest.MediaAsset(t, e.pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind = kind
		p.Profile = profile
		p.RelPath = fmt.Sprintf("sites/%s/store-scope/%d/%s", store.DefaultSite, recordingID, kind)
	})
}

// moveCopy は assetID の dbtest.StoreName のコピーを toStore へ移す。
func (e storeScopeEnv) moveCopy(t *testing.T, assetID int64, toStore string) {
	t.Helper()
	if toStore == dbtest.StoreName {
		return
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE media_asset_copies SET store = $2 WHERE media_asset_id = $1 AND store = $3`,
		assetID, toStore, dbtest.StoreName); err != nil {
		t.Fatalf("moving copy of asset %d to %s: %v", assetID, toStore, err)
	}
}

// untilEncodedFixture は until_encoded の削除条件を満たす録画を作る。全コピーは dbtest.StoreName にある。
func (e storeScopeEnv) untilEncodedFixture(t *testing.T) (recordingID, originalID int64, derived map[string]int64) {
	t.Helper()
	recordingID = e.recording(t)
	originalID = e.asset(t, recordingID, store.AssetKindOriginal, nil)
	profile := "h264"
	derived = map[string]int64{
		store.AssetKindEncoded:   e.asset(t, recordingID, store.AssetKindEncoded, &profile),
		store.AssetKindThumbnail: e.asset(t, recordingID, store.AssetKindThumbnail, nil),
		store.AssetKindSeekTiles: e.asset(t, recordingID, store.AssetKindSeekTiles, nil),
	}
	if _, err := e.pool.Exec(context.Background(), `
INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
SELECT id, size_bytes FROM media_assets WHERE id = $1`, originalID); err != nil {
		t.Fatalf("seeding original TS scan marker: %v", err)
	}
	if _, err := e.pool.Exec(context.Background(), `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'until_encoded', ARRAY['h264'], false)`, recordingID); err != nil {
		t.Fatalf("seeding until_encoded policy: %v", err)
	}
	return recordingID, originalID, derived
}

func stringPointer(value string) *string { return &value }

// TestDerivedReadersRequireCurrentGenerationAcrossStores は、encoded の旧世代のコピーと、別の置き場にだけある現行のコピーを
// どう数えるかを確かめる。active_media_asset_copies（置き場あり・なし）・unencoded_desired_profiles・
// unsatisfied_encode_rebuilds・ListEncodedMeaningEvidence の判定を、それぞれ view・関数の名前で直接読む。
func TestDerivedReadersRequireCurrentGenerationAcrossStores(t *testing.T) {
	ctx := context.Background()
	env := newStoreScopeEnv(t)
	recordingID, _, assets := env.untilEncodedFixture(t)
	encodedID := assets[store.AssetKindEncoded]
	env.moveCopy(t, encodedID, otherStoreName)
	if _, err := env.pool.Exec(ctx, `UPDATE media_assets SET derived_generation = 1 WHERE id = $1`, encodedID); err != nil {
		t.Fatalf("advancing encoded generation: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
INSERT INTO media_asset_cuts (media_asset_id, keep_ranges)
VALUES ($1, '{[0,60000)}'::int8multirange)`, encodedID); err != nil {
		t.Fatalf("seeding committed cut ranges: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `
INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep)
VALUES ($1, 'h264', '{[0,60000)}'::int8multirange)`, recordingID); err != nil {
		t.Fatalf("seeding rebuild request: %v", err)
	}

	activeCopyCount := func(storeName *string) int {
		t.Helper()
		var count int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM active_media_asset_copies($1, $2)`, encodedID, storeName).Scan(&count); err != nil {
			t.Fatalf("counting current encoded copies for store %v: %v", storeName, err)
		}
		return count
	}
	viewContains := func(view, predicate string, args ...any) bool {
		t.Helper()
		var exists bool
		query := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE %s)`, view, predicate)
		if err := env.pool.QueryRow(ctx, query, args...).Scan(&exists); err != nil {
			t.Fatalf("querying %s: %v", view, err)
		}
		return exists
	}
	if got := activeCopyCount(nil); got != 0 {
		t.Fatalf("active copies before publishing current generation = %d, want 0 (the old copy is in %s)", got, otherStoreName)
	}
	if got := activeCopyCount(stringPointer(dbtest.StoreName)); got != 0 {
		t.Fatalf("desired-store current copies before publish = %d, want 0", got)
	}
	if !viewContains("unencoded_desired_profiles", "recording_id = $1 AND profile = 'h264'", recordingID) {
		t.Fatal("old-generation copy satisfied unencoded_desired_profiles")
	}
	if !viewContains("unsatisfied_encode_rebuilds", "recording_id = $1 AND profile = 'h264'", recordingID) {
		t.Fatal("old-generation copy satisfied the rebuild request")
	}
	evidence, err := sqlcgen.New(env.pool).ListEncodedMeaningEvidence(ctx, nil)
	if err != nil {
		t.Fatalf("listing encoded meaning evidence: %v", err)
	}
	for _, row := range evidence {
		if row.Profile == "h264" && row.RelPath == "sites/default/store-scope/"+fmt.Sprint(recordingID)+"/encoded" {
			t.Fatal("old-generation path was returned as active encoded evidence")
		}
	}

	newRelPath := "encoded/h264.g1.mp4"
	if _, err := sqlcgen.New(env.pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: encodedID, Store: otherStoreName, RelPath: newRelPath, Generation: 1, State: "active",
	}); err != nil {
		t.Fatalf("adding current-generation copy in another store: %v", err)
	}
	if got := activeCopyCount(nil); got != 1 {
		t.Fatalf("global current copies after remote publish = %d, want 1", got)
	}
	if got := activeCopyCount(stringPointer(dbtest.StoreName)); got != 0 {
		t.Fatalf("desired-store current copies after remote publish = %d, want 0", got)
	}
	if viewContains("unencoded_desired_profiles", "recording_id = $1 AND profile = 'h264'", recordingID) {
		t.Fatal("current-generation copy in another store did not satisfy the global encoded inventory")
	}
	if viewContains("unsatisfied_encode_rebuilds", "recording_id = $1 AND profile = 'h264'", recordingID) {
		t.Fatal("current-generation copy in another store did not satisfy the rebuild request")
	}
}

// TestPreferredActiveMediaAssetCopyFunction は、Postgres の関数 preferred_active_media_asset_copy が
// 置き場名順で先頭の active コピーを 1 件返し、deleting を候補にしないことを名前で直接読む。
// 配信の読み手の選び方は contracttest の runServingCopySelection が見る。
func TestPreferredActiveMediaAssetCopyFunction(t *testing.T) {
	ctx := context.Background()
	env := newStoreScopeEnv(t)
	rec := env.recording(t)
	profile := "h264"
	kinds := map[string]int64{
		store.AssetKindOriginal:  env.asset(t, rec, store.AssetKindOriginal, nil),
		store.AssetKindEncoded:   env.asset(t, rec, store.AssetKindEncoded, &profile),
		store.AssetKindThumbnail: env.asset(t, rec, store.AssetKindThumbnail, nil),
		store.AssetKindSeekTiles: env.asset(t, rec, store.AssetKindSeekTiles, nil),
	}
	for kind, id := range kinds {
		for _, c := range []struct{ store, state string }{{"a-store", "deleting"}, {"b-store", "active"}, {"z-store", "active"}} {
			if _, err := sqlcgen.New(env.pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
				MediaAssetID: id, Store: c.store, RelPath: fmt.Sprintf("sites/default/pick/%s-%s", kind, c.store), State: c.state,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var n int
	var first string
	if err := env.pool.QueryRow(ctx, `SELECT count(*), min(store) FROM preferred_active_media_asset_copy($1, NULL)`, kinds[store.AssetKindOriginal]).Scan(&n, &first); err != nil {
		t.Fatal(err)
	}
	if n != 1 || first != "b-store" {
		t.Fatalf("preferred_active_media_asset_copy returned %d rows (first store %q), want exactly the b-store copy", n, first)
	}
}

// 読める置き場の nil は SQL の NULL（全置き場が読める）、空配列は '{}'（読める置き場が無い）に対応する。
// 取り違えると、全置き場の読み手が配れなくなるか、読める置き場の無い streamer が他の置き場のコピーを配る。
func TestServingReadableStoresNilMeansAllAndEmptyMeansNone(t *testing.T) {
	ctx := context.Background()
	env := newStoreScopeEnv(t)
	rec := env.recording(t)
	env.asset(t, rec, store.AssetKindThumbnail, nil)

	got, err := testMedia(env.pool, "", "").GetThumbnailMediaAssetForServing(ctx, rec)
	if err != nil || got.Store != dbtest.StoreName {
		t.Fatalf("all stores readable (nil) = (%+v, %v), want the %s copy", got, err, dbtest.StoreName)
	}
	for name, m := range map[string]*Media{
		"empty readable":     NewMediaForStreamer(env.pool, store.StoragePolicy{}, []string{}),
		"unrelated readable": NewMediaForStreamer(env.pool, store.StoragePolicy{}, []string{otherStoreName}),
	} {
		if _, err := m.GetThumbnailMediaAssetForServing(ctx, rec); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: err = %v, want ErrNotFound (the only copy is in %s, not readable here)", name, err, dbtest.StoreName)
		}
	}
}
