package pgstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestMediaAssetsCopyColumnReadersAreDropped(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	var legacyViewExists, storeFunctionExists bool
	if err := pool.QueryRow(ctx, `
SELECT to_regclass('public.until_encoded_deletable_originals') IS NOT NULL,
       to_regprocedure('public.until_encoded_deletable_originals(text)') IS NOT NULL`).
		Scan(&legacyViewExists, &storeFunctionExists); err != nil {
		t.Fatalf("checking until-encoded relations: %v", err)
	}
	if legacyViewExists || !storeFunctionExists {
		t.Fatalf("legacy view/function exists = %v/%v, want false/true", legacyViewExists, storeFunctionExists)
	}

	rows, err := pool.Query(ctx, `
SELECT column_name FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'removed_encoded_assets'
ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("reading removed_encoded_assets shape: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scanning removed_encoded_assets column: %v", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading removed_encoded_assets columns: %v", err)
	}
	if fmt.Sprint(columns) != "[asset_id recording_id]" {
		t.Fatalf("removed_encoded_assets columns = %v, want [asset_id recording_id]", columns)
	}
}

func TestAdoptUnassignedStoreRows(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	media := NewMedia(pool, "tokyo", "")
	recordingID := dbtest.Recording(t, pool)
	copylessAssetID := dbtest.MediaAsset(t, pool, recordingID)
	// コピー行を持たない論理資産。adopt はコピーを作らず、worker 起動時の検査が拒む。
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, copylessAssetID); err != nil {
		t.Fatalf("removing fixture copy to simulate a copyless asset: %v", err)
	}
	// 別の置き場のコピーには adopt は触れない。
	otherAssetID := dbtest.MediaAsset(t, pool, dbtest.Recording(t, pool))
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'osaka' WHERE media_asset_id = $1`, otherAssetID); err != nil {
		t.Fatalf("assigning fixture copy to osaka: %v", err)
	}
	syncedAssetID := dbtest.MediaAsset(t, pool, dbtest.Recording(t, pool))
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'tokyo' WHERE media_asset_id = $1`, syncedAssetID); err != nil {
		t.Fatalf("assigning fixture copy to tokyo: %v", err)
	}
	derivedAssetID := dbtest.MediaAsset(t, pool, dbtest.Recording(t, pool), func(p *dbtest.MediaAssetParams) {
		p.Kind = "encoded"
		p.Profile = stringPointer("cut")
	})
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET derived_generation = 4 WHERE id = $1`, derivedAssetID); err != nil {
		t.Fatalf("seeding migrated derived generation: %v", err)
	}
	// 物理コピーは世代 5 にあるが、論理資産の世代は 4 のまま。
	if _, err := sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: derivedAssetID, Store: "tokyo", RelPath: "sites/default/advanced_cut.g5.mp4", Generation: 5, State: "active",
	}); err != nil {
		t.Fatalf("seeding advanced derived copy path: %v", err)
	}

	unassigned := NewMedia(pool, "", "")
	if err := unassigned.UpsertIngestProgress(ctx, store.IngestProgress{
		RecordingID: recordingID, WrittenBytes: 10, TempRelPath: "sites/default/.rokuban-ingest-test", TempGeneration: 1,
	}); err != nil {
		t.Fatalf("seeding ingest progress: %v", err)
	}
	if err := unassigned.UpsertOrphanFile(ctx, "sites/default/orphan.m2ts"); err != nil {
		t.Fatalf("seeding orphan observation: %v", err)
	}
	missingCopyID := dbtest.MediaAssetCopyID(t, pool, syncedAssetID, "tokyo")
	if err := media.UpsertMissingMediaAsset(ctx, missingCopyID); err != nil {
		t.Fatalf("seeding missing asset observation: %v", err)
	}
	if err := unassigned.UpsertStorageSync(ctx, store.StorageObservationInput{
		Root: "media", Path: "/media", TotalBytes: 100, UsedBytes: 25, AvailableBytes: 75,
	}); err != nil {
		t.Fatalf("seeding storage observation: %v", err)
	}
	if err := media.RequireAdoptedStoreRows(ctx); err == nil {
		t.Fatal("RequireAdoptedStoreRows succeeded while blank-store or copyless rows remained")
	}

	if err := media.AdoptUnassignedStoreRows(ctx); err != nil {
		t.Fatalf("AdoptUnassignedStoreRows: %v", err)
	}
	for _, tableAndColumn := range [][2]string{
		{"recording_ingest_progress", "temp_store"},
		{"orphan_files", "store"},
		{"storage_sync", "store"},
	} {
		table, column := tableAndColumn[0], tableAndColumn[1]
		query := fmt.Sprintf(`SELECT count(*) FILTER (WHERE %s = ''), count(*) FILTER (WHERE %s = 'tokyo') FROM %s`, column, column, table)
		var unassignedRows, adopted int
		if err := pool.QueryRow(ctx, query).Scan(&unassignedRows, &adopted); err != nil {
			t.Fatalf("counting stores in %s: %v", table, err)
		}
		if unassignedRows != 0 || adopted == 0 {
			t.Errorf("%s.%s counts = unassigned %d / adopted %d, want 0 / positive", table, column, unassignedRows, adopted)
		}
	}
	var missingCopyExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM missing_media_assets WHERE media_asset_copy_id = $1)`, missingCopyID).Scan(&missingCopyExists); err != nil {
		t.Fatalf("checking physical-copy missing observation after adoption: %v", err)
	}
	if !missingCopyExists {
		t.Fatal("physical-copy missing observation was lost during adoption")
	}

	// adopt はコピー行を作らない。コピーの無い論理資産は残り、worker 起動時の検査が拒み続ける。
	var copylessCopies, otherTokyoCopies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1`, copylessAssetID).Scan(&copylessCopies); err != nil {
		t.Fatalf("counting copies of the copyless asset: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, otherAssetID).Scan(&otherTokyoCopies); err != nil {
		t.Fatalf("counting tokyo copies of an osaka-only asset: %v", err)
	}
	if copylessCopies != 0 || otherTokyoCopies != 0 {
		t.Fatalf("adopt created copies: copyless asset %d, osaka-only asset in tokyo %d; want 0 / 0", copylessCopies, otherTokyoCopies)
	}
	if err := media.RequireAdoptedStoreRows(ctx); err == nil {
		t.Fatal("RequireAdoptedStoreRows succeeded while a copyless asset remained")
	}
	if _, err := sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: copylessAssetID, Store: "tokyo", RelPath: "sites/default/restored.m2ts", Generation: 0, State: "active",
	}); err != nil {
		t.Fatalf("restoring the copy of the copyless asset: %v", err)
	}
	if err := media.RequireAdoptedStoreRows(ctx); err != nil {
		t.Fatalf("RequireAdoptedStoreRows after every asset has a copy: %v", err)
	}

	var generation int64
	if err := pool.QueryRow(ctx, `SELECT derived_generation FROM media_assets WHERE id = $1`, derivedAssetID).Scan(&generation); err != nil {
		t.Fatalf("reading adopted derived generation: %v", err)
	}
	if generation != 5 {
		t.Fatalf("adopted derived_generation = %d, want 5 from the copy path", generation)
	}
}

