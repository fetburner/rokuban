package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalog"
)

func writeVerifyTestGeneration(t *testing.T, mediaDir string, at time.Time) string {
	t.Helper()
	genDir, err := catalog.Write(mediaDir, &catalog.Document{
		Version:    catalog.Version,
		ExportedAt: at,
	}, 1000)
	if err != nil {
		t.Fatalf("catalog.Write(%s): %v", at, err)
	}
	return genDir
}

// 完成世代と不完全世代を区別して出し、rescue が使う世代と「最新が不完全」を
// 出力すること（issue #289 の受け入れ 2）。
func TestRunCatalogVerify_ReportsCompleteAndIncomplete(t *testing.T) {
	mediaDir := t.TempDir()
	writeVerifyTestGeneration(t, mediaDir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	newest := writeVerifyTestGeneration(t, mediaDir, time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC))
	// 最新世代の完成宣言を落とす（= 書き込み途中で止まった世代の見え方）。
	if err := os.Remove(filepath.Join(newest, catalog.ManifestFilename)); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runCatalogVerify(mediaDir, &out); err != nil {
		t.Fatalf("runCatalogVerify: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"[incomplete] catalog-20260702T000000Z",
		"[complete]   catalog-20260701T000000Z",
		"complete generations: 1",
		"rescue would use: catalog-20260701T000000Z",
		"warning: the newest generation (catalog-20260702T000000Z) is incomplete",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// 完成世代が 1 つも無ければ非ゼロ終了する（error を返す）こと。
// **バックアップが取れていないことを沈黙で返さない。**
func TestRunCatalogVerify_NoUsableSnapshotIsAnError(t *testing.T) {
	mediaDir := t.TempDir()

	var out bytes.Buffer
	err := runCatalogVerify(mediaDir, &out)
	if err == nil {
		t.Fatal("runCatalogVerify succeeded with no catalog at all")
	}
	if !strings.Contains(err.Error(), "no usable catalog snapshot") {
		t.Errorf("err = %v", err)
	}

	// 世代ディレクトリがあっても完成していなければ同じ（存在は完成ではない）。
	torn := filepath.Join(catalog.Dir(mediaDir), "catalog-20260702T000000Z")
	if err := os.MkdirAll(torn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torn, catalog.ManifestFilename), []byte(`{"manifestVer`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCatalogVerify(mediaDir, &out); err == nil {
		t.Fatalf("runCatalogVerify succeeded with only an incomplete generation:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "[incomplete] catalog-20260702T000000Z") {
		t.Errorf("output should show why the generation was rejected:\n%s", out.String())
	}
}

// **manifest があることは完成の証明にならない**（この PR 本体の主張）。
// 同サイズの 1 バイト化けは JSON としては読めてしまうので、verify が
// sha256 まで照合していないとここが通ってしまう。
func TestRunCatalogVerify_DetectsSilentCorruption(t *testing.T) {
	mediaDir := t.TempDir()
	genDir := writeVerifyTestGeneration(t, mediaDir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	docPath := filepath.Join(genDir, catalog.DocumentFilename)
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	flipped := []byte(strings.Replace(string(raw), `"version"`, `"versiop"`, 1))
	if len(flipped) != len(raw) || string(flipped) == string(raw) {
		t.Fatalf("test setup: need a same-size flip (%d -> %d)", len(raw), len(flipped))
	}
	if err := os.WriteFile(docPath, flipped, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runCatalogVerify(mediaDir, &out); err == nil {
		t.Fatalf("runCatalogVerify accepted a silently corrupted generation:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "sha256 mismatch") {
		t.Errorf("output should name the mismatch:\n%s", out.String())
	}
}

// **DB に一切触らないこと**（issue #289 の受け入れ 1）。届かない DB を指した
// config で `rokuban catalog verify` を実行しても成功する = 接続していない。
func TestCatalogVerifyCmd_DoesNotTouchTheDatabase(t *testing.T) {
	mediaDir := t.TempDir()
	writeVerifyTestGeneration(t, mediaDir, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	// port 1 には誰も listen していない。DB を掴もうとすれば必ず失敗する。
	if err := os.WriteFile(configPath, []byte(`
db:
  host: 127.0.0.1
  port: 1
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: `+mediaDir+`
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"catalog", "verify", "--config", configPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("catalog verify: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "rescue would use: catalog-20260701T000000Z") {
		t.Errorf("output = %q", out.String())
	}
}
