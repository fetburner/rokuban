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
	if err := runRescue(context.Background(), pool, mediaDir, []string{"default"}, &out); err != nil {
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
		if err := runRescue(context.Background(), pool, t.TempDir(), []string{"default"}, &out); err != nil {
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
		if err := runRescue(context.Background(), pool, mediaDir, []string{"default"}, &out); err != nil {
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

// 到達不能な DB（127.0.0.1:1）+ 2 サイトのレジストリを使う。
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

// runRescueCmdForTest は rescue サブコマンドの RunE を実際に走らせる。
// コマンドに廃止済みの `--site` を渡したとき、RunE まで進まず Cobra の
// unknown-flag エラーになることもこの入口で検証する。
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

// `rescue` は site 非依存になったため、複数サイトのレジストリでも `--site` 無しで
// DB まで進む。
func TestRescueCmd_MultiSiteRegistryDoesNotRequireSiteFlag(t *testing.T) {
	path := writeServerTestConfig(t, rescueCmdTestConfigTwoSites)
	err := runRescueCmdForTest(t, path)
	if err == nil {
		t.Fatal("到達不能な DB を指しているので error を期待したが nil だった")
	}
	if !strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v, want to fail at the DB stage", err)
	}
}

// `--site` は rescue の site 解決とともに廃止した。未知のフラグを黙って無視せず、
// DB 接続より前にエラーにする。
func TestRescueCmd_RejectsRemovedSiteFlag(t *testing.T) {
	path := writeServerTestConfig(t, rescueCmdTestConfigTwoSites)
	err := runRescueCmdForTest(t, path, "--site", "tokyo")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("err = %v, want an unknown-flag error", err)
	}
	if strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v: DB まで進んでいる（廃止済み --site を受け付けている）", err)
	}
}

func TestRunRescue_ReportsMissingCatalogFiles(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	doc := &catalog.Document{Version: catalog.Version, ExportedAt: at,
		Recordings:  []catalog.Recording{{ID: 1, Source: "manual", Site: "default", NetworkID: 1, ServiceID: 1, EventID: 1, ServiceName: "test", ChannelType: "GR", Channel: "27", Title: "test", ProgramStartAt: at, Status: "finished", CreatedAt: at, UpdatedAt: at}},
		MediaAssets: []catalog.MediaAsset{{ID: 1, RecordingID: 1, Kind: "original", RelPath: "sites/default/missing.m2ts", State: "active", CreatedAt: at, UpdatedAt: at}},
	}
	mediaDir := t.TempDir()
	if _, err := catalog.Write(mediaDir, doc, 7); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runRescue(ctx, pool, mediaDir, []string{"default"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "warning: 1 media file(s) missing; not restored as active") {
		t.Fatalf("missing-file warning absent: %s", out.String())
	}
}
