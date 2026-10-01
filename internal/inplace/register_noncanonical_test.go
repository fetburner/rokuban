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

// epgimport は外部 JSON の relPath をそのまま連結するので "./" や "//" を含みうる。
// Register は正規化した path で登録する（LockPaths が非正規 path を拒否しても落ちない）。
func TestRegister_AcceptsNonCanonicalRelPath(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mediaDir, "imported"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "imported", "show.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := "rescue-mp4"
	res, err := Register(context.Background(), pool, mediaDir, Input{
		Recording: Recording{
			Source: "manual", Site: "default", NetworkID: -1, ServiceID: -2, EventID: -3,
			ServiceName: "x", ChannelType: "GR", Channel: "unknown", Title: "show",
			ProgramStartAt: time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC), Status: "finished",
		},
		Assets: []Asset{{Kind: db.AssetKindEncoded, Profile: &profile, RelPath: "./imported//show.mp4"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var relPath string
	if err := pool.QueryRow(context.Background(), `SELECT rel_path FROM media_assets WHERE id = $1`, res.AssetIDs[0]).Scan(&relPath); err != nil {
		t.Fatal(err)
	}
	if relPath != "imported/show.mp4" {
		t.Fatalf("rel_path = %q", relPath)
	}
}
