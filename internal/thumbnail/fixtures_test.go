package thumbnail

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
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