func TestListStorageSyncWithoutConfiguredStoreIncludesNamedObservations(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	workerMedia := NewMedia(pool, "tokyo", "")
	if err := workerMedia.UpsertStorageSync(ctx, store.StorageObservationInput{
		Root: "media", Path: "/media", TotalBytes: 100, UsedBytes: 25, AvailableBytes: 75,
	}); err != nil {
		t.Fatalf("recording worker storage observation: %v", err)
	}

	apiMedia := NewMedia(pool, "tokyo", "")
	rows, err := apiMedia.ListStorageSync(ctx)
	if err != nil {
		t.Fatalf("listing storage observations without configured store: %v", err)
	}
	if len(rows) != 1 || rows[0].Root != "media" || rows[0].Path != "/media" {
		t.Fatalf("API-only storage observations = %+v, want named worker observation", rows)
	}
}

func TestMediaAssetCopyTracksDeleteLifecycle(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'tokyo' WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("assigning fixture copy store: %v", err)
	}
	media := NewMedia(pool, "tokyo", "")
	copyID := dbtest.MediaAssetCopyID(t, pool, assetID, "tokyo")

	if marked, err := media.MarkDeleting(ctx, copyID, "active"); err != nil || !marked {
		t.Fatalf("MarkDeleting: %v", err)
	}
	assertCopyState(t, pool, assetID, "tokyo", "deleting")
	if err := media.MarkDeleted(ctx, copyID); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	assertCopyState(t, pool, assetID, "tokyo", "deleted")
	var hasDeletedAt bool
	if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID).Scan(&hasDeletedAt); err != nil {
		t.Fatalf("reading deleted copy timestamp: %v", err)
	}
	if !hasDeletedAt {
		t.Fatal("deleted copy has no deleted_at timestamp")
	}
}

