package worker

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/fetburner/rokuban/internal/mediapath"
)

// ProbeIngestStorage checks the operation sequence required by the ingest contract:
// create a file under media_dir, fsync the file, close it, atomically rename it within
// the same directory, and fsync the parent directory. It deliberately does not infer a
// filesystem type from the path. A successful probe is only evidence that this operation
// sequence worked once; the configuration contract still excludes filesystems whose
// rename/fsync semantics are not reliable.
func ProbeIngestStorage(mediaDir string) error {
	info, err := os.Stat(mediaDir)
	if err != nil {
		return fmt.Errorf("stating storage.media_dir %q: %w", mediaDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("storage.media_dir %q is not a directory", mediaDir)
	}

	tempPath, err := os.CreateTemp(mediaDir, mediapath.IngestTempFilePrefix+"probe-*")
	if err != nil {
		return fmt.Errorf("creating ingest storage probe file: %w", err)
	}
	tempName := tempPath.Name()
	finalPath := filepath.Join(mediaDir, mediapath.IngestTempFilePrefix+"probe-"+uuid.NewString())
	cleanup := func() {
		_ = os.Remove(tempName)
		_ = os.Remove(finalPath)
	}
	defer cleanup()

	if err := tempPath.Sync(); err != nil {
		_ = tempPath.Close()
		return fmt.Errorf("fsyncing ingest storage probe file: %w", err)
	}
	if err := tempPath.Close(); err != nil {
		return fmt.Errorf("closing ingest storage probe file: %w", err)
	}
	if err := os.Rename(tempName, finalPath); err != nil {
		return fmt.Errorf("renaming ingest storage probe file: %w", err)
	}
	if err := syncIngestDirectory(finalPath); err != nil {
		return fmt.Errorf("fsyncing ingest storage probe parent directory: %w", err)
	}
	return nil
}
