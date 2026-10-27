package catalog

import (
	"testing"

	"github.com/fetburner/rokuban/internal/mediapath"
)

func newTestMediaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := mediapath.WriteStoreMarker(dir, "test-store"); err != nil {
		t.Fatalf("writing test media store marker: %v", err)
	}
	return dir
}
