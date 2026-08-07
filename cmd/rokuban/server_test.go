package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
)

func writeServerTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// 既存の `mirakc: {url, site}` だけの config は 1 文字も変えずに動き、--sites
// 省略でそのサイトに束縛される（issue #183 の受け入れ基準 1）。
func TestServerSiteBinding_ExistingSingleMirakcConfig_Unchanged(t *testing.T) {
	path := writeServerTestConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
  site: home
storage:
  media_dir: /mnt/media
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cmd := newSitesTestCmd(t)
	bound, err := resolveSiteBinding(cmd, cfg.Registry())
	if err != nil {
		t.Fatalf("resolveSiteBinding: %v", err)
	}
	if len(bound) != 1 || bound[0].Site != "home" || bound[0].URL != "http://mirakc.local:40772" {
		t.Errorf("bound = %+v, want [{home http://mirakc.local:40772}]", bound)
	}
}

// mirakcs: 2 要素 + --sites tokyo は tokyo だけに束縛される
// （issue #183 の受け入れ基準 2）。
func TestServerSiteBinding_MirakcsRegistry_BindsToNamedSite(t *testing.T) {
	path := writeServerTestConfig(t, `
db:
  host: localhost
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
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cmd := newSitesTestCmd(t)
	if err := cmd.Flags().Set(siteFlagName, "tokyo"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	bound, err := resolveSiteBinding(cmd, cfg.Registry())
	if err != nil {
		t.Fatalf("resolveSiteBinding: %v", err)
	}
	if len(bound) != 1 || bound[0].Site != "tokyo" || bound[0].URL != "http://mirakc-tokyo:40772" {
		t.Errorf("bound = %+v, want [{tokyo http://mirakc-tokyo:40772}]", bound)
	}
}

// mirakcs: 2 要素 + --sites 省略は起動エラーになる（issue #183 の受け入れ基準 3）。
func TestServerSiteBinding_MirakcsRegistry_UnspecifiedSitesIsError(t *testing.T) {
	path := writeServerTestConfig(t, `
db:
  host: localhost
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
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cmd := newSitesTestCmd(t)
	if _, err := resolveSiteBinding(cmd, cfg.Registry()); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// mirakc: と mirakcs: の同時指定は Load 自体が起動エラーにする
// （issue #183 の受け入れ基準 4）。
func TestServerSiteBinding_MirakcAndMirakcsBothSet_LoadFails(t *testing.T) {
	path := writeServerTestConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
storage:
  media_dir: /mnt/media
`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// resolveRiverClientKind はロール指定が実際に実行する仕事を制約するための唯一の
// 分岐点。ここが誤ると、--roles watcher 単独のプロセスが worker.NewWorkers の
// フルのワーカー群（EncodeWorker/ThumbnailWorker を含む）を登録し、ffmpeg/ffprobe
// を検査しないまま encode/thumbnail ジョブを実行しうる（不変条件 4 違反、issue #113）。
func TestResolveRiverClientKind(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		want  riverClientKind
	}{
		{"watcher alone must not get the full worker set", []string{"watcher"}, riverClientInsertOnly},
		{"worker alone", []string{"worker"}, riverClientFull},
		{"worker and watcher together (monolith) keep the shared full client", []string{"worker", "watcher"}, riverClientFull},
		{"watcher and worker together regardless of order", []string{"watcher", "worker"}, riverClientFull},
		{"api alone needs no river client here (uses its own insert-only client)", []string{"api"}, riverClientNone},
		{"streamer alone", []string{"streamer"}, riverClientNone},
		{"notifier alone", []string{"notifier"}, riverClientNone},
		{"api and watcher: watcher still must not get the full worker set", []string{"api", "watcher"}, riverClientInsertOnly},
		{"all roles together", allRoles, riverClientFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveRiverClientKind(tt.roles); got != tt.want {
				t.Errorf("resolveRiverClientKind(%v) = %v, want %v", tt.roles, got, tt.want)
			}
		})
	}
}
