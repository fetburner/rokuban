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
