package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/testutil"
)

func storagePolicyYAML(name string) string {
	return fmt.Sprintf("  stores:\n    %s:\n      kind: fs\n  originals:\n    - store: %s\n  derived: %s\n  transfer:\n    overlap: 48h\n", name, name, name)
}

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
`+storagePolicyYAML("tokyo")+`mirakcs:
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
` + storagePolicyYAML("tokyo") + `mirakcs:
  - site: tokyo
    url: http://mirakc
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--config", configPath, "storage", "adopt", "osaka"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "does not match the fs store in storage.stores") {
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
` + storagePolicyYAML("tokyo") + `mirakcs:
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

// 目印の無い root に目印を新しく作るのは、DB にその置き場のコピー行が 1 件も無いときだけ。
// 初回導入（行 0）と移行初回（論理資産はあるがコピーが無い）は通り、コピー行があって目印が
// 無いとき（マウント漏れか別のボリューム）だけ、目印を書かずに拒む。
func TestStorageAdoptRefusesNewMarkerWhenStoreHasCopies(t *testing.T) {
	pool := testutil.SetupDB(t)
	dbCfg := testutil.DatabaseConfig(t)
	user := dbCfg.User
	if user == "" {
		user = os.Getenv("USER")
	}
	password := dbCfg.Password
	if password == "" {
		password = "unused-under-trust-auth"
	}
	adopt := func(t *testing.T, mediaDir string) error {
		t.Helper()
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
`+storagePolicyYAML("test-store")+`mirakcs:
  - site: tokyo
    url: http://mirakc
`, dbCfg.Host, dbCfg.Port, user, password, dbCfg.Database, dbCfg.SSLMode)
		if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := newRootCmd()
		cmd.SetArgs([]string{"--config", configPath, "storage", "adopt", "test-store"})
		cmd.SetOut(&strings.Builder{})
		return cmd.Execute()
	}
	markerExists := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, mediapath.StoreMarkerFileName))
		return err == nil
	}

	t.Run("first install with no rows", func(t *testing.T) {
		dir := t.TempDir()
		if err := adopt(t, dir); err != nil || !markerExists(dir) {
			t.Fatalf("adopt = %v, marker exists %v; want success with a marker", err, markerExists(dir))
		}
	})

	recordingID := dbtest.Recording(t, pool)
	assetID := dbtest.MediaAsset(t, pool, recordingID)
	if _, err := pool.Exec(t.Context(), `DELETE FROM media_asset_copies WHERE media_asset_id = $1`, assetID); err != nil {
		t.Fatal(err)
	}
	t.Run("first migration: assets without copies", func(t *testing.T) {
		dir := t.TempDir()
		if err := adopt(t, dir); err != nil || !markerExists(dir) {
			t.Fatalf("adopt = %v, marker exists %v; want success with a marker", err, markerExists(dir))
		}
	})

	// 直前のケースの adopt がコピー行を作った。別の（空の）root では拒否される。
	t.Run("copies exist but the root has no marker", func(t *testing.T) {
		dir := t.TempDir()
		err := adopt(t, dir)
		if err == nil || !strings.Contains(err.Error(), "already has media asset copies") {
			t.Fatalf("adopt on an unmarked root = %v, want refusal", err)
		}
		if markerExists(dir) {
			t.Fatal("marker was written despite the refusal")
		}
	})

	t.Run("same-name marker already present", func(t *testing.T) {
		dir := t.TempDir()
		if err := mediapath.WriteStoreMarker(dir, "test-store"); err != nil {
			t.Fatal(err)
		}
		if err := adopt(t, dir); err != nil {
			t.Fatalf("adopt with an existing same-name marker = %v, want success", err)
		}
	})
}
