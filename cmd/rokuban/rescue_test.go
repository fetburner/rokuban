package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/catalog"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 最新世代が不完全なら 1 世代前から復元し、**飛ばしたことを運用者に見せる**
// こと（docs/storage.md §8。黙って古い世代へ落ちない）。
func TestRunRescue_FallsBackAndReportsSkippedGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()

	if _, err := catalog.Write(mediaDir, &catalog.Document{
		Version:    catalog.Version,
		ExportedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}, 7); err != nil {
		t.Fatalf("writing the previous generation: %v", err)
	}
	// 書き込み途中で止まった世代（manifest 未着）。
	torn := filepath.Join(catalog.Dir(mediaDir), "catalog-20260702T000000Z")
	if err := os.MkdirAll(torn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torn, catalog.DocumentFilename), []byte(`{"vers`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runRescue(context.Background(), pool, mediaDir, "default", &out); err != nil {
		t.Fatalf("runRescue: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, filepath.Join("catalog-20260701T000000Z", catalog.DocumentFilename)) {
		t.Errorf("output = %q, want the previous generation as the source", got)
	}
	if !strings.Contains(got, "skipped incomplete generation catalog-20260702T000000Z") {
		t.Errorf("output = %q, want the skipped generation to be reported", got)
	}
}

// 完成世代が 1 つも無いときは走査に落ちるが、「catalog が無い」と「あったが
// 全部不完全」を区別して出すこと。
func TestRunRescue_DistinguishesNoCatalogFromNoCompleteGeneration(t *testing.T) {
	pool := testutil.SetupDB(t)

	t.Run("no catalog at all", func(t *testing.T) {
		var out bytes.Buffer
		if err := runRescue(context.Background(), pool, t.TempDir(), "default", &out); err != nil {
			t.Fatalf("runRescue: %v", err)
		}
		if !strings.Contains(out.String(), "catalog not found") {
			t.Errorf("output = %q, want %q", out.String(), "catalog not found")
		}
	})

	t.Run("only incomplete generations", func(t *testing.T) {
		mediaDir := t.TempDir()
		torn := filepath.Join(catalog.Dir(mediaDir), "catalog-20260702T000000Z")
		if err := os.MkdirAll(torn, 0o755); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer
		if err := runRescue(context.Background(), pool, mediaDir, "default", &out); err != nil {
			t.Fatalf("runRescue: %v", err)
		}
		if !strings.Contains(out.String(), "no complete catalog generation") {
			t.Errorf("output = %q, want %q", out.String(), "no complete catalog generation")
		}
		if !strings.Contains(out.String(), "skipped incomplete generation catalog-20260702T000000Z") {
			t.Errorf("output = %q, want the incomplete generation to be reported", out.String())
		}
	})
}

// 到達不能な DB（127.0.0.1:1）+ 2 サイトのレジストリ。shadowdiff_test.go の
// shadowDiffCmdTestConfigTwoSites と同じ手法。
const rescueCmdTestConfigTwoSites = `
db:
  host: 127.0.0.1
  port: 1
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
  - site: takamatsu
    url: http://mirakc-takamatsu:40772
storage:
  media_dir: /mnt/media
`

const rescueCmdTestConfigOneSite = `
db:
  host: 127.0.0.1
  port: 1
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
storage:
  media_dir: /mnt/media
`

// runRescueCmdForTest は rescue サブコマンドの RunE を実際に走らせる。
// 単体の resolveSiteFlag だけを直接呼ぶテストでは RunE の配線（--site フラグの
// 登録・requireSingleSite からの置き換え）が検証できない（server_test.go の
// runServerCmdForTest と同じ理由）。
func runRescueCmdForTest(t *testing.T, configPath string, args ...string) error {
	t.Helper()
	cmd := newRescueCmd()
	cmd.Flags().String("config", configPath, "")
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd.Execute()
}

// mirakcs: 2 要素 + --site tokyo は site 解決を通り、DB 接続まで進むこと
// （issue #533 の受け入れ基準 1）。
func TestRescueCmd_SiteFlag_MultiSiteRegistryResolvesNamedSite(t *testing.T) {
	path := writeServerTestConfig(t, rescueCmdTestConfigTwoSites)
	err := runRescueCmdForTest(t, path, "--site", "tokyo")
	if err == nil {
		t.Fatal("到達不能な DB を指しているので error を期待したが nil だった")
	}
	if !strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v, want to fail at the DB stage (= site 解決を通ったこと)", err)
	}
}

// mirakcs: 2 要素 + --site 省略は DB に触る前の起動エラーになること
// （issue #533 の受け入れ基準 1）。
func TestRescueCmd_SiteFlag_MultiSiteRegistryRequiresSite(t *testing.T) {
	path := writeServerTestConfig(t, rescueCmdTestConfigTwoSites)
	err := runRescueCmdForTest(t, path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--site is required") {
		t.Errorf("err = %v, want the --site required error (DB に触る前に落ちること)", err)
	}
	if strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v: DB まで進んでいる（--site の必須化が効いていない）", err)
	}
}

// レジストリに無い --site はタイポの早期検出のため起動エラーになり、
// どの site にも一致しないまま静かに成功してはならない（issue #533 の「罠」）。
func TestRescueCmd_SiteFlag_UnknownSiteIsError(t *testing.T) {
	path := writeServerTestConfig(t, rescueCmdTestConfigTwoSites)
	err := runRescueCmdForTest(t, path, "--site", "osaka")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown site") {
		t.Errorf("err = %v, want an unknown-site error", err)
	}
	if strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v: DB まで進んでいる（未知 site の照合が効いていない）", err)
	}
}

// mirakcs: 1 要素では --site 省略でも従来どおり動くこと（issue #533 の受け入れ
// 基準 2）。
func TestRescueCmd_SiteFlag_SingleSiteRegistryOptional(t *testing.T) {
	path := writeServerTestConfig(t, rescueCmdTestConfigOneSite)
	err := runRescueCmdForTest(t, path)
	if err == nil {
		t.Fatal("到達不能な DB を指しているので error を期待したが nil だった")
	}
	if !strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v, want to fail at the DB stage (単一サイトレジストリは --site 無しで解決する)", err)
	}
}
