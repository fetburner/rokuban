package inplace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestRegister_IdempotentExistingFile(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mediaDir, "imported"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mediaDir, "imported", "show.mp4")
	if err := os.WriteFile(path, []byte("first bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	profile := "rescue-mp4"
	at := time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC)
	input := Input{
		Recording: Recording{
			Source: "manual", Site: "default",
			NetworkID: -1, ServiceID: -2, EventID: -3,
			ServiceName: "Recovered file", ChannelType: "GR", Channel: "unknown",
			Title: "show", ProgramStartAt: at, Status: "finished",
		},
		Assets: []Asset{{Kind: db.AssetKindEncoded, Profile: &profile, RelPath: "imported/show.mp4"}},
	}

	first, err := Register(context.Background(), pool, mediaDir, input)
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	second, err := Register(context.Background(), pool, mediaDir, input)
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if second.RecordingID != first.RecordingID || second.AssetIDs[0] != first.AssetIDs[0] {
		t.Fatalf("ids changed on repeat: first=%+v second=%+v", first, second)
	}

	// ごみ箱の録画を rescue が暗黙に復元・複製してはならない。rel_path から同じ
	// recording を再利用し、deleted_at はそのまま保つ。
	if _, err := pool.Exec(context.Background(), `UPDATE recordings SET deleted_at = now() WHERE id = $1`, first.RecordingID); err != nil {
		t.Fatal(err)
	}
	third, err := Register(context.Background(), pool, mediaDir, input)
	if err != nil {
		t.Fatalf("Register after soft delete: %v", err)
	}
	if third.RecordingID != first.RecordingID || third.AssetIDs[0] != first.AssetIDs[0] {
		t.Fatalf("soft-deleted asset was duplicated: first=%+v third=%+v", first, third)
	}
	var stillDeleted bool
	if err := pool.QueryRow(context.Background(), `SELECT deleted_at IS NOT NULL FROM recordings WHERE id = $1`, first.RecordingID).
		Scan(&stillDeleted); err != nil {
		t.Fatal(err)
	}
	if !stillDeleted {
		t.Fatal("Register must not restore a soft-deleted recording")
	}

	var recordings, assets int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recordings`).Scan(&recordings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_assets`).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if recordings != 1 || assets != 1 {
		t.Fatalf("rows after repeat: recordings=%d assets=%d, want 1/1", recordings, assets)
	}

	var relPath, kind, gotProfile string
	var size int64
	if err := pool.QueryRow(context.Background(), `
		SELECT rel_path, kind, profile, size_bytes FROM media_assets WHERE id = $1
	`, first.AssetIDs[0]).Scan(&relPath, &kind, &gotProfile, &size); err != nil {
		t.Fatal(err)
	}
	if relPath != "imported/show.mp4" || kind != db.AssetKindEncoded || gotProfile != profile || size != 11 {
		t.Errorf("asset = path=%q kind=%q profile=%q size=%d", relPath, kind, gotProfile, size)
	}
}

func TestRegister_PrefersLiveAssetOverDeletedForSameRelPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mediaDir, "reused"), 0o755); err != nil {
		t.Fatal(err)
	}
	relPath := "reused/show.ts"
	if err := os.WriteFile(filepath.Join(mediaDir, relPath), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	at := time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC)
	const insertRecording = `
		INSERT INTO recordings (
			source, site, network_id, service_id, event_id,
			service_name, channel_type, channel, title, program_start_at, program_duration_ms, status
		) VALUES ('manual', 'default', $1, $2, $3, $4, 'GR', 'unknown', $4, $5, 0, 'finished')
		RETURNING id`

	// rel_path が再利用された後を模して、同じ rel_path に live 行 (新しい recording) と
	// deleted 行 (古い recording) を直接仕込む。deleted 行を live 行より後に挿入して
	// 大きい id を持たせる: これで id DESC の一発検索では deleted 行が先に来てしまう
	// ため、live 行を優先する state の並び替えが効いているかを検証できる。
	var liveRecordingID int64
	if err := pool.QueryRow(ctx, insertRecording, -20, -21, -22, "new", at).Scan(&liveRecordingID); err != nil {
		t.Fatal(err)
	}
	profile := "rescue-mp4"
	var liveAssetID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state)
		VALUES ($1, 'encoded', $2, $3, 5, 'active')
		RETURNING id
	`, liveRecordingID, profile, relPath).Scan(&liveAssetID); err != nil {
		t.Fatal(err)
	}

	var deletedRecordingID int64
	if err := pool.QueryRow(ctx, insertRecording, -10, -11, -12, "old", at).Scan(&deletedRecordingID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media_assets (recording_id, kind, profile, rel_path, size_bytes, state, deleted_at)
		VALUES ($1, 'original', NULL, $2, 5, 'deleted', now())
	`, deletedRecordingID, relPath); err != nil {
		t.Fatal(err)
	}

	got, err := Register(ctx, pool, mediaDir, Input{
		Recording: Recording{
			Source: "manual", Site: "default",
			NetworkID: -20, ServiceID: -21, EventID: -22,
			ServiceName: "new", ChannelType: "GR", Channel: "unknown",
			Title: "new", ProgramStartAt: at, Status: "finished",
		},
		Assets: []Asset{{Kind: db.AssetKindEncoded, Profile: &profile, RelPath: relPath}},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got.RecordingID != liveRecordingID || got.AssetIDs[0] != liveAssetID {
		t.Fatalf("Register must pick the live row over the deleted one: got recording=%d asset=%d, want recording=%d asset=%d",
			got.RecordingID, got.AssetIDs[0], liveRecordingID, liveAssetID)
	}

	var assetCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_assets WHERE rel_path = $1`, relPath).Scan(&assetCount); err != nil {
		t.Fatal(err)
	}
	if assetCount != 2 {
		t.Fatalf("Register must not create a new row when a live asset already exists at rel_path, got %d rows", assetCount)
	}
}

func TestRegister_RejectsSymlink(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.ts")
	if err := os.WriteFile(outside, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mediaDir, "linked.ts")); err != nil {
		t.Fatal(err)
	}

	_, err := Register(context.Background(), pool, mediaDir, Input{
		Recording: Recording{Site: "default"},
		Assets:    []Asset{{Kind: db.AssetKindOriginal, RelPath: "linked.ts"}},
	})
	if err == nil {
		t.Fatal("symlink should be rejected")
	}
}

func TestRegister_RejectsIntermediateSymlinkEscape(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "outside.ts"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(mediaDir, "linked-dir")); err != nil {
		t.Fatal(err)
	}

	_, err := Register(context.Background(), pool, mediaDir, Input{
		Recording: Recording{Site: "default"},
		Assets:    []Asset{{Kind: db.AssetKindOriginal, RelPath: "linked-dir/outside.ts"}},
	})
	if err == nil {
		t.Fatal("intermediate symlink escaping media_dir should be rejected")
	}
}

func TestRegister_RejectsPathOutsideMediaDir(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.ts")
	if err := os.WriteFile(outside, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Register(context.Background(), pool, mediaDir, Input{
		Recording: Recording{Site: "default"},
		Assets:    []Asset{{Kind: db.AssetKindOriginal, RelPath: outside}},
	})
	if err == nil {
		t.Fatal("absolute path should be rejected")
	}
	var count int
	if queryErr := pool.QueryRow(context.Background(), `SELECT count(*) FROM recordings`).Scan(&count); queryErr != nil {
		t.Fatal(queryErr)
	}
	if count != 0 {
		t.Fatalf("rejected asset created %d recording rows", count)
	}
}