func TestMediaAssetDeleteIgnoresAssetWithoutCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("removing fixture copy: %v", err)
	}
	media := NewMedia(pool, "tokyo", "")
	if err := media.RequireAdoptedStoreRows(ctx); err == nil {
		t.Fatal("RequireAdoptedStoreRows succeeded while the physical copy was missing")
	}
	if marked, err := media.MarkDeleting(ctx, 0, "active"); err != nil || marked {
		t.Fatalf("MarkDeleting with an unknown physical copy = (%v, %v), want (false, nil)", marked, err)
	}
	var copies int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1`, assetID).Scan(&copies); err != nil {
		t.Fatalf("counting physical copies: %v", err)
	}
	if copies != 0 {
		t.Fatalf("physical copy count = %d, want 0", copies)
	}
}

func TestMediaAssetCopiesAcceptLeadingHyphenStoreName(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID, Store: "-tokyo", RelPath: "tokyo/original.m2ts", State: "active",
	}); err != nil {
		t.Fatalf("storing accepted leading-hyphen name: %v", err)
	}
}

func TestRegisterInPlaceRejectsDeletingCopyPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	media := NewMedia(pool, dbtest.StoreName, "")
	copyID := dbtest.MediaAssetCopyID(t, pool, assetID, dbtest.StoreName)
	if marked, err := media.MarkDeleting(ctx, copyID, "active"); err != nil || !marked {
		t.Fatalf("MarkDeleting: %v", err)
	}

	var relPath string
	if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, assetID, dbtest.StoreName).Scan(&relPath); err != nil {
		t.Fatalf("reading deleting asset path: %v", err)
	}
	// deleting のファイルは unlink される。同じパスの登録を通すと、消えるファイルを公開してしまう。
	_, err := media.RegisterInPlace(ctx, store.InPlaceInput{
		Assets: []store.InPlaceAsset{{Kind: store.AssetKindOriginal, RelPath: relPath, SizeBytes: 1}},
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("RegisterInPlace over a deleting copy error = %v, want store.ErrConflict", err)
	}
	assertCopyState(t, pool, assetID, dbtest.StoreName, "deleting")
}

func inPlaceEncodedInput(relPath string, size int64) store.InPlaceInput {
	profile := "h264"
	return store.InPlaceInput{
		Recording: store.InPlaceRecording{
			Source: "manual", Site: "default", NetworkID: 32736, ServiceID: 1024, EventID: 900,
			ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "in-place generation",
			ProgramStartAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), ProgramDurationMs: 60_000, Status: "finished",
		},
		Assets: []store.InPlaceAsset{{
			Kind: store.AssetKindEncoded, Profile: &profile, RelPath: relPath, SizeBytes: size,
		}},
	}
}

func TestRegisterInPlaceNewAssetTakesGenerationFromPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	media := NewMedia(pool, dbtest.StoreName, "")
	res, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/show.g8.mp4", 100))
	if err != nil {
		t.Fatalf("registering generation 8: %v", err)
	}
	var derived, copyGeneration int64
	if err := pool.QueryRow(ctx, `
