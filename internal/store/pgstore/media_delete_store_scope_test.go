package pgstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

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
	return storeScopeEnv{pool: pool, media: NewMedia(pool, dbtest.StoreName).WithDerivedStore(dbtest.StoreName)}
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

func (e storeScopeEnv) setCopyState(t *testing.T, assetID int64, storeName, state string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE media_asset_copies SET state = $3 WHERE media_asset_id = $1 AND store = $2`,
		assetID, storeName, state); err != nil {
		t.Fatalf("setting copy state of asset %d in %s: %v", assetID, storeName, err)
	}
}

func (e storeScopeEnv) copyState(t *testing.T, assetID int64, storeName string) string {
	t.Helper()
	var state string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, assetID, storeName).Scan(&state); err != nil {
		t.Fatalf("reading copy state of asset %d in %s: %v", assetID, storeName, err)
	}
	return state
}

func (e storeScopeEnv) trash(t *testing.T, recordingID int64) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE recordings SET deleted_at = now() - interval '40 days' WHERE id = $1`, recordingID); err != nil {
		t.Fatalf("trashing recording %d: %v", recordingID, err)
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
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'until_encoded', ARRAY['h264'], false)`, recordingID); err != nil {
		t.Fatalf("seeding until_encoded policy: %v", err)
	}
	return recordingID, originalID, derived
}

func (e storeScopeEnv) functionAssetIDs(t *testing.T) []int64 {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT asset_id FROM until_encoded_deletable_originals($1) ORDER BY asset_id`, dbtest.StoreName)
	if err != nil {
		t.Fatalf("querying until_encoded_deletable_originals(%q): %v", dbtest.StoreName, err)
	}
	defer rows.Close()
	return scanInt64s(t, rows)
}

