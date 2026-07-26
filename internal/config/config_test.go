package config

import (
	"errors"
	"os"
	"path/filepath"
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
	if cfg.Epg.SyncInterval != 10*time.Minute {
		t.Errorf("epg.sync_interval = %v, want %v", cfg.Epg.SyncInterval, 10*time.Minute)
	}
	if cfg.Epg.RetentionGrace != 24*time.Hour {
		t.Errorf("epg.retention_grace = %v, want %v", cfg.Epg.RetentionGrace, 24*time.Hour)
	}
	if cfg.Encode.FFmpeg != "ffmpeg" {
		t.Errorf("encode.ffmpeg = %q, want %q", cfg.Encode.FFmpeg, "ffmpeg")
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
epg:
  sync_interval: 30m
  retention_grace: 48h
encode:
  ffmpeg: /usr/local/bin/ffmpeg
  ffprobe: /usr/local/bin/ffprobe
  profiles:
    - name: h264
    - name: h265
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
	if cfg.Epg.SyncInterval != 30*time.Minute {
		t.Errorf("epg.sync_interval = %v, want %v", cfg.Epg.SyncInterval, 30*time.Minute)
	}
	if cfg.Epg.RetentionGrace != 48*time.Hour {
		t.Errorf("epg.retention_grace = %v, want %v", cfg.Epg.RetentionGrace, 48*time.Hour)
	}
	if cfg.Encode.FFmpeg != "/usr/local/bin/ffmpeg" {
		t.Errorf("encode.ffmpeg = %q, want %q", cfg.Encode.FFmpeg, "/usr/local/bin/ffmpeg")
	}
	if len(cfg.Encode.Profiles) != 2 {
		t.Errorf("encode.profiles len = %d, want 2", len(cfg.Encode.Profiles))
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want %q", cfg.Log.Level, "debug")
	}
	if cfg.Log.Format != "text" {
		t.Errorf("log.format = %q, want %q", cfg.Log.Format, "text")
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
