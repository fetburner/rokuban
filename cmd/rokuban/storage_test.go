package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestStorageAdoptWritesStoreMarker(t *testing.T) {
	testutil.SetupDB(t)
	dbCfg := testutil.DatabaseConfig(t)
	user := dbCfg.User
	if user == "" {
		user = os.Getenv("USER")
	}
	password := dbCfg.Password
	if password == "" {
		password = "unused-under-trust-auth"
	}
	mediaDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yml")
	config := fmt.Sprintf(`db:
  host: %q
  port: %d
  user: %q
  password: %q
  database: %q
  sslmode: %q
storage:
  media_dir: `+mediaDir+`
  media_store_name: tokyo
mirakcs:
  - site: tokyo
    url: http://mirakc
`, dbCfg.Host, dbCfg.Port, user, password, dbCfg.Database, dbCfg.SSLMode)
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

func TestStorageAdoptDoesNotRenameExistingStoreMarker(t *testing.T) {
	mediaDir := t.TempDir()
	if err := mediapath.WriteStoreMarker(mediaDir, "osaka"); err != nil {
		t.Fatal(err)
	}
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
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "media store marker mismatch") {
		t.Fatalf("storage adopt existing different marker error = %v, want marker mismatch", err)
	}
	if got, err := os.ReadFile(filepath.Join(mediaDir, mediapath.StoreMarkerFileName)); err != nil {
		t.Fatalf("reading existing marker: %v", err)
	} else if string(got) != "osaka" {
		t.Fatalf("existing marker after rejected adoption = %q, want %q", got, "osaka")
	}
}