SELECT a.derived_generation, c.generation
FROM media_assets a JOIN media_asset_copies c ON c.media_asset_id = a.id
WHERE a.id = $1`, res.AssetIDs[0]).Scan(&derived, &copyGeneration); err != nil {
		t.Fatalf("reading generations: %v", err)
	}
	if derived != 8 || copyGeneration != 8 {
		t.Fatalf("logical/copy generation = %d/%d, want 8/8", derived, copyGeneration)
	}
	assertCopyState(t, pool, res.AssetIDs[0], dbtest.StoreName, "active")
}

func TestRegisterInPlaceSamePathActiveReregistrationChangesNothing(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	media := NewMedia(pool, dbtest.StoreName, "")
	first, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/show.g8.mp4", 100))
	if err != nil {
		t.Fatalf("first registration: %v", err)
	}
	second, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/show.g8.mp4", 999))
	if err != nil {
		t.Fatalf("same-path active re-registration: %v", err)
	}
	if second.AssetIDs[0] != first.AssetIDs[0] {
		t.Fatalf("asset ids = %d, %d; want the same", first.AssetIDs[0], second.AssetIDs[0])
	}
	var size int64
	if err := pool.QueryRow(ctx, `SELECT size_bytes FROM media_assets WHERE id = $1`, first.AssetIDs[0]).Scan(&size); err != nil {
		t.Fatalf("reading size: %v", err)
	}
	if size != 100 {
		t.Fatalf("size_bytes = %d after a same-path re-registration, want unchanged 100", size)
	}
}

// 既存の論理資産に通すのは、この置き場に生きたコピーが無く・パスの世代が現行の世代と等しく・
// 録画がごみ箱にないときだけ。3 条件を 1 つずつ破って、それぞれ ErrConflict になることを固定する。
func TestRegisterInPlaceRejectsExistingAssetUnlessAllConditionsHold(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Media, *pgxpool.Pool, store.InPlaceResult) {
		t.Helper()
		pool := testutil.SetupDB(t)
		media := NewMedia(pool, dbtest.StoreName, "")
		first, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/show.g8.mp4", 100))
		if err != nil {
			t.Fatalf("registering generation 8: %v", err)
		}
		return media, pool, first
	}
	deleteCopies := func(t *testing.T, pool *pgxpool.Pool, assetID int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1`, assetID); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("live copy in this store", func(t *testing.T) {
		media, _, first := setup(t)
		// 世代は現行（8）と等しいが、この置き場に生きたコピーがある。
		_, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/other.g8.mp4", 80))
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("error = %v, want store.ErrConflict (asset %d)", err, first.AssetIDs[0])
		}
	})
	t.Run("live copy of another generation in this store", func(t *testing.T) {
		media, pool, first := setup(t)
		deleteCopies(t, pool, first.AssetIDs[0])
		// 旧世代のコピーがまだ生きている（overlap 中）。現行世代のパスでも、同じ置き場に 2 つ目の
		// 生きたコピーを足すことになるので拒否する。一意索引は世代が違えば止めないので、この条件は別に要る。
		if _, err := pool.Exec(ctx, `INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state) VALUES ($1, $2, 'encoded/show.g7.mp4', 7, 'active')`, first.AssetIDs[0], dbtest.StoreName); err != nil {
			t.Fatal(err)
		}
		_, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/show.g8.mp4", 80))
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("error = %v, want store.ErrConflict", err)
		}
	})
	t.Run("older generation", func(t *testing.T) {
		media, pool, first := setup(t)
		deleteCopies(t, pool, first.AssetIDs[0])
		_, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/show.g7.mp4", 80))
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("older generation error = %v, want store.ErrConflict", err)
		}
		var derived int64
		if err := pool.QueryRow(ctx, `SELECT derived_generation FROM media_assets WHERE id = $1`, first.AssetIDs[0]).Scan(&derived); err != nil {
			t.Fatal(err)
		}
		if derived != 8 {
			t.Fatalf("logical generation = %d after a rejected older generation, want 8", derived)
		}
	})
	t.Run("newer generation", func(t *testing.T) {
		media, pool, first := setup(t)
		deleteCopies(t, pool, first.AssetIDs[0])
		_, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/show.g9.mp4", 80))
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("newer generation error = %v, want store.ErrConflict", err)
		}
	})
	t.Run("trashed recording", func(t *testing.T) {
		media, pool, first := setup(t)
		deleteCopies(t, pool, first.AssetIDs[0])
		if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() WHERE id = $1`, first.RecordingID); err != nil {
			t.Fatal(err)
		}
		_, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/show.g8.mp4", 80))
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("trashed recording error = %v, want store.ErrConflict", err)
		}
	})
	t.Run("all conditions hold", func(t *testing.T) {
		media, pool, first := setup(t)
		deleteCopies(t, pool, first.AssetIDs[0])
		second, err := media.RegisterInPlace(ctx, inPlaceEncodedInput("encoded/show.g8.mp4", 80))
		if err != nil {
			t.Fatalf("re-registering a deleted copy at the current generation: %v", err)
		}
		if second.AssetIDs[0] != first.AssetIDs[0] {
			t.Fatalf("asset id = %d, want %d", second.AssetIDs[0], first.AssetIDs[0])
		}
		// 削除済みの行は残り、生きたコピーが新しく 1 行できる。
		var live, total int
		if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM active_media_asset_copies($1, NULL)),
       (SELECT count(*) FROM media_asset_copies WHERE media_asset_id = $1)`, first.AssetIDs[0]).Scan(&live, &total); err != nil {
			t.Fatal(err)
		}
		if live != 1 || total != 2 {
			t.Fatalf("current live copies/all copies = %d/%d, want 1/2", live, total)
		}
	})
}

// 同じ置き場・同じ世代の生きたコピーは 1 行だけ。規則の SQL を通らない書き手（並行する 2 本の登録の
// 2 本目）も、一意索引が拒否して ErrConflict に写る。
func TestMediaAssetCopiesRejectSecondLiveCopyOfSameStoreAndGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	err := upsertMediaAssetCopy(ctx, sqlcgen.New(pool), assetID, dbtest.StoreName, "other/second.m2ts", 0)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second live copy at the same store and generation error = %v, want store.ErrConflict", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "media_asset_copies_asset_store_generation_idx" {
		t.Fatalf("error = %v, want a violation of media_asset_copies_asset_store_generation_idx", err)
	}
	// 削除済みの行は数えない。
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatal(err)
	}
	if err := upsertMediaAssetCopy(ctx, sqlcgen.New(pool), assetID, dbtest.StoreName, "other/second.m2ts", 0); err != nil {
		t.Fatalf("live copy after the previous one was deleted: %v", err)
	}
}

