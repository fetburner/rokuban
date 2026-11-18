package cmdetect

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// seedOriginalAsset は原本の active media_assets 行 + ファイルを 1 件用意する。
func seedOriginalAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, relPath string, content []byte) int64 {
	t.Helper()
	return seedMediaAsset(t, pool, mediaDir, recordingID, store.AssetKindOriginal, nil, relPath, content)
}

func seedUntilEncodedAssets(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64) {
	t.Helper()
	base := fmt.Sprintf("cm/%d", recordingID)
	originalID := seedOriginalAsset(t, pool, mediaDir, recordingID, base+".ts", []byte("ts"))
	markOriginalTSScanComplete(t, pool, originalID)
	profile := "h264"
	seedMediaAsset(t, pool, mediaDir, recordingID, store.AssetKindEncoded, &profile, base+"-h264.mp4", []byte("encoded"))
	seedMediaAsset(t, pool, mediaDir, recordingID, store.AssetKindThumbnail, nil, base+".jpg", []byte("thumbnail"))
	seedMediaAsset(t, pool, mediaDir, recordingID, store.AssetKindSeekTiles, nil, base+"-tiles", []byte("tiles"))
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

func markOriginalTSScanComplete(t *testing.T, pool *pgxpool.Pool, assetID int64) {
	t.Helper()
	testutil.MustExec(t, pool, "marking original as scanned", `
		INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
		SELECT id, size_bytes FROM media_assets WHERE id = $1 AND kind = 'original'
		ON CONFLICT (media_asset_id) DO NOTHING`, assetID)
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