func (e storeScopeEnv) legacyViewAssetIDs(t *testing.T) []int64 {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT asset_id FROM until_encoded_deletable_originals ORDER BY asset_id`)
	if err != nil {
		t.Fatalf("querying legacy until_encoded_deletable_originals view: %v", err)
	}
	defer rows.Close()
	return scanInt64s(t, rows)
}

type int64Rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanInt64s(t *testing.T, rows int64Rows) []int64 {
	t.Helper()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning asset id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading asset ids: %v", err)
	}
	return ids
}

// 派生物（encoded / thumbnail / seek_tiles）の active コピーが desired_store 以外にしか無い原本は対象外で、
// desired_store に揃えば対象になる。種別ごとに別の条件なので種別ごとに確かめる。
func TestUntilEncodedDeletableRequiresDerivedCopiesInDesiredStore(t *testing.T) {
	for _, kind := range []string{store.AssetKindEncoded, store.AssetKindThumbnail, store.AssetKindSeekTiles} {
		t.Run(kind, func(t *testing.T) {
			env := newStoreScopeEnv(t)
			_, originalID, derived := env.untilEncodedFixture(t)
			if got := env.functionAssetIDs(t); !slices.Equal(got, []int64{originalID}) {
				t.Fatalf("complete fixture qualifying originals = %v, want [%d]", got, originalID)
			}

			env.moveCopy(t, derived[kind], otherStoreName)
			if got := env.functionAssetIDs(t); len(got) != 0 {
				t.Fatalf("%s copy only in %s: originals for %s = %v, want none", kind, otherStoreName, dbtest.StoreName, got)
			}

			if err := sqlcgen.New(env.pool).UpsertMediaAssetCopy(context.Background(), sqlcgen.UpsertMediaAssetCopyParams{
				MediaAssetID: derived[kind], Store: dbtest.StoreName, RelPath: "sites/default/store-scope-back-" + kind, State: "active",
			}); err != nil {
				t.Fatalf("adding %s copy to %s: %v", kind, dbtest.StoreName, err)
			}
			if got := env.functionAssetIDs(t); !slices.Equal(got, []int64{originalID}) {
				t.Fatalf("%s copy back in %s: originals = %v, want [%d]", kind, dbtest.StoreName, got, originalID)
			}
		})
	}
}

// 旧 binary が読む旧 view は、関数と同じ原本を返す（対象になる録画とならない録画の両方）。
func TestLegacyUntilEncodedViewMatchesFunction(t *testing.T) {
	env := newStoreScopeEnv(t)
	_, qualifying, _ := env.untilEncodedFixture(t)
	_, incompleteOriginal, incompleteDerived := env.untilEncodedFixture(t)
	env.setCopyState(t, incompleteDerived[store.AssetKindThumbnail], dbtest.StoreName, "deleted")

	fn := env.functionAssetIDs(t)
	view := env.legacyViewAssetIDs(t)
	if !slices.Equal(fn, []int64{qualifying}) {
		t.Fatalf("function originals = %v, want [%d] (and not %d)", fn, qualifying, incompleteOriginal)
	}
	if !slices.Equal(view, fn) {
		t.Fatalf("legacy view originals = %v, function originals = %v, want equal", view, fn)
	}

	var relPath, state string
	var size int64
	if err := env.pool.QueryRow(context.Background(),
		`SELECT rel_path, size_bytes, state FROM until_encoded_deletable_originals WHERE asset_id = $1`, qualifying).Scan(&relPath, &size, &state); err != nil {
		t.Fatalf("reading legacy view columns: %v", err)
	}
	if relPath == "" || size != 1 || state != "active" {
		t.Fatalf("legacy view columns = (%q, %d, %q), want the media_assets row's rel_path, size 1, active", relPath, size, state)
	}
}

// 2 つの置き場で資格を満たす原本も、旧 view は 1 行しか返さない。
func TestLegacyUntilEncodedViewReturnsOneRowPerOriginal(t *testing.T) {
	env := newStoreScopeEnv(t)
	_, originalID, derived := env.untilEncodedFixture(t)
	for _, id := range []int64{originalID, derived[store.AssetKindEncoded], derived[store.AssetKindThumbnail], derived[store.AssetKindSeekTiles]} {
		if err := sqlcgen.New(env.pool).UpsertMediaAssetCopy(context.Background(), sqlcgen.UpsertMediaAssetCopyParams{
			MediaAssetID: id, Store: otherStoreName, RelPath: fmt.Sprintf("sites/default/store-scope-other-%d", id), State: "active",
		}); err != nil {
			t.Fatalf("adding other-store copy for asset %d: %v", id, err)
		}
	}
	if got := env.legacyViewAssetIDs(t); !slices.Equal(got, []int64{originalID}) {
		t.Fatalf("legacy view originals = %v, want one row [%d]", got, originalID)
	}
}

// forEachCopyStore は同じ検査を、コピーが別の置き場にある場合と自分の置き場にある場合で走らせる。
func forEachCopyStore(t *testing.T, check func(t *testing.T, copyStore string, own bool)) {
	t.Helper()
	for _, copyStore := range []string{otherStoreName, dbtest.StoreName} {
		t.Run("copy in "+copyStore, func(t *testing.T) { check(t, copyStore, copyStore == dbtest.StoreName) })
	}
}

func wantCopyState(own bool, ifOwn, ifOther string) string {
	if own {
		return ifOwn
	}
	return ifOther
}

// 削除の一覧クエリは worker 自身の置き場のコピーだけを見る。同じ論理資産が別の置き場にだけコピーを持つときは
// 何も返さず、同じ状態のコピーが自分の置き場にあれば返す（両方向）。
func TestMediaDeleteListsIgnoreOtherStoreCopies(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	cases := map[string]func(t *testing.T, env storeScopeEnv, copyStore string) (listed []int64, id int64){
		"trash": func(t *testing.T, env storeScopeEnv, copyStore string) ([]int64, int64) {
			rec := env.recording(t)
			id := env.asset(t, rec, store.AssetKindOriginal, nil)
			env.moveCopy(t, id, copyStore)
			env.trash(t, rec)
			rows, err := env.media.ListTrashAssetsToDelete(ctx, cutoff, 10)
			if err != nil {
				t.Fatal(err)
			}
			return mediaDeleteIDs(rows), id
		},
		"until_encoded": func(t *testing.T, env storeScopeEnv, copyStore string) ([]int64, int64) {
			_, original, _ := env.untilEncodedFixture(t)
			env.moveCopy(t, original, copyStore)
			rows, err := env.media.ListUntilEncodedOriginalsToDelete(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			return mediaDeleteIDs(rows), original
		},
		"removed encoded": func(t *testing.T, env storeScopeEnv, copyStore string) ([]int64, int64) {
			rec := env.recording(t)
			env.asset(t, rec, store.AssetKindOriginal, nil)
			if _, err := env.pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', '{}', false)`, rec); err != nil {
				t.Fatal(err)
			}
			profile := "h264"
			id := env.asset(t, rec, store.AssetKindEncoded, &profile)
			if err := sqlcgen.New(env.pool).InsertEncodedAssetRemovalRequest(ctx, sqlcgen.InsertEncodedAssetRemovalRequestParams{RecordingID: rec, Profile: profile}); err != nil {
				t.Fatal(err)
			}
			env.moveCopy(t, id, copyStore)
			rows, err := env.media.ListRemovedEncodedAssetsToDelete(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			return mediaDeleteIDs(rows), id
		},
		"pending": func(t *testing.T, env storeScopeEnv, copyStore string) ([]int64, int64) {
			rec := env.recording(t)
			id := env.asset(t, rec, store.AssetKindOriginal, nil)
			env.moveCopy(t, id, copyStore)
			env.setCopyState(t, id, copyStore, "deleting")
			env.trash(t, rec)
			rows, err := env.media.ListPendingDeleteAssets(ctx, cutoff, 10)
			if err != nil {
				t.Fatal(err)
			}
			return mediaDeleteIDs(rows), id
		},
		"unqualified deleting": func(t *testing.T, env storeScopeEnv, copyStore string) ([]int64, int64) {
			id := env.asset(t, env.recording(t), store.AssetKindOriginal, nil)
			env.moveCopy(t, id, copyStore)
			env.setCopyState(t, id, copyStore, "deleting")
			rows, err := env.media.ListUnqualifiedDeletingAssets(ctx, cutoff, 10)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, r := range rows {
				ids = append(ids, r.ID)
			}
			return ids, id
		},
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			forEachCopyStore(t, func(t *testing.T, copyStore string, own bool) {
				ids, id := list(t, newStoreScopeEnv(t), copyStore)
				assertListed(t, ids, id, own)
			})
		})
	}
}