// 競合したコピーの upsert は state を書かない。deleting を active に戻すと、unlink 予定のファイルを公開してしまう。
func TestUpsertMediaAssetCopyConflictKeepsState(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	media := NewMedia(pool, dbtest.StoreName, "")
	copyID := dbtest.MediaAssetCopyID(t, pool, assetID, dbtest.StoreName)
	if marked, err := media.MarkDeleting(ctx, copyID, "active"); err != nil || !marked {
		t.Fatalf("MarkDeleting: %v", err)
	}
	var relPath string
	if err := pool.QueryRow(ctx, `SELECT rel_path FROM media_asset_copies WHERE id = $1`, copyID).Scan(&relPath); err != nil {
		t.Fatal(err)
	}
	if err := upsertMediaAssetCopy(ctx, sqlcgen.New(pool), assetID, dbtest.StoreName, relPath, 0); err != nil {
		t.Fatalf("re-upserting the deleting copy: %v", err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM media_asset_copies WHERE id = $1`, copyID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "deleting" {
		t.Fatalf("copy state = %q after a conflicting upsert, want deleting", state)
	}
}

func TestDerivedMediaAssetReplacementsClearCatalogSHA256(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	generation := int64(0)
	digest := bytes.Repeat([]byte{0x5a}, 32)
	setCopy := func(assetID int64, relPath string) {
		t.Helper()
		if _, err := q.UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
			MediaAssetID: assetID, Store: dbtest.StoreName, RelPath: relPath, Generation: generation, State: "active",
		}); err != nil {
			t.Fatalf("seeding active copy for media asset %d: %v", assetID, err)
		}
	}

	setDigest := func(assetID int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE media_assets SET sha256 = $2 WHERE id = $1`, assetID, digest); err != nil {
			t.Fatalf("seeding catalog SHA-256 for media asset %d: %v", assetID, err)
		}
	}
	assertDigestCleared := func(assetID int64) {
		t.Helper()
		var got []byte
		if err := pool.QueryRow(ctx, `SELECT sha256 FROM media_assets WHERE id = $1`, assetID).Scan(&got); err != nil {
			t.Fatalf("reading catalog SHA-256 for media asset %d: %v", assetID, err)
		}
		if got != nil {
			t.Fatalf("media asset %d retained catalog SHA-256 %x after content replacement", assetID, got)
		}
	}

	encodedID, err := q.UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{Kind: "encoded",
		RecordingID: recordingID, Profile: &profile, SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating encoded asset: %v", err)
	}
	setCopy(encodedID, "encoded/h264.mp4")
	setDigest(encodedID)
	generation = 1
	if _, err := q.UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{Kind: "encoded",
		RecordingID: recordingID, Profile: &profile, SizeBytes: 11, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("replacing encoded asset: %v", err)
	}
	setCopy(encodedID, "encoded/h264.g1.mp4")
	assertDigestCleared(encodedID)

	generation = 0
	thumbnailID, err := q.UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{Kind: "thumbnail",
		RecordingID: recordingID, SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating thumbnail asset: %v", err)
	}
	setCopy(thumbnailID, "thumbnails/1.jpg")
	setDigest(thumbnailID)
	generation = 1
	if _, err := q.UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{Kind: "thumbnail",
		RecordingID: recordingID, SizeBytes: 11, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("recreating thumbnail asset: %v", err)
	}
	setCopy(thumbnailID, "thumbnails/1.g1.jpg")
	assertDigestCleared(thumbnailID)
	setDigest(thumbnailID)
	generation = 2
	if _, err := q.UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{
		RecordingID: recordingID, Kind: "thumbnail", SizeBytes: 12, DerivedGeneration: &generation,
	}); err != nil {
		t.Fatalf("replacing thumbnail path: %v", err)
	}
	setCopy(thumbnailID, "thumbnails/1.g2.jpg")
	assertDigestCleared(thumbnailID)

	generation = 0
	seekTilesID, err := q.UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{Kind: "seek_tiles",
		RecordingID: recordingID, SizeBytes: 10, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("creating seek tiles asset: %v", err)
	}
	setCopy(seekTilesID, "thumbnails/1_tiles.jpg")
	setDigest(seekTilesID)
	generation = 1
	replacedSeekTilesID, err := q.UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{Kind: "seek_tiles",
		RecordingID: recordingID, SizeBytes: 11, DerivedGeneration: &generation,
	})
	if err != nil {
		t.Fatalf("recreating seek tiles asset: %v", err)
	}
	if replacedSeekTilesID != seekTilesID {
		t.Fatalf("seek tiles replacement ID = %d, want existing asset %d", replacedSeekTilesID, seekTilesID)
	}
	setCopy(seekTilesID, "thumbnails/1_tiles.g1.jpg")
	assertDigestCleared(seekTilesID)
}

