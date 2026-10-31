package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/testutil"
)

func storagePolicyYAML(name string) string {
	return fmt.Sprintf("  stores:\n    %s:\n      kind: fs\n  originals:\n    - store: %s\n  derived: %s\n", name, name, name)
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

func TestStorageAdoptRejectsUnknownAndNonFSStoreName(t *testing.T) {
	mediaDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yml")
	config := `db:
  host: localhost
  port: 1
  user: postgres
  password: postgres
  database: postgres
storage:
  media_dir: ` + mediaDir + `
  stores:
    tokyo:
      kind: fs
    s3main:
      kind: s3
      bucket: rokuban-derived
  originals:
    - store: tokyo
  derived: tokyo
mirakcs:
  - site: tokyo
    url: http://mirakc
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		wantError string
	}{
		{name: "unknown", wantError: "is not configured in storage.stores"},
		{name: "s3main", wantError: "storage adopt requires an fs store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs([]string{"--config", configPath, "storage", "adopt", tc.name})
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("storage adopt error = %v, want containing %q", err, tc.wantError)
			}
			if _, err := os.Stat(filepath.Join(mediaDir, mediapath.StoreMarkerFileName)); !os.IsNotExist(err) {
				t.Fatalf("marker exists after rejected adoption: stat err = %v", err)
			}
		})
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

	dbtest.MediaAsset(t, pool, dbtest.Recording(t, pool)) // test-store のコピーを 1 件作る

	// コピーがある別の（空の）root では拒否される。
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

// storage adopt は、スキーマが埋め込みの最新 migration でなければ、目印も行も触らずに失敗する。
// 1 つ戻した DB では、検査が無いと空の root に目印が書かれて成功してしまうので、目印の有無が
// 検査の効きを示す。最新に上げると同じ操作が成功する。
func TestStorageAdoptRequiresCurrentSchema(t *testing.T) {
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
`+storagePolicyYAML("test-store")+`mirakcs:
  - site: tokyo
    url: http://mirakc
`, dbCfg.Host, dbCfg.Port, user, password, dbCfg.Database, dbCfg.SSLMode)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	adopt := func(t *testing.T) error {
		t.Helper()
		cmd := newRootCmd()
		cmd.SetArgs([]string{"--config", configPath, "storage", "adopt", "test-store"})
		cmd.SetOut(&strings.Builder{})
		return cmd.Execute()
	}
	markerPath := filepath.Join(mediaDir, mediapath.StoreMarkerFileName)
	dbtest.MediaAsset(t, pool, dbtest.Recording(t, pool)) // 行の変化を見るための既存行
	snapshot := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(context.Background(),
			`SELECT coalesce(string_agg(row_to_json(c)::text, '|' ORDER BY id), '') FROM media_asset_copies c`).Scan(&s); err != nil {
			t.Fatalf("snapshotting media_asset_copies: %v", err)
		}
		return s
	}
	before := snapshot()
	if before == "" {
		t.Fatal("media_asset_copies is empty; the no-row-change check would pass vacuously")
	}

	ctx := context.Background()
	dsn := dbCfg.DSN()
	if err := db.MigrateDown(ctx, dsn); err != nil {
		t.Fatalf("migrating down one step: %v", err)
	}
	t.Cleanup(func() { _ = db.MigrateUp(ctx, dsn) }) // 後続テストのスキーマを戻す

	t.Run("one migration behind", func(t *testing.T) {
		err := adopt(t)
		if err == nil || !strings.Contains(err.Error(), "run migrate up first") {
			t.Fatalf("adopt one migration behind = %v, want the schema refusal", err)
		}
		if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
			t.Fatalf("marker written despite the schema refusal: stat err = %v", statErr)
		}
		if after := snapshot(); after != before {
			t.Fatalf("media_asset_copies changed despite the schema refusal:\nbefore: %s\nafter:  %s", before, after)
		}
	})

	// 目印の無い root では、既存のコピー行による別の拒否が先に出うる。同名の目印があれば、
	// 拒否はスキーマの検査だけの仕事になる（検査が無いと、この adopt は成功してしまう）。
	t.Run("one migration behind with the same-name marker", func(t *testing.T) {
		if err := mediapath.WriteStoreMarker(mediaDir, "test-store"); err != nil {
			t.Fatal(err)
		}
		err := adopt(t)
		if err == nil || !strings.Contains(err.Error(), "run migrate up first") {
			t.Fatalf("adopt one migration behind with a marker = %v, want the schema refusal", err)
		}
		if after := snapshot(); after != before {
			t.Fatalf("media_asset_copies changed despite the schema refusal:\nbefore: %s\nafter:  %s", before, after)
		}
	})
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}

	if err := db.MigrateUp(ctx, dsn); err != nil {
		t.Fatalf("migrating back up: %v", err)
	}
	t.Run("at the latest migration", func(t *testing.T) {
		// 既存のコピー行があるので、目印の無い root は別の理由で拒まれる。同名の目印を先に置いて、
		// スキーマの検査を通ったあとの成功だけを見る。
		if err := mediapath.WriteStoreMarker(mediaDir, "test-store"); err != nil {
			t.Fatal(err)
		}
		if err := adopt(t); err != nil {
			t.Fatalf("adopt at the latest migration = %v, want success", err)
		}
	})
}
