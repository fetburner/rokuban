package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`

func TestLoad_Minimal(t *testing.T) {
	path := writeConfig(t, minimalConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.Listen != ":40773" {
		t.Errorf("server.listen = %q, want %q", cfg.Server.Listen, ":40773")
	}
	if cfg.DB.Port != 5432 {
		t.Errorf("db.port = %d, want %d", cfg.DB.Port, 5432)
	}
	if cfg.DB.SSLMode != "disable" {
		t.Errorf("db.sslmode = %q, want %q", cfg.DB.SSLMode, "disable")
	}
	if cfg.Storage.ScratchDir != "/var/tmp/rokuban" {
		t.Errorf("storage.scratch_dir = %q, want %q", cfg.Storage.ScratchDir, "/var/tmp/rokuban")
	}
	// 既定では Go が直接配る（X-Accel-Redirect なし）
	if cfg.Storage.AccelLocation != "" {
		t.Errorf("storage.accel_location = %q, want empty", cfg.Storage.AccelLocation)
	}
	if cfg.Ingest.Concurrency != 2 {
		t.Errorf("ingest.concurrency = %d, want %d", cfg.Ingest.Concurrency, 2)
	}
	if cfg.Ingest.StallTimeout != 30*time.Second {
		t.Errorf("ingest.stall_timeout = %v, want %v", cfg.Ingest.StallTimeout, 30*time.Second)
	}
	if cfg.Epg.SyncInterval != 10*time.Minute {
		t.Errorf("epg.sync_interval = %v, want %v", cfg.Epg.SyncInterval, 10*time.Minute)
	}
	if cfg.Epg.RetentionGrace != 24*time.Hour {
		t.Errorf("epg.retention_grace = %v, want %v", cfg.Epg.RetentionGrace, 24*time.Hour)
	}
	if !cfg.Worker.PeriodicJobs {
		t.Error("worker.periodic_jobs の既定値は true (monolith/Docker では既定で有効)")
	}
	if len(cfg.Worker.Queues) != 0 {
		t.Errorf("worker.queues = %v, want empty (既定は全キュー)", cfg.Worker.Queues)
	}
	if cfg.Encode.FFmpeg != "ffmpeg" {
		t.Errorf("encode.ffmpeg = %q, want %q", cfg.Encode.FFmpeg, "ffmpeg")
	}
	if cfg.Encode.Concurrency != 1 {
		t.Errorf("encode.concurrency = %d, want 1", cfg.Encode.Concurrency)
	}
	if cfg.Encode.ThumbnailConcurrency != 1 {
		t.Errorf("encode.thumbnail_concurrency = %d, want 1", cfg.Encode.ThumbnailConcurrency)
	}
	if cfg.Webhook.URL != "" {
		t.Errorf("webhook.url = %q, want empty (no-op by default)", cfg.Webhook.URL)
	}
	if cfg.Webhook.Timeout != 5*time.Second {
		t.Errorf("webhook.timeout = %v, want %v", cfg.Webhook.Timeout, 5*time.Second)
	}
	if len(cfg.Webhook.Events) != 0 {
		t.Errorf("webhook.events = %v, want empty (all known events)", cfg.Webhook.Events)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want %q", cfg.Log.Level, "info")
	}
	if cfg.Log.Format != "json" {
		t.Errorf("log.format = %q, want %q", cfg.Log.Format, "json")
	}
}

func TestLoad_EnvExpansion(t *testing.T) {
	t.Setenv("TEST_DB_USER", "myuser")
	t.Setenv("TEST_DB_PASS", "mypass")

	path := writeConfig(t, `
db:
  host: localhost
  user: ${TEST_DB_USER}
  password: ${TEST_DB_PASS}
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DB.User != "myuser" {
		t.Errorf("db.user = %q, want %q", cfg.DB.User, "myuser")
	}
	if cfg.DB.Password != "mypass" {
		t.Errorf("db.password = %q, want %q", cfg.DB.Password, "mypass")
	}
}

