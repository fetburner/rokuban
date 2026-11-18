package cmdetect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/testutil"
)

func TestNewDomainScratchDirRejectsEmptyRoot(t *testing.T) {
	t.Parallel()
	if dir, err := newDomainScratchDir("", "cm_detect", "42-1"); err == nil || err.Error() != "scratch dir is empty" {
		t.Fatalf("scratch directory = %q, error = %v, want empty-root error", dir, err)
	}
}

func TestNewDomainScratchDirClearsPreviousResidue(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir, err := newDomainScratchDir(root, "cm_detect", "42-1")
	testutil.RequireNoError(t, err, "newDomainScratchDir")
	marker := filepath.Join(dir, "stale-output")
	testutil.RequireNoError(t, os.WriteFile(marker, []byte("stale"), 0o600), "os.WriteFile")

	otherJobDir, err := newDomainScratchDir(root, "cm_detect", "43-1")
	testutil.RequireNoError(t, err, "newDomainScratchDir")
	if otherJobDir == dir {
		t.Fatalf("different jobs share scratch directory %q", dir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("creating another job's scratch directory changed the first job's residue: %v", err)
	}

	restartedDir, err := newDomainScratchDir(root, "cm_detect", "42-1")
	testutil.RequireNoError(t, err, "newDomainScratchDir")
	if restartedDir != dir {
		t.Fatalf("restarted scratch = %q, want stable path %q", restartedDir, dir)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("stale output stat error = %v, want os.ErrNotExist", err)
	}
}
