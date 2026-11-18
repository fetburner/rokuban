package storagemaint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestWalkMediaFiles_IgnoresReservedLockArtifacts(t *testing.T) {
	t.Parallel()
	mediaDir := t.TempDir()
	canonicalPath := filepath.Join(mediaDir, "recording.m2ts")
	const relPath = "recording.m2ts"
	testutil.RequireNoError(t, os.WriteFile(canonicalPath, []byte("media"), 0o644), "writing canonical file")
	lockDir := filepath.Join(mediaDir, mediapath.MediaRelPathLockDirName)
	testutil.RequireNoError(t, os.MkdirAll(lockDir, 0o755), "creating lock directory")
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(lockDir, "legacy-entry"), nil, 0o666), "writing reserved lock directory entry")
	legacyPath := filepath.Join(mediaDir, ".rokuban-rel-path-lock-legacy.lock")
	testutil.RequireNoError(t, os.WriteFile(legacyPath, nil, 0o666), "writing legacy rel_path lock file")

	var got []string
	testutil.RequireNoError(t, walkMediaFiles(mediaDir, func(path string, _ os.FileInfo) {
		got = append(got, path)
	}), "walking media files")
	if len(got) != 1 || got[0] != relPath {
		t.Fatalf("walked files = %v, want only %q", got, relPath)
	}
}

func TestWalkMediaFilesIgnoresOnlyRootStoreMarker(t *testing.T) {
	t.Parallel()
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(mediaDir, ".rokuban-store"), []byte("tokyo"), 0o644), "writing root store marker")
	nestedMarkerPath := filepath.Join(mediaDir, "nested", ".rokuban-store")
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(nestedMarkerPath), 0o755), "creating nested directory")
	testutil.RequireNoError(t, os.WriteFile(nestedMarkerPath, []byte("media"), 0o644), "writing nested media file")

	var got []string
	testutil.RequireNoError(t, walkMediaFiles(mediaDir, func(relPath string, _ os.FileInfo) {
		got = append(got, relPath)
	}), "walking media files")
	if len(got) != 1 || got[0] != "nested/.rokuban-store" {
		t.Fatalf("walked files = %v, want only nested/.rokuban-store", got)
	}
}