func TestLoad_EnvDefault(t *testing.T) {
	path := writeConfig(t, `
db:
  host: localhost
  user: ${UNSET_VAR:-fallback_user}
  password: secret
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DB.User != "fallback_user" {
		t.Errorf("db.user = %q, want %q", cfg.DB.User, "fallback_user")
	}
}

func TestLoad_DollarEscape(t *testing.T) {
	t.Setenv("TEST_DB_PASS", "secret")

	path := writeConfig(t, `
db:
  host: localhost
  user: rokuban
  password: ${TEST_DB_PASS}
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
encode:
  ffmpeg: ffmpeg
  ffprobe: ffprobe
  profiles:
    - name: "literal $${NOT_A_VAR}"
      container: mp4
      video_codec: libx264
      audio_codec: aac
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Encode.Profiles[0].Name != "literal ${NOT_A_VAR}" {
		t.Errorf("profile name = %q, want %q", cfg.Encode.Profiles[0].Name, "literal ${NOT_A_VAR}")
	}
}

func TestLoad_MissingRequiredKeys(t *testing.T) {
	path := writeConfig(t, `
log:
  level: debug
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	fe := ve.FieldErrors()
	if len(fe) != 6 {
		t.Fatalf("expected 6 validation errors, got %d: %v", len(fe), err)
	}
}

func TestLoad_DBPoolingDefaults(t *testing.T) {
	path := writeConfig(t, minimalConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DB.MaxConns != 0 {
		t.Errorf("db.max_conns = %d, want 0 (auto from roles)", cfg.DB.MaxConns)
	}
	if cfg.DB.APIStatementTimeout != 0 {
		t.Errorf("db.api_statement_timeout = %v, want 0 (built-in default)", cfg.DB.APIStatementTimeout)
	}
	if cfg.DB.PoolerCompat {
		t.Error("db.pooler_compat の既定値は false")
	}
}

func TestLoad_DBPoolingOverridden(t *testing.T) {
	path := writeConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
  max_conns: 20
  api_statement_timeout: 15s
  pooler_compat: true
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DB.MaxConns != 20 {
		t.Errorf("db.max_conns = %d, want 20", cfg.DB.MaxConns)
	}
	if cfg.DB.APIStatementTimeout != 15*time.Second {
		t.Errorf("db.api_statement_timeout = %v, want 15s", cfg.DB.APIStatementTimeout)
	}
	if !cfg.DB.PoolerCompat {
		t.Error("db.pooler_compat = false, want true")
	}
}

func TestLoad_DBMaxConnsNegative(t *testing.T) {
	path := writeConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
  max_conns: -1
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for negative db.max_conns, got nil")
	}
}

// db セクション内の typo も strict パースで検出できることを確認する
// （既存の TestLoad_UnknownKey はトップレベルの typo しか見ていない）。
func TestLoad_UnknownKey_NestedInDBSection(t *testing.T) {
	path := writeConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
  max_cons: 20
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for typo'd db.max_cons, got nil")
	}
}

func TestLoad_UnknownKey(t *testing.T) {
	path := writeConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
typo_key: oops
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/config.yml")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLoad_AllFieldsOverridden(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":8080"
  allowed_hosts: [example.com, rokuban.local]
db:
  host: db.example.com
  port: 5433
  user: admin
  password: hunter2
  database: rokuban_prod
  sslmode: require
mirakc:
  url: http://10.0.0.1:40772
storage:
  media_dir: /data/media
  scratch_dir: /data/scratch
  accel_location: /_media/
ingest:
  concurrency: 4
  stall_timeout: 2m
epg:
  sync_interval: 30m
  retention_grace: 48h
worker:
  periodic_jobs: false
  queues: [ruler, epg]
encode:
  ffmpeg: /usr/local/bin/ffmpeg
  ffprobe: /usr/local/bin/ffprobe
  concurrency: 3
  thumbnail_concurrency: 2
  profiles:
    - name: h264
      container: mp4
      video_codec: libx264
      audio_codec: aac
      height: 1080
      crf: 23
      preset: medium
    - name: h265
      container: mkv
      video_codec: libx265
      audio_codec: aac
webhook:
  url: https://hooks.example.com/rokuban
  secret: s3cret
  timeout: 10s
  events:
    - recording.finished
    - recording.failed
log:
  level: debug
  format: text
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.Listen != ":8080" {
		t.Errorf("server.listen = %q, want %q", cfg.Server.Listen, ":8080")
	}
	if len(cfg.Server.AllowedHosts) != 2 {
		t.Errorf("server.allowed_hosts len = %d, want 2", len(cfg.Server.AllowedHosts))
	}
	if cfg.DB.Host != "db.example.com" {
		t.Errorf("db.host = %q, want %q", cfg.DB.Host, "db.example.com")
	}
	if cfg.DB.Port != 5433 {
		t.Errorf("db.port = %d, want %d", cfg.DB.Port, 5433)
	}
	if cfg.DB.SSLMode != "require" {
		t.Errorf("db.sslmode = %q, want %q", cfg.DB.SSLMode, "require")
	}
	if cfg.Mirakc.URL != "http://10.0.0.1:40772" {
		t.Errorf("mirakc.url = %q, want %q", cfg.Mirakc.URL, "http://10.0.0.1:40772")
	}
	if cfg.Storage.MediaDir != "/data/media" {
		t.Errorf("storage.media_dir = %q, want %q", cfg.Storage.MediaDir, "/data/media")
	}
	if cfg.Storage.ScratchDir != "/data/scratch" {
		t.Errorf("storage.scratch_dir = %q, want %q", cfg.Storage.ScratchDir, "/data/scratch")
	}
	if cfg.Storage.AccelLocation != "/_media/" {
		t.Errorf("storage.accel_location = %q, want %q", cfg.Storage.AccelLocation, "/_media/")
	}
	if cfg.Ingest.Concurrency != 4 {
		t.Errorf("ingest.concurrency = %d, want %d", cfg.Ingest.Concurrency, 4)
	}
	if cfg.Ingest.StallTimeout != 2*time.Minute {
		t.Errorf("ingest.stall_timeout = %v, want %v", cfg.Ingest.StallTimeout, 2*time.Minute)
	}
	if cfg.Epg.SyncInterval != 30*time.Minute {
		t.Errorf("epg.sync_interval = %v, want %v", cfg.Epg.SyncInterval, 30*time.Minute)
	}
	if cfg.Epg.RetentionGrace != 48*time.Hour {
		t.Errorf("epg.retention_grace = %v, want %v", cfg.Epg.RetentionGrace, 48*time.Hour)
	}
	if cfg.Worker.PeriodicJobs {
		t.Error("worker.periodic_jobs = true, want false")
	}
	if want := []string{"ruler", "epg"}; !slices.Equal(cfg.Worker.Queues, want) {
		t.Errorf("worker.queues = %v, want %v", cfg.Worker.Queues, want)
	}
	if cfg.Encode.FFmpeg != "/usr/local/bin/ffmpeg" {
		t.Errorf("encode.ffmpeg = %q, want %q", cfg.Encode.FFmpeg, "/usr/local/bin/ffmpeg")
	}
	if cfg.Encode.Concurrency != 3 {
		t.Errorf("encode.concurrency = %d, want 3", cfg.Encode.Concurrency)
	}
	if cfg.Encode.ThumbnailConcurrency != 2 {
		t.Errorf("encode.thumbnail_concurrency = %d, want 2", cfg.Encode.ThumbnailConcurrency)
	}
	if len(cfg.Encode.Profiles) != 2 {
		t.Errorf("encode.profiles len = %d, want 2", len(cfg.Encode.Profiles))
	}
	p0 := cfg.Encode.Profiles[0]
	if p0.Name != "h264" || p0.Container != "mp4" || p0.VideoCodec != "libx264" ||
		p0.AudioCodec != "aac" || p0.Height != 1080 || p0.Preset != "medium" {
		t.Errorf("profiles[0] = %+v", p0)
	}
	if p0.CRF == nil || *p0.CRF != 23 {
		t.Errorf("profiles[0].crf = %v, want 23", p0.CRF)
	}
	if cfg.Webhook.URL != "https://hooks.example.com/rokuban" {
		t.Errorf("webhook.url = %q, want %q", cfg.Webhook.URL, "https://hooks.example.com/rokuban")
	}
	if cfg.Webhook.Secret != "s3cret" {
		t.Errorf("webhook.secret = %q, want %q", cfg.Webhook.Secret, "s3cret")
	}
	if cfg.Webhook.Timeout != 10*time.Second {
		t.Errorf("webhook.timeout = %v, want %v", cfg.Webhook.Timeout, 10*time.Second)
	}
	if want := []string{"recording.finished", "recording.failed"}; !slices.Equal(cfg.Webhook.Events, want) {
		t.Errorf("webhook.events = %v, want %v", cfg.Webhook.Events, want)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want %q", cfg.Log.Level, "debug")
	}
	if cfg.Log.Format != "text" {
		t.Errorf("log.format = %q, want %q", cfg.Log.Format, "text")
	}
}

func TestLoad_EncodeProfileValidation(t *testing.T) {
	base := `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
encode:
  profiles:
`

	t.Run("duplicate name", func(t *testing.T) {
		path := writeConfig(t, base+`
    - name: h264
      container: mp4
      video_codec: libx264
      audio_codec: aac
    - name: h264
      container: mkv
      video_codec: libx265
      audio_codec: aac
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for duplicate profile name")
		}
		if !strings.Contains(err.Error(), "duplicate name") {
			t.Errorf("error = %v, want mention of duplicate name", err)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		path := writeConfig(t, base+`
    - name: ""
      container: mp4
      video_codec: libx264
      audio_codec: aac
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for empty profile name")
		}
	})

	t.Run("invalid container", func(t *testing.T) {
		path := writeConfig(t, base+`
    - name: h264
      container: ts
      video_codec: libx264
      audio_codec: aac
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for invalid container")
		}
		if !strings.Contains(err.Error(), "container") {
			t.Errorf("error = %v, want mention of container", err)
		}
	})

	t.Run("missing video_codec", func(t *testing.T) {
		path := writeConfig(t, base+`
    - name: h264
      container: mp4
      audio_codec: aac
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for missing video_codec")
		}
	})

	t.Run("missing audio_codec", func(t *testing.T) {
		path := writeConfig(t, base+`
    - name: h264
      container: mp4
      video_codec: libx264
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for missing audio_codec")
		}
	})

	t.Run("negative height", func(t *testing.T) {
		path := writeConfig(t, base+`
    - name: h264
      container: mp4
      video_codec: libx264
      audio_codec: aac
      height: -1
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for negative height")
		}
	})

	t.Run("negative concurrency", func(t *testing.T) {
		path := writeConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
encode:
  concurrency: -1
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for negative concurrency")
		}
	})
}

func TestEncodeConfig_Profile(t *testing.T) {
	cfg := EncodeConfig{Profiles: []EncodeProfile{
		{Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac"},
		{Name: "h265", Container: "mkv", VideoCodec: "libx265", AudioCodec: "aac"},
	}}
	p, ok := cfg.Profile("h265")
	if !ok || p.Name != "h265" {
		t.Fatalf("Profile(h265) = (%+v, %v), want found", p, ok)
	}
	if _, ok := cfg.Profile("missing"); ok {
		t.Error("Profile(missing) should be not found")
	}
	if got := cfg.ProfileNames(); !slices.Equal(got, []string{"h264", "h265"}) {
		t.Errorf("ProfileNames() = %v, want [h264 h265]", got)
	}
}

func TestEncodeConfig_ValidateTools(t *testing.T) {
	// 存在しないコマンド名は失敗する（LookPath の経路）。
	// 実在する ffmpeg は環境依存なので「無い方」だけ固定する。
	cfg := EncodeConfig{
		FFmpeg:  "rokuban-no-such-ffmpeg-binary",
		FFprobe: "rokuban-no-such-ffprobe-binary",
	}
	err := cfg.ValidateTools()
	if err == nil {
		t.Fatal("expected error for missing tools")
	}
	if !strings.Contains(err.Error(), "ffmpeg") && !strings.Contains(err.Error(), "ffprobe") {
		t.Errorf("error = %v, want mention of missing tool", err)
	}
}

// DSN は値を引用しないと空値・空白入りの値で壊れる。
// 引用がないと `password= dbname=x` となり、libpq は次のトークン（`dbname=x`）を
// パスワードの値として読む。結果 dbname が未指定になり、ユーザー名と同名の
// データベースへ黙って接続してしまう（実際にテスト DB でこれを踏んだ）。
func TestDBConfigDSN_QuotesValues(t *testing.T) {
	cases := map[string]struct {
		cfg  DBConfig
		want string
	}{
		"空パスワード": {
			cfg: DBConfig{Host: "localhost", Port: 5432, User: "u", Password: "",
				Database: "d", SSLMode: "disable"},
			want: "host='localhost' port=5432 user='u' password='' dbname='d' sslmode='disable'",
		},
		"空白入りパスワード": {
			cfg: DBConfig{Host: "localhost", Port: 5432, User: "u", Password: "p a s s",
				Database: "d", SSLMode: "disable"},
			want: "host='localhost' port=5432 user='u' password='p a s s' dbname='d' sslmode='disable'",
		},
		"引用符とバックスラッシュ": {
			cfg: DBConfig{Host: "localhost", Port: 5432, User: "u", Password: `it's\ok`,
				Database: "d", SSLMode: "disable"},
			want: `host='localhost' port=5432 user='u' password='it\'s\\ok' dbname='d' sslmode='disable'`,
		},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tt.cfg.DSN(); got != tt.want {
				t.Errorf("DSN() =\n  %s\nwant\n  %s", got, tt.want)
			}
		})
	}
}
