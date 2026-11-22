package thumbnail

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// seedOriginalAsset は原本の active media_assets 行 + ファイルを 1 件用意する。
func seedOriginalAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, relPath string, content []byte) int64 {
	t.Helper()
	return seedMediaAsset(t, pool, mediaDir, recordingID, store.AssetKindOriginal, nil, relPath, content)
}

func seedMediaAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, kind string, profile *string, relPath string, content []byte) int64 {
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir")
	testutil.RequireNoError(t, os.WriteFile(full, content, 0o644), "write")
	return dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.Kind = kind
		fixtureParams.Profile = profile
		fixtureParams.RelPath = relPath
		fixtureParams.SizeBytes = int64(len(content))
	})
}

func recordedArgs[T jobq.Args](t *testing.T, calls []jobqfaketest.EnqueueCall) []T {
	t.Helper()
	args := make([]T, 0, len(calls))
	for _, call := range calls {
		value, ok := call.Args.(T)
		if !ok {
			t.Fatalf("enqueued args type = %T, want %T", call.Args, *new(T))
		}
		args = append(args, value)
	}
	return args
}
func testFSObjectStore(root string) objstore.Store {
	objects, err := fsstore.New(root)
	if err != nil {
		panic(err)
	}
	return objects
}

// seedSeekTilesAsset は seek_tiles の active media_assets 行 + ファイルを用意する。
// 原本の until_encoded 削除は thumbnail と同じく seek_tiles の完備も要求するので、
// 「派生物が揃っている」ことを作るテストはこれを必ず呼ぶ。
func seedSeekTilesAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, relPath string) {
	t.Helper()
	seedMediaAsset(t, pool, mediaDir, recordingID, store.AssetKindSeekTiles, nil, relPath, []byte("tiles"))
}

// seedCMRanges は録画 recordingID の CM 区間を、CM 検出の契約（試行の開始と結果の保存）で保存する。
// 契約は CM 検出が desired の録画だけ結果を保存するので、方針を有効にし、原本が実体を持つ状態で呼ぶ。
func seedCMRanges(t *testing.T, pool *pgxpool.Pool, recordingID int64, ranges []chapters.Range) {
	t.Helper()
	ctx := t.Context()
	enabled := true
	testutil.RequireNoError(t, pgstoretest.Media(pool).SetPolicy(ctx, recordingID, store.EncodePolicyUpdate{CMDetect: &enabled}), "enabling CM detection")
	det := pgstoretest.Analysis(pool)
	attempt, shouldRun, err := det.BeginAttempt(ctx, recordingID, 1)
	testutil.RequireNoError(t, err, "beginning CM detection attempt")
	if !shouldRun {
		t.Fatalf("CM detection attempt for recording %d should run", recordingID)
	}
	saved, err := det.SaveDetection(ctx, attempt, ranges)
	testutil.RequireNoError(t, err, "saving CM detection result")
	if !saved {
		t.Fatalf("CM detection result for recording %d was not saved", recordingID)
	}
}

// seedLostOriginalCMRanges は、原本の行はあるが実体無しの印が付いた録画に CM 区間を保存する（原本が active で実体無し候補の状態）。
// CM 検出の契約は検出の時点で原本が実体を持つことを要求するので、原本を作って検出し、そのあとで実体無しの印を付ける。
// 「active な原本が無い」状態は seedDeletedOriginalCMRanges を使う。
func seedLostOriginalCMRanges(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, ranges []chapters.Range) {
	t.Helper()
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("lost/%d.ts", recordingID), []byte("fake-ts"))
	seedCMRanges(t, pool, recordingID, ranges)
	testutil.RequireNoError(t, pgstoretest.Media(pool).UpsertMissingMediaAsset(t.Context(), dbtest.MediaAssetCopyID(t, pool, originalID, dbtest.StoreName)), "marking original missing")
}

// requireThumbnailState は録画 recordingID の active なサムネイルを dbtest.StoreName から返す。無ければテストを失敗させる。
func requireThumbnailState(t *testing.T, pool *pgxpool.Pool, recordingID int64) dbtest.ThumbnailRow {
	t.Helper()
	row, found := dbtest.ThumbnailState(t, pool, recordingID, dbtest.StoreName)
	if !found {
		t.Fatalf("recording %d has no active thumbnail in store %q", recordingID, dbtest.StoreName)
	}
	return row
}

// seedDeletedOriginalCMRanges は、CM 区間を保存したあとで原本が削除された録画（カットだけを残して原本が消えた状態）を作る。
// CM 検出の契約は検出の時点で原本が実体を持つことを要求するので、原本を作って検出し、そのあとで原本の複製を deleted にする。
// これで GetActiveOriginalMediaAsset は見つからない（active な原本が無い分岐）。
func seedDeletedOriginalCMRanges(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, ranges []chapters.Range) {
	t.Helper()
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, fmt.Sprintf("deleted/%d.ts", recordingID), []byte("fake-ts"))
	seedCMRanges(t, pool, recordingID, ranges)
	dbtest.SetMediaAssetState(t, pool, originalID, "deleted")
}

// setThumbnailSeek は、サムネイル asset の位置（seek_ms）を衛星表に書く。位置は公開と同じ commit で書かれるもので、テストは公開を経ずに直接作る。
func setThumbnailSeek(t *testing.T, pool *pgxpool.Pool, assetID int64, seekMs int64) {
	t.Helper()
	testutil.MustExec(t, pool, "setting thumbnail seek", `INSERT INTO media_asset_thumbnail_seeks (media_asset_id, seek_ms) VALUES ($1, $2)
ON CONFLICT (media_asset_id) DO UPDATE SET seek_ms = EXCLUDED.seek_ms`, assetID, seekMs)
}
