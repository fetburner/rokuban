package epgimport

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestImportLibrary_IdempotentRerun is the acceptance criterion: re-running
// the same library import must not add rows (relies on internal/inplace's
// existing rel_path / event-identity idempotency).
func TestImportLibrary_IdempotentRerun(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mediaDir, "imported"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "imported", "show.ts"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	items := []LibraryItem{{
		ChannelID:   3273601024,
		ChannelType: "GR",
		StartAt:     1785000000000,
		EndAt:       1785001800000,
		Name:        "番組A",
		VideoFiles:  []LibraryVideoFile{{Type: "ts", RelPath: "imported/show.ts", Size: 5}},
	}}

	first, err := ImportLibrary(context.Background(), pool, mediaDir, "default", items)
	if err != nil {
		t.Fatalf("first ImportLibrary: %v", err)
	}
	if first.Registered != 1 {
		t.Fatalf("first.Registered = %d, want 1", first.Registered)
	}

	second, err := ImportLibrary(context.Background(), pool, mediaDir, "default", items)
	if err != nil {
		t.Fatalf("second ImportLibrary: %v", err)
	}
	if second.Registered != 1 {
		t.Fatalf("second.Registered = %d, want 1", second.Registered)
	}

	var recordings, assets int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recordings`).Scan(&recordings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_assets`).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if recordings != 1 || assets != 1 {
		t.Fatalf("recordings=%d assets=%d after rerun, want 1/1", recordings, assets)
	}
}

// TestImportLibrary_EncodedTypeSkippedWithWarning: EPGStation encoded video
// files have no rokuban encode-profile equivalent, so they must be skipped
// with a warning rather than registered under a fabricated profile name.
func TestImportLibrary_EncodedTypeSkippedWithWarning(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mediaDir, "show.mp4"), []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	items := []LibraryItem{{
		ChannelID:   3273601024,
		ChannelType: "GR",
		StartAt:     1785000000000,
		EndAt:       1785001800000,
		Name:        "番組B",
		VideoFiles:  []LibraryVideoFile{{Type: "encoded", RelPath: "show.mp4", Size: 5}},
	}}

	result, err := ImportLibrary(context.Background(), pool, mediaDir, "default", items)
	if err != nil {
		t.Fatalf("ImportLibrary: %v", err)
	}
	if result.Registered != 0 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want Registered=0 Skipped=1 (no importable asset)", result)
	}
	if len(result.Warnings) < 2 {
		t.Fatalf("warnings = %v, want at least 2 (encoded-skip + no-asset-skip)", result.Warnings)
	}
}