func TestCatalogExportSelectsStoreCopyAndKeepsFullInventory(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	assets := make(map[int64]int64)
	shaByRecording := make(map[int64]string)
	for i := range 2 {
		recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Title = fmt.Sprintf("catalog copy %d", i) })
		assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
			p.RelPath = fmt.Sprintf("legacy/%d.m2ts", recordingID)
		})
		assets[recordingID] = assetID
		if _, err := pool.Exec(ctx, `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
			t.Fatalf("removing default fixture copy: %v", err)
		}
		sum := make([]byte, 32)
		for n := range sum {
			sum[n] = byte(n + i)
		}
		shaByRecording[recordingID] = hex.EncodeToString(sum)
		if _, err := pool.Exec(ctx, `UPDATE media_assets SET sha256 = $2 WHERE id = $1`, assetID, sum); err != nil {
			t.Fatalf("setting SHA-256: %v", err)
		}
		for _, storeName := range []string{"tokyo", "osaka"} {
			if _, err := q.UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
				MediaAssetID: assetID, Store: storeName, RelPath: fmt.Sprintf("%s/%d.m2ts", storeName, recordingID), State: "active",
			}); err != nil {
				t.Fatalf("adding %s copy: %v", storeName, err)
			}
		}
	}

	tokyo, err := NewCatalog(pool, []string{"default"}, "tokyo").Export(ctx)
	if err != nil {
		t.Fatalf("exporting tokyo catalog: %v", err)
	}
	osaka, err := NewCatalog(pool, []string{"default"}, "osaka").Export(ctx)
	if err != nil {
		t.Fatalf("exporting osaka catalog: %v", err)
	}
	if len(tokyo.Recordings) != 2 || len(osaka.Recordings) != len(tokyo.Recordings) {
		t.Fatalf("recording inventory tokyo/osaka = %d/%d, want full inventory 2/2", len(tokyo.Recordings), len(osaka.Recordings))
	}
	if len(tokyo.MediaAssets) != 2 || len(osaka.MediaAssets) != len(tokyo.MediaAssets) {
		t.Fatalf("asset inventory tokyo/osaka = %d/%d, want full inventory 2/2", len(tokyo.MediaAssets), len(osaka.MediaAssets))
	}
	byRecording := func(rows []catalogAssetCopy) map[int64]catalogAssetCopy {
		out := make(map[int64]catalogAssetCopy, len(rows))
		for _, row := range rows {
			out[row.recordingID] = row
		}
		return out
	}
	gotTokyo := byRecording(mediaAssetCopyRows(tokyo.MediaAssets))
	gotOsaka := byRecording(mediaAssetCopyRows(osaka.MediaAssets))
	for recordingID := range assets {
		wantSHA := shaByRecording[recordingID]
		if got := gotTokyo[recordingID]; got.store != "tokyo" || got.relPath != fmt.Sprintf("tokyo/%d.m2ts", recordingID) || got.sha256 != wantSHA {
			t.Errorf("tokyo catalog asset for recording %d = %+v, want tokyo copy path and logical SHA %s", recordingID, got, wantSHA)
		}
		if got := gotOsaka[recordingID]; got.store != "osaka" || got.relPath != fmt.Sprintf("osaka/%d.m2ts", recordingID) || got.sha256 != wantSHA {
			t.Errorf("osaka catalog asset for recording %d = %+v, want osaka copy path and logical SHA %s", recordingID, got, wantSHA)
		}
	}
}

func TestMediaServingRequiresAnActiveStoreCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "legacy/original.m2ts"
	})
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'tokyo', rel_path = 'copies/tokyo/original.m2ts' WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("setting the physical copy path: %v", err)
	}

	media := NewMedia(pool, "tokyo", "")
	got, err := media.GetOriginalMediaAssetForServing(ctx, recordingID)
	if err != nil {
		t.Fatalf("serving active copy: %v", err)
	}
	if got.Store != "tokyo" || got.RelPath != "copies/tokyo/original.m2ts" {
		t.Fatalf("served asset = %+v, want the active tokyo copy", got)
	}

	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'deleted', deleted_at = now() WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID); err != nil {
		t.Fatalf("deleting the physical copy: %v", err)
	}
	if _, err := media.GetOriginalMediaAssetForServing(ctx, recordingID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("serving without an active copy error = %v, want ErrNotFound", err)
	}
	if _, err := media.GetActiveOriginalMediaAsset(ctx, recordingID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reading an available original without an active copy error = %v, want ErrNotFound", err)
	}
}

func TestTrashDeleteCandidatesUseActiveCopyPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath = "legacy/original.m2ts"
	})
	if _, err := pool.Exec(ctx, `UPDATE recordings SET deleted_at = now() - interval '2 days' WHERE id = $1`, recordingID); err != nil {
		t.Fatalf("putting recording in trash: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET store = 'tokyo', rel_path = 'copies/tokyo/original.m2ts', state = 'deleted', deleted_at = now() WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatalf("making physical copy deleted: %v", err)
	}

	media := NewMedia(pool, "tokyo", "")
	rows, err := media.ListTrashAssetsToDelete(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("listing trash deletion candidates with a deleted copy: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("trash deletion candidates with a deleted copy = %+v, want none", rows)
	}

	if _, err := pool.Exec(ctx, `UPDATE media_asset_copies SET state = 'active', deleted_at = NULL WHERE media_asset_id = $1 AND store = 'tokyo'`, assetID); err != nil {
		t.Fatalf("activating physical copy: %v", err)
	}
	rows, err = media.ListTrashAssetsToDelete(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("listing trash deletion candidates with an active copy: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != assetID || rows[0].RelPath != "copies/tokyo/original.m2ts" {
		t.Fatalf("trash deletion candidates = %+v, want asset %d at its active copy path", rows, assetID)
	}
}

type catalogAssetCopy struct {
	recordingID int64
	store       string
	relPath     string
	sha256      string
}

func mediaAssetCopyRows(assets []catalogdoc.MediaAsset) []catalogAssetCopy {
	rows := make([]catalogAssetCopy, 0, len(assets))
	for _, asset := range assets {
		sha := ""
		if asset.SHA256 != nil {
			sha = *asset.SHA256
		}
		rows = append(rows, catalogAssetCopy{recordingID: asset.RecordingID, store: asset.Store, relPath: asset.RelPath, sha256: sha})
	}
	return rows
}

func assertCopyState(t *testing.T, pool *pgxpool.Pool, assetID int64, storeName, want string) {
	t.Helper()
	var copyState string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = $2`, assetID, storeName).Scan(&copyState); err != nil {
		t.Fatalf("reading copy state: %v", err)
	}
	if copyState != want {
		t.Fatalf("copy state = %q, want %q", copyState, want)
	}
}