// 削除の遷移クエリも worker 自身の置き場のコピーだけを動かす。別の置き場のコピーは状態を変えず、
// 副作用（外した版の要求行の削除）も起こさない。
func TestMediaDeleteTransitionsIgnoreOtherStoreCopies(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	t.Run("MarkUntilEncodedDeleting", func(t *testing.T) {
		forEachCopyStore(t, func(t *testing.T, copyStore string, own bool) {
			env := newStoreScopeEnv(t)
			rec, original, _ := env.untilEncodedFixture(t)
			env.moveCopy(t, original, copyStore)
			marked, err := env.media.MarkUntilEncodedDeleting(ctx, rec, original)
			if err != nil {
				t.Fatalf("MarkUntilEncodedDeleting: %v", err)
			}
			if marked != own {
				t.Fatalf("MarkUntilEncodedDeleting = %v, want %v", marked, own)
			}
			if got, want := env.copyState(t, original, copyStore), wantCopyState(own, "deleting", "active"); got != want {
				t.Fatalf("copy state = %q, want %q", got, want)
			}
		})
	})
	t.Run("RevertToActive", func(t *testing.T) {
		forEachCopyStore(t, func(t *testing.T, copyStore string, own bool) {
			env := newStoreScopeEnv(t)
			id := env.asset(t, env.recording(t), store.AssetKindOriginal, nil)
			env.moveCopy(t, id, copyStore)
			env.setCopyState(t, id, copyStore, "deleting")
			reverted, err := env.media.RevertToActive(ctx, id, cutoff)
			if err != nil {
				t.Fatalf("RevertToActive: %v", err)
			}
			if reverted != own {
				t.Fatalf("RevertToActive = %v, want %v", reverted, own)
			}
			if got, want := env.copyState(t, id, copyStore), wantCopyState(own, "active", "deleting"); got != want {
				t.Fatalf("copy state = %q, want %q", got, want)
			}
		})
	})
	t.Run("MarkDeleting", func(t *testing.T) {
		forEachCopyStore(t, func(t *testing.T, copyStore string, own bool) {
			env := newStoreScopeEnv(t)
			id := env.asset(t, env.recording(t), store.AssetKindOriginal, nil)
			env.moveCopy(t, id, copyStore)
			if err := env.media.MarkDeleting(ctx, id); err != nil {
				t.Fatalf("MarkDeleting: %v", err)
			}
			if got, want := env.copyState(t, id, copyStore), wantCopyState(own, "deleting", "active"); got != want {
				t.Fatalf("copy state = %q, want %q", got, want)
			}
		})
	})
	t.Run("MarkDeleted", func(t *testing.T) {
		forEachCopyStore(t, func(t *testing.T, copyStore string, own bool) {
			env := newStoreScopeEnv(t)
			rec := env.recording(t)
			profile := "h264"
			id := env.asset(t, rec, store.AssetKindEncoded, &profile)
			if err := sqlcgen.New(env.pool).InsertEncodedAssetRemovalRequest(ctx, sqlcgen.InsertEncodedAssetRemovalRequestParams{RecordingID: rec, Profile: profile}); err != nil {
				t.Fatal(err)
			}
			env.moveCopy(t, id, copyStore)
			env.setCopyState(t, id, copyStore, "deleting")
			if err := env.media.MarkDeleted(ctx, id); err != nil {
				t.Fatalf("MarkDeleted: %v", err)
			}
			if got, want := env.copyState(t, id, copyStore), wantCopyState(own, "deleted", "deleting"); got != want {
				t.Fatalf("copy state = %q, want %q", got, want)
			}
			var requestExists bool
			if err := env.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM encoded_asset_removal_requests WHERE recording_id = $1 AND profile = $2)`, rec, profile).Scan(&requestExists); err != nil {
				t.Fatal(err)
			}
			if requestExists == own {
				t.Fatalf("removal request exists = %v after MarkDeleted for a copy in %s, want %v", requestExists, copyStore, !own)
			}
		})
	})
}

func mediaDeleteIDs(rows []store.MediaDeleteAsset) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func assertListed(t *testing.T, ids []int64, id int64, want bool) {
	t.Helper()
	if got := slices.Contains(ids, id); got != want {
		t.Fatalf("asset %d listed = %v (ids %v), want %v", id, got, ids, want)
	}
}

// 削除遷移は判定の前に対象コピー行をロックする。別の tx がコピーを deleted に確定する最中は待ち、
// commit 後にその結果を見て、deleted を deleting に戻さない。
func TestMarkDeletingWaitsForConcurrentCopyTransitionAndDoesNotResurrect(t *testing.T) {
	env := newStoreScopeEnv(t)
	ctx := context.Background()
	id := env.asset(t, env.recording(t), store.AssetKindOriginal, nil)

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx,
		`UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1 AND store = $2`,
		id, dbtest.StoreName); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- env.media.MarkDeleting(ctx, id) }()

	waitForContractLockWaiter(t, env.pool, "MarkDeleting")
	select {
	case err := <-done:
		t.Fatalf("MarkDeleting returned (%v) before the concurrent transition committed, want it to wait", err)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("MarkDeleting after commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("MarkDeleting did not finish after the concurrent transition committed")
	}
	if got := env.copyState(t, id, dbtest.StoreName); got != "deleted" {
		t.Fatalf("copy state = %q, want deleted (MarkDeleting must not move a deleted copy back to deleting)", got)
	}
}

// 置き場を限定しない読み手（api）は、複数の置き場に active コピーがあれば store 名順で先頭の 1 件を選ぶ。
// deleting のコピーは候補に入らない。設定済みの置き場を持つ読み手はその置き場のコピーを選ぶ。
func TestMediaReadersPickActiveCopyDeterministically(t *testing.T) {
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
		// dbtest が作る test-store の active コピーに、先頭になる置き場の deleting と active を足す。
		for _, c := range []struct{ store, state string }{{"a-store", "deleting"}, {"b-store", "active"}, {"z-store", "active"}} {
			if err := sqlcgen.New(env.pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
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
	api := NewMedia(env.pool, "")
	readers := map[string]func(*Media) (store.MediaAsset, error){
		"original":      func(m *Media) (store.MediaAsset, error) { return m.GetOriginalMediaAssetForServing(ctx, rec) },
		"active origin": func(m *Media) (store.MediaAsset, error) { return m.GetActiveOriginalMediaAsset(ctx, rec) },
		"encoded":       func(m *Media) (store.MediaAsset, error) { return m.GetEncodedMediaAssetForServing(ctx, rec, profile) },
		"thumbnail":     func(m *Media) (store.MediaAsset, error) { return m.GetThumbnailMediaAssetForServing(ctx, rec) },
		"seek tiles":    func(m *Media) (store.MediaAsset, error) { return m.GetSeekTilesMediaAssetForServing(ctx, rec) },
	}
	for name, read := range readers {
		got, err := read(api)
		if err != nil {
			t.Fatalf("%s without a configured store: %v", name, err)
		}
		if got.Store != "b-store" {
			t.Errorf("%s without a configured store picked %q, want b-store (first active copy by store name; a-store is deleting)", name, got.Store)
		}
		got, err = read(NewMedia(env.pool, "z-store"))
		if err != nil || got.Store != "z-store" {
			t.Errorf("%s with z-store = (%+v, %v), want the z-store copy", name, got, err)
		}
	}
}

// 原本の削除は取り返せないので、desired_store が欠けた（NULL の）ときは全置き場には倒さず、何も返さない。
func TestUntilEncodedDeletableReturnsNothingForNullDesiredStore(t *testing.T) {
	env := newStoreScopeEnv(t)
	_, originalID, _ := env.untilEncodedFixture(t)
	if got := env.functionAssetIDs(t); !slices.Equal(got, []int64{originalID}) {
		t.Fatalf("until_encoded_deletable_originals(%q) = %v, want [%d]", dbtest.StoreName, got, originalID)
	}
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM until_encoded_deletable_originals(NULL)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("until_encoded_deletable_originals(NULL) returned %d rows, want 0 (must not fall open to all stores)", n)
	}
}

// until_encoded の原本は、派生物の desired 置き場（storage.derived）にエンコード済みのコピーがあるときだけ
// 削除候補になる。コピーを読む worker の置き場（test-store）とは別の値で、worker の置き場を代わりにしない。
func TestUntilEncodedUsesDerivedStoreNotWorkerStore(t *testing.T) {
	const derivedStore = "derived-store"
	ctx := context.Background()
	cases := []struct {
		name         string
		derivedStore string
		derivedCopy  string
		want         bool
	}{
		{"derived copies in the derived store", derivedStore, derivedStore, true},
		{"derived copies only in the worker store", derivedStore, dbtest.StoreName, false},
		{"no derived store configured", "", dbtest.StoreName, false},
		{"no derived store configured, copies in another store", "", derivedStore, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newStoreScopeEnv(t)
			rec, original, derived := env.untilEncodedFixture(t)
			for _, id := range derived {
				env.moveCopy(t, id, tc.derivedCopy)
			}
			media := NewMedia(env.pool, dbtest.StoreName).WithDerivedStore(tc.derivedStore)
			rows, err := media.ListUntilEncodedOriginalsToDelete(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			assertListed(t, mediaDeleteIDs(rows), original, tc.want)
			marked, err := media.MarkUntilEncodedDeleting(ctx, rec, original)
			if err != nil {
				t.Fatal(err)
			}
			if marked != tc.want {
				t.Fatalf("MarkUntilEncodedDeleting = %v, want %v", marked, tc.want)
			}
		})
	}
}
