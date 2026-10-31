package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FS 置き場が 2 つ以上ある構成でも、置き場をマウントしないロール（api）は --stores 無しで起動の検査を通る。
// worker と streamer は、暗黙に「全部」をマウントしないので --stores を要求する。--stores の効かない指定は拒む。
func TestServerCmd_StoresFlagIsOnlyRequiredByRolesThatMountStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	config := strings.Replace(serverCmdTestConfig, "storage:\n  media_dir: /mnt/media\n", `storage:
  stores:
    tokyo: { kind: fs, root: /mnt/tokyo }
    takamatsu: { kind: fs, root: /mnt/takamatsu }
  originals:
    - store: tokyo
  derived: tokyo
  scratch_dir: /tmp/rokuban
`, 1)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	// 検査を通ると DB への接続（到達不能）で止まる。
	if err := runServerCmdForTest(t, path, "--roles", "api", "--sites", "tokyo"); err == nil || !strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("api with two fs stores and no --stores: error = %v, want to pass the store checks and reach the database", err)
	}
	for _, role := range []string{"worker", "streamer"} {
		args := []string{"--roles", role, "--sites", "tokyo"}
		if err := runServerCmdForTest(t, path, args...); err == nil || !strings.Contains(err.Error(), "--stores is required") {
			t.Errorf("%s with two fs stores and no --stores: error = %v, want --stores is required", role, err)
		}
	}
	if err := runServerCmdForTest(t, path, "--roles", "api", "--sites", "tokyo", "--stores", "tokyo"); err == nil || !strings.Contains(err.Error(), "has no effect") {
		t.Errorf("api with --stores: error = %v, want the has-no-effect rejection", err)
	}
	// 2 つの置き場を 1 プロセスでマウントしても、別の root を持つので検査を通る。
	if err := runServerCmdForTest(t, path, "--roles", "streamer", "--sites", "tokyo", "--stores", "tokyo,takamatsu"); err == nil || !strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("streamer mounting both stores: error = %v, want to reach the database", err)
	}
	// 定義にない名前は拒む。
	if err := runServerCmdForTest(t, path, "--roles", "streamer", "--sites", "tokyo", "--stores", "nagoya"); err == nil || !strings.Contains(err.Error(), "unknown store(s) nagoya") {
		t.Errorf("streamer with an unknown store: error = %v, want unknown store(s) nagoya", err)
	}
}
