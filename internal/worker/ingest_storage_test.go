package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/mediapath"
)

func TestProbeIngestStorage_CompletesOperationSequenceAndCleansUp(t *testing.T) {
	mediaDir := t.TempDir()

	if err := ProbeIngestStorage(mediaDir); err != nil {
		t.Fatalf("ProbeIngestStorage: %v", err)
	}

	entries, err := os.ReadDir(mediaDir)
	if err != nil {
		t.Fatalf("reading probe directory: %v", err)
	}
	for _, entry := range entries {
		if mediapath.IsIngestTempFile(entry.Name()) {
			t.Errorf("probe left temporary file %q", entry.Name())
		}
	}
}

func TestProbeIngestStorage_RejectsMissingOrNonDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := ProbeIngestStorage(missing); err == nil {
		t.Fatal("ProbeIngestStorage(missing) returned nil")
	} else if !strings.Contains(err.Error(), "storage.media_dir") {
		t.Errorf("error = %q, want storage.media_dir context", err)
	}

	filePath := filepath.Join(t.TempDir(), "media-file")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("writing non-directory probe target: %v", err)
	}
	if err := ProbeIngestStorage(filePath); err == nil {
		t.Fatal("ProbeIngestStorage(file) returned nil")
	} else if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error = %q, want not-a-directory context", err)
	}
}