// profile を持つ kind（encoded は profile ごとに複数行）は単一の世代を持たない。
// :one が任意の行を返さず「無い」を返すことを固定する。
func TestGetDerivedMediaAssetGenerationIgnoresKindsWithProfiles(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	for profile, generation := range map[string]int64{"h264": 3, "h265": 7} {
		if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES ($1, 'encoded', $2, 1, $3)`, recordingID, profile, generation); err != nil {
			t.Fatalf("seeding encoded %s: %v", profile, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, size_bytes, derived_generation)
VALUES ($1, 'thumbnail', 1, 4)`, recordingID); err != nil {
		t.Fatalf("seeding thumbnail: %v", err)
	}
	media := NewMedia(pool, "tokyo", "")
	if got, found, err := media.GetDerivedMediaAssetGeneration(ctx, recordingID, "encoded"); err != nil || found {
		t.Fatalf("encoded generation = %d, found %v, err %v; want not found", got, found, err)
	}
	if got, found, err := media.GetDerivedMediaAssetGeneration(ctx, recordingID, "thumbnail"); err != nil || !found || got != 4 {
		t.Fatalf("thumbnail generation = %d, found %v, err %v; want 4", got, found, err)
	}
}

func TestDerivedGenerationReadsLogicalRowsWithoutWriterCopiesAndNeverRegresses(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation)
VALUES ($1, 'encoded', $2, 9, 9)`, recordingID, profile); err != nil {
		t.Fatalf("seeding copyless derived assets: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, size_bytes, derived_generation)
VALUES ($1, 'thumbnail', 9, 9)`, recordingID); err != nil {
		t.Fatalf("seeding copyless thumbnail: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO media_assets (recording_id, kind, size_bytes, derived_generation)
VALUES ($1, 'seek_tiles', 9, 9)`, recordingID); err != nil {
		t.Fatalf("seeding copyless seek tiles: %v", err)
	}
	q := sqlcgen.New(pool)
	encoded, err := q.GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: recordingID, Profile: &profile,
	})
	if err != nil {
		t.Fatalf("reading encoded generation without a writer-store copy: %v", err)
	}
	if encoded.DerivedGeneration != 9 {
		t.Fatalf("encoded logical generation = %d, want 9", encoded.DerivedGeneration)
	}
	lowerGeneration := int64(4)
	for _, p := range []sqlcgen.UpsertDerivedMediaAssetParams{
		{Kind: "encoded", Profile: &profile},
		{Kind: "thumbnail"},
		{Kind: "seek_tiles"},
	} {
		p.RecordingID, p.SizeBytes, p.DerivedGeneration = recordingID, 4, &lowerGeneration
		if _, err := q.UpsertDerivedMediaAsset(ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s upsert at generation 4 below the logical 9: error = %v, want pgx.ErrNoRows (0 rows)", p.Kind, err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT kind, COALESCE(derived_generation, 0) FROM media_assets WHERE recording_id = $1 ORDER BY kind`, recordingID)
	if err != nil {
		t.Fatalf("reading logical derived generations after upsert: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var generation int64
		if err := rows.Scan(&kind, &generation); err != nil {
			t.Fatalf("scanning derived generation: %v", err)
		}
		if generation != 9 {
			t.Errorf("%s logical generation = %d after a lower upsert, want 9", kind, generation)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading derived generations: %v", err)
	}
}

// 同じ世代の再試行は通り、低い世代は 0 行になり、高い世代は引き上げる。
func TestUpsertDerivedMediaAssetGenerationGuard(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	recordingID := dbtest.Recording(t, pool)
	upsert := func(gen int64) (int64, error) {
		return q.UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{
			Kind: "thumbnail", RecordingID: recordingID, SizeBytes: 1, DerivedGeneration: &gen,
		})
	}
	id, err := upsert(5)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if again, err := upsert(5); err != nil || again != id {
		t.Fatalf("same-generation retry = %d, %v; want %d, nil", again, err, id)
	}
	if _, err := upsert(4); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lower-generation upsert error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := upsert(6); err != nil {
		t.Fatalf("higher-generation upsert: %v", err)
	}
	var derived int64
	if err := pool.QueryRow(ctx, `SELECT derived_generation FROM media_assets WHERE id = $1`, id).Scan(&derived); err != nil {
		t.Fatal(err)
	}
	if derived != 6 {
		t.Fatalf("generation = %d, want 6", derived)
	}
}

// 公開の計画が見る active は現行世代のコピーだけで決める。旧世代のコピーだけが active の版を active と読むと、
// 現行ファイルが無いのに公開を「満たし済み」として飛ばす。
func TestEncodedPublishStateActiveRequiresCurrentGenerationCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind = store.AssetKindEncoded
		p.Profile = &profile
		p.RelPath = "encoded/h264.mp4"
	})
	if _, err := pool.Exec(ctx, `INSERT INTO recording_encode_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES ($1, 'always', ARRAY['h264'], false)`, recordingID); err != nil {
		t.Fatalf("seeding encode policy: %v", err)
	}
	media := NewMedia(pool, dbtest.StoreName, "")
	observe := func() *store.EncodedAssetObservation {
		t.Helper()
		state, err := media.GetEncodedPublishState(ctx, recordingID, profile, false, false)
		if err != nil || state.EncodedAsset == nil {
			t.Fatalf("GetEncodedPublishState = %+v, %v", state, err)
		}
		return state.EncodedAsset
	}
	if got := observe(); !got.Active || got.DerivedGeneration != 0 {
		t.Fatalf("observation = %+v, want active at generation 0", got)
	}

	// 論理世代だけを進める。active なコピーは旧世代のままである。
	if _, err := pool.Exec(ctx, `UPDATE media_assets SET derived_generation = 1 WHERE id = $1`, assetID); err != nil {
		t.Fatal(err)
	}
	if got := observe(); got.Active || got.DerivedGeneration != 1 {
		t.Fatalf("observation = %+v, want inactive at generation 1 (only an old-generation copy is active)", got)
	}

	if _, err := sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID, Store: dbtest.StoreName, RelPath: "encoded/h264.g1.mp4", Generation: 1, State: "active",
	}); err != nil {
		t.Fatal(err)
	}
	if got := observe(); !got.Active || got.DerivedGeneration != 1 {
		t.Fatalf("observation = %+v, want active at generation 1", got)
	}
}
