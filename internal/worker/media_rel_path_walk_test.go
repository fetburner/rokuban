package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/medialock"
)

const mediaRelPathLockGateFile = medialock.GateFile

var mediaRelPathLockPath = medialock.Path

func mediaRelPathLockGatePath(mediaDir string) string {
	return filepath.Join(mediaDir, ".rokuban-locks", medialock.GateFile)
}
func TestWalkMediaFiles_IgnoresRelPathLockDirectoryAndLegacyFiles(t *testing.T) {
	mediaDir := t.TempDir()
	canonicalPath := filepath.Join(mediaDir, "recording.m2ts")
	const relPath = "recording.m2ts"
	if err := os.WriteFile(canonicalPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("writing canonical file: %v", err)
	}
	lockPath := mediaRelPathLockPath(mediaDir, relPath)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("creating lock directory: %v", err)
	}
	if err := os.WriteFile(mediaRelPathLockGatePath(mediaDir), nil, 0o666); err != nil {
		t.Fatalf("writing GC gate: %v", err)
	}
	if err := os.WriteFile(lockPath, nil, 0o666); err != nil {
		t.Fatalf("writing rel_path lock file: %v", err)
	}
	legacyPath := filepath.Join(mediaDir, ".rokuban-rel-path-lock-legacy.lock")
	if err := os.WriteFile(legacyPath, nil, 0o666); err != nil {
		t.Fatalf("writing legacy rel_path lock file: %v", err)
	}

	var got []string
	if err := walkMediaFiles(mediaDir, func(path string, _ os.FileInfo) {
		got = append(got, path)
	}); err != nil {
		t.Fatalf("walking media files: %v", err)
	}
	if len(got) != 1 || got[0] != relPath {
		t.Fatalf("walked files = %v, want only %q", got, relPath)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
