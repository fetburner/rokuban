package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/mediapath"
)

func TestStorageAdoptWritesStoreMarker(t *testing.T) {
	mediaDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yml")
	config := `db:
  host: localhost
  user: postgres
  password: postgres
  database: postgres
storage:
  media_dir: ` + mediaDir + `
  media_store_name: tokyo
mirakcs:
  - site: tokyo
    url: http://mirakc
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--config", configPath, "storage", "adopt", "tokyo"})
	var output strings.Builder
	cmd.SetOut(&output)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("storage adopt failed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(mediaDir, mediapath.StoreMarkerFileName)); err != nil {
		t.Fatalf("reading store marker: %v", err)
	} else if string(got) != "tokyo" {
		t.Fatalf("store marker = %q, want %q", got, "tokyo")
	}
	if !strings.Contains(output.String(), mediapath.StoreMarkerFileName) {
		t.Errorf("output %q does not mention the written marker", output.String())
	}
}

func TestStorageAdoptRejectsNameDifferentFromConfig(t *testing.T) {
	mediaDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yml")
	config := `db:
  host: localhost
  user: postgres
  password: postgres
  database: postgres
storage:
  media_dir: ` + mediaDir + `
  media_store_name: tokyo
mirakcs:
  - site: tokyo
    url: http://mirakc
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--config", configPath, "storage", "adopt", "osaka"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "does not match storage.media_store_name") {
		t.Fatalf("storage adopt wrong name error = %v, want config mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(mediaDir, mediapath.StoreMarkerFileName)); !os.IsNotExist(err) {
		t.Fatalf("marker exists after rejected adoption: stat err = %v", err)
	}
}
