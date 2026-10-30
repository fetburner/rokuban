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
	return storeScopeEnv{pool: pool, media: NewMedia(pool, dbtest.StoreName, dbtest.StoreName)}
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

			if _, err := sqlcgen.New(env.pool).UpsertMediaAssetCopy(context.Background(), sqlcgen.UpsertMediaAssetCopyParams{
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

func TestDerivedReadersRequireCurrentGenerationAcrossStores(t *testing.T) {
	ctx := context.Background()
	env := newStoreScopeEnv(t)
	recordingID, originalID, assets := env.untilEncodedFixture(t)
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
	if got := env.functionAssetIDs(t); len(got) != 0 {
		t.Fatalf("until_encoded originals with only an old-generation copy = %v, want none", got)
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
	if got := env.functionAssetIDs(t); len(got) != 0 {
		t.Fatalf("until_encoded originals without the desired-store current copy = %v, want none", got)
	}

	if _, err := sqlcgen.New(env.pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: encodedID, Store: dbtest.StoreName, RelPath: "encoded/h264.g1.mp4", Generation: 1, State: "active",
	}); err != nil {
		t.Fatalf("adding current-generation copy in desired store: %v", err)
	}
	if got := env.functionAssetIDs(t); !slices.Equal(got, []int64{originalID}) {
		t.Fatalf("until_encoded originals after desired-store current publish = %v, want [%d]", got, originalID)
	}
}

func stringPointer(value string) *string { return &value }

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
			rows, err := env.media.ListPendingDeleteAssets(ctx, cutoff, time.Time{}, nil, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			return mediaDeleteIDs(rows), id
		},
		"unqualified deleting": func(t *testing.T, env storeScopeEnv, copyStore string) ([]int64, int64) {
			id := env.asset(t, env.recording(t), store.AssetKindOriginal, nil)
			env.moveCopy(t, id, copyStore)
			env.setCopyState(t, id, copyStore, "deleting")
			rows, err := env.media.ListUnqualifiedDeletingAssets(ctx, cutoff, time.Time{}, nil, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, r := range rows {
				ids = append(ids, r.MediaAssetID)
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
			copyID := dbtest.MediaAssetCopyID(t, env.pool, original, copyStore)
			marked, err := env.media.MarkUntilEncodedDeleting(ctx, rec, original, copyID)
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
			copyID := dbtest.MediaAssetCopyID(t, env.pool, id, copyStore)
			reverted, err := env.media.RevertToActive(ctx, copyID, cutoff, time.Time{}, nil, "")
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
	t.Run("RevertDeletedCopy", func(t *testing.T) {
		env := newStoreScopeEnv(t)
		id := env.asset(t, env.recording(t), store.AssetKindOriginal, nil)
		copyID := dbtest.MediaAssetCopyID(t, env.pool, id, dbtest.StoreName)
		env.setCopyState(t, id, dbtest.StoreName, "deleted")
		reverted, err := env.media.RevertToActive(ctx, copyID, cutoff, time.Time{}, nil, "")
		if err != nil {
			t.Fatalf("RevertToActive for deleted copy: %v", err)
		}
		if reverted {
			t.Fatal("RevertToActive changed a deleted physical copy to active")
		}
		if got := env.copyState(t, id, dbtest.StoreName); got != "deleted" {
			t.Fatalf("deleted copy state after revert = %q, want deleted", got)
		}
	})
	t.Run("MarkDeleting", func(t *testing.T) {
		forEachCopyStore(t, func(t *testing.T, copyStore string, own bool) {
			env := newStoreScopeEnv(t)
			id := env.asset(t, env.recording(t), store.AssetKindOriginal, nil)
			env.moveCopy(t, id, copyStore)
			copyID := dbtest.MediaAssetCopyID(t, env.pool, id, copyStore)
			marked, err := env.media.MarkDeleting(ctx, copyID, "active")
			if err != nil {
				t.Fatalf("MarkDeleting: %v", err)
			}
			if marked != own {
				t.Fatalf("MarkDeleting = %v, want %v", marked, own)
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
			copyID := dbtest.MediaAssetCopyID(t, env.pool, id, copyStore)
			if err := env.media.MarkDeleted(ctx, copyID); err != nil {
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

func TestRevertMediaAssetCopyToActiveRequiresDeletingState(t *testing.T) {
	env := newStoreScopeEnv(t)
	ctx := context.Background()
	assetID := env.asset(t, env.recording(t), store.AssetKindOriginal, nil)
	copyID := dbtest.MediaAssetCopyID(t, env.pool, assetID, dbtest.StoreName)
	env.setCopyState(t, assetID, dbtest.StoreName, "deleted")

	updated, err := sqlcgen.New(env.pool).RevertMediaAssetCopyToActive(ctx, sqlcgen.RevertMediaAssetCopyToActiveParams{
		CopyID: copyID, StoreName: dbtest.StoreName,
		GraceCutoff:   time.Now().Add(-30 * 24 * time.Hour),
		DesiredStore:  stringPointer(dbtest.StoreName),
		OverlapCutoff: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("reverting a deleted physical copy: %v", err)
	}
	if updated != 0 {
		t.Fatalf("revert updated %d rows for a deleted copy, want 0", updated)
	}
	if got := env.copyState(t, assetID, dbtest.StoreName); got != "deleted" {
		t.Fatalf("deleted copy state after direct revert query = %q, want deleted", got)
	}
}

func mediaDeleteIDs(rows []store.MediaDeleteAsset) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.MediaAssetID)
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
	copyID := dbtest.MediaAssetCopyID(t, env.pool, id, dbtest.StoreName)
	go func() {
		_, err := env.media.MarkDeleting(ctx, copyID, "active")
		done <- err
	}()

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
	api := NewMedia(env.pool, "", "")
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
		got, err = read(NewMedia(env.pool, "z-store", ""))
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
			media := NewMedia(env.pool, dbtest.StoreName, tc.derivedStore)
			rows, err := media.ListUntilEncodedOriginalsToDelete(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			assertListed(t, mediaDeleteIDs(rows), original, tc.want)
			copyID := dbtest.MediaAssetCopyID(t, env.pool, original, dbtest.StoreName)
			marked, err := media.MarkUntilEncodedDeleting(ctx, rec, original, copyID)
			if err != nil {
				t.Fatal(err)
			}
			if marked != tc.want {
				t.Fatalf("MarkUntilEncodedDeleting = %v, want %v", marked, tc.want)
			}
		})
	}
}

// removed_encoded_assets の「他に現行のコピーがある版が残る」は、active_media_asset_copies の現行判定を使う。
// 旧世代のコピーしか active でない版を残る版に数えると、現行ファイルが無いのに最後の版を消してしまう。
func TestRemovedEncodedAssetsCountsOnlyCurrentGenerationCopies(t *testing.T) {
	ctx := context.Background()
	env := newStoreScopeEnv(t)
	recordingID := env.recording(t)
	removed, kept := "h264", "h265"
	encoded := func(profile *string) int64 {
		return dbtest.MediaAsset(t, env.pool, recordingID, func(p *dbtest.MediaAssetParams) {
			p.Kind = store.AssetKindEncoded
			p.Profile = profile
			p.RelPath = fmt.Sprintf("encoded/%d.%s.mp4", recordingID, *profile)
		})
	}
	removedID, keptID := encoded(&removed), encoded(&kept)
	if _, err := env.pool.Exec(ctx, `
INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', ARRAY['h265'], false)`, recordingID); err != nil {
		t.Fatalf("seeding encode policy: %v", err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO encoded_asset_removal_requests (recording_id, profile) VALUES ($1, 'h264')`, recordingID); err != nil {
		t.Fatalf("seeding removal request: %v", err)
	}
	removable := func() bool {
		t.Helper()
		var exists bool
		if err := env.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM removed_encoded_assets WHERE asset_id = $1)`, removedID).Scan(&exists); err != nil {
			t.Fatalf("querying removed_encoded_assets: %v", err)
		}
		return exists
	}
	if !removable() {
		t.Fatal("removed version is not removable while the kept version has a current active copy")
	}

	// 残る版の論理世代だけを進める。active なコピーは旧世代（0）のままになる。
	if _, err := env.pool.Exec(ctx, `UPDATE media_assets SET derived_generation = 1 WHERE id = $1`, keptID); err != nil {
		t.Fatalf("advancing the kept version's generation: %v", err)
	}
	if removable() {
		t.Fatal("removed version is removable although the kept version has only an old-generation active copy")
	}

	if _, err := sqlcgen.New(env.pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: keptID, Store: dbtest.StoreName, RelPath: "encoded/h265.g1.mp4", Generation: 1, State: "active",
	}); err != nil {
		t.Fatalf("adding the current-generation copy: %v", err)
	}
	if !removable() {
		t.Fatal("removed version is not removable once the kept version has a current-generation copy")
	}
}
