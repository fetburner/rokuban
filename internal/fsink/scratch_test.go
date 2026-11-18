package fsink

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewScratchDirIsUniqueForSameRecording(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, err := NewScratchDir(root, "thumbnail", "42-")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewScratchDir(root, "thumbnail", "42-")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("scratch directory reused: %q", first)
	}
	for _, path := range []string{first, second} {
		if filepath.Dir(path) != filepath.Join(root, "thumbnail") || !strings.HasPrefix(filepath.Base(path), "42-") {
			t.Errorf("scratch path = %q, want thumbnail directory and 42- prefix", path)
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Errorf("scratch path %q is not a directory: info=%v err=%v", path, info, err)
		}
	}
}

func TestNewScratchDirRejectsEmptyRoot(t *testing.T) {
	t.Parallel()
	if dir, err := NewScratchDir("", "thumbnail", "42-"); err == nil || err.Error() != "scratch dir is empty" {
		t.Fatalf("scratch directory = %q, error = %v, want empty-root error", dir, err)
	}
}
