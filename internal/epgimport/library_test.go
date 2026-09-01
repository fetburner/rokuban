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
		VideoFiles:  []LibraryVideoFile{{Type: "ts", RelPath: "imported/show.ts"}},
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
		VideoFiles:  []LibraryVideoFile{{Type: "encoded", RelPath: "show.mp4"}},
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

// TestImportLibrary_MultipleThumbnails_ImportsFirstAndWarns is the
// acceptance criterion behind blocking finding 3: media_assets is
// UNIQUE NULLS NOT DISTINCT (recording_id, kind, profile), so a recording
// can only ever have one 'thumbnail' row. EPGStation's Thumbnail is 1:N on
// Recorded (one per video file), so this is not an exotic input — any
// library with encoded files trips it. The second thumbnail must not abort
// the whole item (and, with it, every later item in the batch).
func TestImportLibrary_MultipleThumbnails_ImportsFirstAndWarns(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mediaDir, "thumb1.jpg"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "thumb2.jpg"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	items := []LibraryItem{{
		ChannelID:   3273601024,
		ChannelType: "GR",
		StartAt:     1785000000000,
		EndAt:       1785001800000,
		Name:        "番組A",
		Thumbnails: []LibraryThumbnail{
			{RelPath: "thumb1.jpg"},
			{RelPath: "thumb2.jpg"},
		},
	}}

	result, err := ImportLibrary(context.Background(), pool, mediaDir, "default", items)
	if err != nil {
		t.Fatalf("ImportLibrary: %v", err)
	}
	if result.Registered != 1 {
		t.Fatalf("result = %+v, want Registered=1 (the item must still import with its first thumbnail)", result)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("want a warning about the dropped second thumbnail, got none")
	}

	var recordings, assets int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM recordings`).Scan(&recordings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM media_assets`).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if recordings != 1 || assets != 1 {
		t.Fatalf("recordings=%d assets=%d, want 1/1 (only the first thumbnail should be registered)", recordings, assets)
	}

	var relPath string
	if err := pool.QueryRow(context.Background(), `SELECT rel_path FROM media_assets`).Scan(&relPath); err != nil {
		t.Fatal(err)
	}
	if relPath != "thumb1.jpg" {
		t.Errorf("rel_path = %q, want thumb1.jpg (the first thumbnail)", relPath)
	}
}

// TestSyntheticEventID_DeterministicAndDistinct: re-importing the same
// (name, endAt) must produce the same synthetic event id (the idempotency
// key for rows with no real broadcast event id), and different inputs must
// not collide trivially. This is what lets a re-import survive after the
// operator has moved or renamed files — rel_path-based idempotency
// (TestImportLibrary_IdempotentRerun) doesn't exercise this path at all.
func TestSyntheticEventID_DeterministicAndDistinct(t *testing.T) {
	a1 := syntheticEventID("番組A", 1785001800000)
	a2 := syntheticEventID("番組A", 1785001800000)
	if a1 != a2 {
		t.Fatalf("syntheticEventID not deterministic: %d != %d", a1, a2)
	}
	if a1 >= 0 {
		t.Errorf("syntheticEventID = %d, want negative (must not collide with real mirakc event ids)", a1)
	}
	b := syntheticEventID("番組B", 1785001800000)
	if a1 == b {
		t.Errorf("different names produced the same synthetic event id: %d", a1)
	}
}
