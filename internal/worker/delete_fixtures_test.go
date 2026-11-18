package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// seedOriginalAsset は原本の active media_assets 行 + ファイルを 1 件用意する。
func seedOriginalAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, relPath string, content []byte) int64 {
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir")
	testutil.RequireNoError(t, os.WriteFile(full, content, 0o644), "write")
	id := dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = relPath
		fixtureParams.SizeBytes = int64(len(content))
	})

	return id
}

// seedEncodedOrThumbnailAsset は encoded / thumbnail の active media_assets 行 + ファイルを用意する。
func seedEncodedOrThumbnailAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, kind string, profile *string, relPath string, content []byte) int64 {
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir")
	testutil.RequireNoError(t, os.WriteFile(full, content, 0o644), "write")
	id := dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.Kind = kind
		fixtureParams.Profile = profile
		fixtureParams.RelPath = relPath
		fixtureParams.SizeBytes = int64(len(content))
	})

	return id
}

// seedSeekTilesAsset は seek_tiles の active media_assets 行 + ファイルを用意する。
// 原本の until_encoded 削除は thumbnail と同じく seek_tiles の完備も要求するので、
// 「派生物が揃っている」ことを作るテストはこれを必ず呼ぶ。
func seedSeekTilesAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, relPath string) int64 {
	t.Helper()
	return seedEncodedOrThumbnailAsset(t, pool, mediaDir, recordingID, store.AssetKindSeekTiles, nil, relPath, []byte("tiles"))
}

// markRecordingUntilEncoded は recording_processing_policy 衛星表（issue #159）に
// keep_original='until_encoded' の行を作る/上書きするテスト用フィクスチャ。
// ingest の resolveAndSnapshotEncodePolicy が原本コミットと同一 tx で凍結する
// 内容を模す（reservations.sql は #52 並走中につき、この目的のためだけの
// 書き込みクエリを新設しない。CLAUDE.md 同種の規律）。このファイルの呼び出し元は
// すべて until_encoded 腕の削除判定を確認するテストなので keep_original を
// パラメータにしない（unparam）。本番の凍結は ON CONFLICT を持たない素の
// INSERT だが（1 録画につき 1 回しか呼ばれない前提）、ここはテストの都合上
// 何度でも呼べるように upsert にしてある。
func markRecordingUntilEncoded(t *testing.T, pool *pgxpool.Pool, recordingID int64, profiles []string) {
	t.Helper()
	if profiles == nil {
		profiles = []string{}
	}
	testutil.MustExec(t, pool, "marking original as scanned", `
		INSERT INTO media_asset_ts_scans (media_asset_id, scanned_size_bytes)
		SELECT id, size_bytes FROM media_assets
		WHERE recording_id = $1 AND kind = 'original'
		ON CONFLICT (media_asset_id) DO NOTHING`, recordingID)
	testutil.MustExec(t, pool, "setting recording_processing_policy", `INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles)
		 VALUES ($1, 'until_encoded', $2)
		 ON CONFLICT (recording_id) DO UPDATE SET
		   keep_original   = EXCLUDED.keep_original,
		   encode_profiles = EXCLUDED.encode_profiles,
		   updated_at      = now()`, recordingID, profiles)
}

func singleCopyState(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT state FROM media_asset_copies WHERE media_asset_id = $1", id)
	testutil.RequireNoError(t, err, "querying media asset copy state")
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	testutil.RequireNoError(t, err, "collecting media asset copy states")
	if len(states) != 1 {
		t.Fatalf("media asset %d has %d copies, want exactly one; inspect a specific copy or use hasCurrentCopy", id, len(states))
	}
	return states[0]
}
