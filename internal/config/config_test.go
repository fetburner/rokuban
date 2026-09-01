package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/ffargs"
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
mirakcs:
  - site: default
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
	// server.trust_forwarded_host は既定 false（opt-in）。X-Forwarded-Host を
	// 信頼しない安全側の初期値であること（issue #216）。
	if cfg.Server.TrustForwardedHost {
		t.Error("server.trust_forwarded_host default = true, want false")
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
	// ruler.retract_grace 未設定の既定は 1h（0 の「無効」とは区別される。
	// RulerConfig.RetractGrace のコメント参照）。
	if cfg.Ruler.RetractGrace == nil || *cfg.Ruler.RetractGrace != time.Hour {
		t.Errorf("ruler.retract_grace = %v, want %v", cfg.Ruler.RetractGrace, time.Hour)
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
mirakcs:
  - site: default
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
mirakcs:
  - site: default
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
mirakcs:
  - site: default
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

// mirakcs は missingRequired に載せない（空配列を許容する型なので struct タグの
// required 相当では表現できないため。missingRequired のコメント参照）ので、
// ここで数えるのは db.* (4) + storage.media_dir (1) の 5 件。mirakcs が空の
// 場合の検出は validateMirakcRegistry が別のエラーとして行う
// （TestLoad_MirakcRegistry の "empty mirakcs is an error" が確認する）。
//
// **1 件目で止まらず全件返すこと**が規約 4 の要点なので、件数だけでなく
// キー名も突き合わせる（期待値はリテラル。実装の順序を読んで比べない）。
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
	want := []string{"db.host", "db.user", "db.password", "db.database", "storage.media_dir"}
	got := ve.MissingKeys()
	if !slices.Equal(got, want) {
		t.Fatalf("MissingKeys() = %v, want %v", got, want)
	}
	// メッセージにも全件出ること（Error() が 1 件目だけ出す実装に退行しない）。
	for _, key := range want {
		if !strings.Contains(err.Error(), key+" is required") {
			t.Errorf("error message %q is missing %q", err.Error(), key+" is required")
		}
	}
}

// mirakcsBase は db/storage だけ満たし、mirakcs はテストごとに足す。
const mirakcsBase = `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
storage:
  media_dir: /mnt/media
`

func TestLoad_MirakcRegistry(t *testing.T) {
	t.Run("empty mirakcs is an error", func(t *testing.T) {
		path := writeConfig(t, mirakcsBase)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "mirakcs is required") {
			t.Errorf("error = %v, want mention of mirakcs required", err)
		}
	})

	// mirakc:（単数）は issue #444 で廃止した糖衣。旧キーを書いた config は struct に
	// 対応するフィールドが無いため、strict パースの未知キー検出で落ちる
	// （黙って無視されない）。
	// 壊し方: loadFromString から yaml.Strict() を外す（この分岐が無いと
	// cfg.Mirakcs は素通りし、mirakc.url は静かに無視される）。
	t.Run("legacy mirakc: key is an unknown field error", func(t *testing.T) {
		path := writeConfig(t, mirakcsBase+`
mirakc:
  url: http://mirakc.local:40772
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for legacy mirakc: key, got nil")
		}
		if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), `"mirakc"`) {
			t.Errorf("error = %v, want mention of unknown field \"mirakc\"", err)
		}
	})

	t.Run("mirakcs registry with multiple valid sites", func(t *testing.T) {
		path := writeConfig(t, mirakcsBase+`
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
  - site: takamatsu
    url: http://mirakc-takamatsu:40772
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		reg := cfg.Registry()
		if len(reg) != 2 {
			t.Fatalf("Registry() len = %d, want 2", len(reg))
		}
		if reg[0].Site != "tokyo" || reg[1].Site != "takamatsu" {
			t.Errorf("Registry() = %+v", reg)
		}
	})

	t.Run("invalid site names are all enumerated together", func(t *testing.T) {
		longName := strings.Repeat("a", 65)
		path := writeConfig(t, mirakcsBase+`
mirakcs:
  - site: "Tokyo"
    url: http://a:40772
  - site: "to.kyo"
    url: http://b:40772
  - site: "`+longName+`"
    url: http://c:40772
  - site: "catalog"
    url: http://d:40772
  - site: "thumbnails"
    url: http://e:40772
  - site: "osaka"
    url: http://f:40772
  - site: "osaka"
    url: http://g:40772
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		msg := err.Error()
		for _, want := range []string{
			"Tokyo", "to.kyo", longName, "catalog", "thumbnails", "duplicate site",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("error missing mention of %q; full error:\n%v", want, msg)
			}
		}
		// 全件列挙されていること（7 要素のうち 6 件が問題を持つ。osaka の 2 件目は
		// 重複だけが問題）。"  - " ブレット行の数で数える。
		lines := strings.Split(msg, "\n  - ")
		if len(lines) < 7 {
			t.Errorf("expected at least 7 enumerated violations, got %d:\n%s", len(lines), msg)
		}
	})

	t.Run("valid site names accept lowercase alnum with - and _", func(t *testing.T) {
		path := writeConfig(t, mirakcsBase+`
mirakcs:
  - site: "tokyo-1_a"
    url: http://a:40772
`)
		_, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// site 名の上限は 53 文字（River のキュー名上限 64 から、site 修飾される論理
	// キューのうち最長の prefix `reconciler_`（11 文字）を引いた値。
	// MirakcSiteNameMaxLen のコメント参照）。レジストリに載っているだけで
	// `--sites` に束縛されていない site 名も、ロード時のこの検査の対象になる。
	t.Run("site name of exactly 53 characters (MirakcSiteNameMaxLen) is accepted", func(t *testing.T) {
		name53 := strings.Repeat("a", 53)
		path := writeConfig(t, mirakcsBase+`
mirakcs:
  - site: "`+name53+`"
    url: http://a:40772
`)
		_, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error for a 53-char site name: %v", err)
		}
	})

	t.Run("site name of 54 characters (one over MirakcSiteNameMaxLen) is rejected at load time", func(t *testing.T) {
		name54 := strings.Repeat("a", 54)
		path := writeConfig(t, mirakcsBase+`
mirakcs:
  - site: "`+name54+`"
    url: http://a:40772
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for a 54-char site name, got nil")
		}
		if !strings.Contains(err.Error(), name54) {
			t.Errorf("error = %v, want mention of the offending site name", err)
		}
	})

	t.Run("missing url in a mirakcs entry is an error", func(t *testing.T) {
		path := writeConfig(t, mirakcsBase+`
mirakcs:
  - site: tokyo
    url: ""
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "url") {
			t.Errorf("error = %v, want mention of url", err)
		}
	})

	// mirakc:（単数）の時代は site 省略が "default" にフォールバックしたが、
	// mirakcs: の要素にその既定値補完は無い（defaults() は Config 全体に 1 回
	// しか走らず、スライス要素は補完しない）。省略すると空文字列のまま
	// validateSiteName の構文制約に落ちる。
	t.Run("missing site in a mirakcs entry is an error, not a default fallback", func(t *testing.T) {
		path := writeConfig(t, mirakcsBase+`
mirakcs:
  - url: http://mirakc.local:40772
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error, got nil (site should not silently default)")
		}
		if !strings.Contains(err.Error(), "must match") {
			t.Errorf("error = %v, want mention of the site name syntax constraint", err)
		}
	})

	// url は非空でも「HTTP クライアントに渡せる形」でなければならない
	// （isAbsoluteURL）。scheme が無いと mirakc.NewClient は相対パスとして
	// 解釈して接続先を失い、host が無いとどこにも繋がらない。空文字は上の
	// サブテストが別の分岐（"url is required"）で覆っている。
	t.Run("url without a scheme or host is an error", func(t *testing.T) {
		for _, bad := range []string{"mirakc.local:40772", "/api/tuners", "http://", "::not a url"} {
			t.Run(bad, func(t *testing.T) {
				path := writeConfig(t, mirakcsBase+`
mirakcs:
  - site: tokyo
    url: "`+bad+`"
`)
				_, err := Load(path)
				if err == nil {
					t.Fatalf("expected error for url %q, got nil", bad)
				}
				if !strings.Contains(err.Error(), "is not a valid URL") {
					t.Errorf("error = %v, want mention of an invalid URL", err)
				}
			})
		}
	})

	// 逆方向。正しい URL がこの検査で落ちないこと（isAbsoluteURL を
	// 「常に false」に壊すとここが落ちる）。
	t.Run("absolute http and https urls pass", func(t *testing.T) {
		for _, ok := range []string{"http://mirakc.local:40772", "https://mirakc.example.com", "http://10.0.0.1:40772/"} {
			t.Run(ok, func(t *testing.T) {
				path := writeConfig(t, mirakcsBase+`
mirakcs:
  - site: tokyo
    url: "`+ok+`"
`)
				if _, err := Load(path); err != nil {
					t.Errorf("Load with url %q: unexpected error: %v", ok, err)
				}
			})
		}
	})
}

// log.level / log.format は既定値以外を書ける唯一の列挙で、かつ defaults() が
// 埋めるので「未設定」は起こらない。validator の oneof タグを LogConfig.validate
// に置き換えたので、両方向（不正値は落ちる / 既知の値は通る）を固定する。
func TestLoad_LogEnums(t *testing.T) {
	t.Run("unknown level is an error", func(t *testing.T) {
		path := writeConfig(t, minimalConfig+`
log:
  level: verbose
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for log.level=verbose, got nil")
		}
		if !strings.Contains(err.Error(), "log.level") {
			t.Errorf("error = %v, want mention of log.level", err)
		}
	})

	// 両方が不正なら両方を報告する（規約 4）。片方で return する実装だと落ちる。
	t.Run("both bad values are reported together", func(t *testing.T) {
		path := writeConfig(t, minimalConfig+`
log:
  level: verbose
  format: logfmt
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		for _, want := range []string{"log.level", "log.format"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %s", err, want)
			}
		}
	})

	t.Run("unknown format is an error", func(t *testing.T) {
		path := writeConfig(t, minimalConfig+`
log:
  format: logfmt
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for log.format=logfmt, got nil")
		}
		if !strings.Contains(err.Error(), "log.format") {
			t.Errorf("error = %v, want mention of log.format", err)
		}
	})

	// **期待値はリテラルで書く。** logLevels / logFormats を実装から読んで
	// 回すと、値を 1 つ落としても緑のまま（実測: "warn" を消して通った）。
	t.Run("every declared value is accepted", func(t *testing.T) {
		for _, level := range []string{"debug", "info", "warn", "error"} {
			for _, format := range []string{"json", "text"} {
				path := writeConfig(t, minimalConfig+`
log:
  level: `+level+`
  format: `+format+`
`)
				if _, err := Load(path); err != nil {
					t.Errorf("Load with level=%s format=%s: unexpected error: %v", level, format, err)
				}
			}
		}
	})
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
mirakcs:
  - site: default
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
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for negative db.max_conns, got nil")
	}
}

// ingest.stall_timeout を明示的に 0 / 負にすると起動時エラーになることを確認する。
// worker 側は既定値へのフォールバックを持たない（IngestConfig.validate 参照）ので、
// ここで弾かないと 0 は StallReader を即発火させ、負値はさらに壊れる。
func TestLoad_IngestStallTimeoutNotPositive(t *testing.T) {
	for _, v := range []string{"0s", "-1s"} {
		path := writeConfig(t, fmt.Sprintf(`
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
ingest:
  stall_timeout: %s
`, v))
		if _, err := Load(path); err == nil {
			t.Errorf("ingest.stall_timeout: %s: expected error, got nil", v)
		}
	}
}

// epg.retention_grace を明示的に 0 / 負にすると起動時エラーになることを確認する。
// 負値は mark.Add(-grace) が mark より未来を指してしまい、まだ放送中の番組まで
// 刈り取られる（EpgConfig.validate 参照）。
func TestLoad_EpgRetentionGraceNotPositive(t *testing.T) {
	for _, v := range []string{"0s", "-1h"} {
		path := writeConfig(t, fmt.Sprintf(`
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
epg:
  retention_grace: %s
`, v))
		if _, err := Load(path); err == nil {
			t.Errorf("epg.retention_grace: %s: expected error, got nil", v)
		}
	}
}

// ruler.retract_grace を明示的に 0 にすると、未設定時の既定 1h にフォールバック
// せず「無効」として保持されることを確認する（RulerConfig.RetractGrace のコメント
// が定める、未設定と明示的 0 の区別）。
func TestLoad_RulerRetractGraceExplicitZeroDisables(t *testing.T) {
	path := writeConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
ruler:
  retract_grace: 0s
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Ruler.RetractGrace == nil {
		t.Fatal("ruler.retract_grace = nil, want a non-nil pointer to 0 (explicit disable)")
	}
	if *cfg.Ruler.RetractGrace != 0 {
		t.Errorf("ruler.retract_grace = %v, want 0", *cfg.Ruler.RetractGrace)
	}
}

// ruler.retract_grace に負値を与えると起動時エラーになることを確認する。
func TestLoad_RulerRetractGraceNegative(t *testing.T) {
	path := writeConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
ruler:
  retract_grace: -1h
`)
	if _, err := Load(path); err == nil {
		t.Error("ruler.retract_grace: -1h: expected error, got nil")
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
mirakcs:
  - site: default
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
mirakcs:
  - site: default
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

// intPtr はテスト用に *int リテラルを組み立てる。
func intPtr(v int) *int { return &v }

// durPtr はテスト用に *time.Duration リテラルを組み立てる。
func durPtr(v time.Duration) *time.Duration { return &v }

// allFieldsOverriddenConfig は Config の yaml タグをほぼ全部非既定値で上書きした
// 設定。
//
// encode.profiles / live.profiles は 2 要素にし、crf と qp（互いに排他）を
// 1 要素ずつに分けて両方の yaml タグを踏む。hwaccel ブロックの中身
// （kind/device/output_format、internal/ffargs のタグで config.go の 87 個には
// 含まれない）は TestLoad_EncodeProfileHWAccel / TestLoad_LiveHWAccel が別途
// 固定しているので、ここではブロックが非 nil で通ることだけを確認する。
const allFieldsOverriddenConfig = `
server:
  listen: ":8080"
  allowed_hosts: [example.com, rokuban.local]
  trust_forwarded_host: true
db:
  host: db.example.com
  port: 5433
  user: admin
  password: hunter2
  database: rokuban_prod
  sslmode: require
  max_conns: 20
  api_statement_timeout: 45s
  pooler_compat: true
mirakcs:
  - site: tokyo
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
ruler:
  max_deletes_per_pass: 77
  retract_grace: 90m
reconciler:
  start_delay_grace: 5m
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
      scaler: software
      crf: 23
      preset: medium
      input_extra_args: ["-analyzeduration", "10M"]
      extra_args: ["-movflags", "+faststart"]
    - name: h265_vaapi
      container: mkv
      video_codec: hevc_vaapi
      audio_codec: aac
      height: 720
      scaler: vaapi
      qp: 28
      hwaccel:
        kind: vaapi
        device: /dev/dri/renderD128
        output_format: vaapi
live:
  enabled: true
  ffmpeg: /usr/local/bin/ffmpeg-live
  segment_dir: /tmp/hls
  max_sessions: 8
  idle_timeout: 45s
  tuner_priority: 5
  hwaccel:
    kind: vaapi
    device: /dev/dri/renderD128
    output_format: vaapi
  input_extra_args: ["-re"]
  profiles:
    - name: high
      video_codec: libx264
      audio_codec: aac
      height: 720
      scaler: software
      crf: 24
      preset: veryfast
      segment_seconds: 4
      playlist_size: 8
      extra_args: ["-movflags", "+faststart"]
    - name: low
      video_codec: libx264
      audio_codec: aac
      height: 360
      qp: 30
      segment_seconds: 3
      playlist_size: 7
webhook:
  url: https://hooks.example.com/rokuban
  secret: s3cret
  timeout: 10s
  events:
    - recording.finished
    - recording.failed
cleanup:
  trash_retention: 240h
  orphan_mtime_grace: 100h
  orphan_age: 300h
  max_deletes_per_pass: 55
  missing_asset_age: 12h
log:
  level: debug
  format: text
`

// TestLoad_AllFieldsOverridden は Config の yaml タグ（config.go に 87 個）を
// 全部上書きした設定を読み、セクションごとに実際の値と期待値をテーブルで
// 突き合わせる。
//
// セクション単位（struct 丸ごと）の比較にしているのは、そのセクション内の
// どの yaml タグの上書きを 1 つ外しても（既定値に戻って期待値と食い違うので）
// 対応する行が落ちるため --- フィールドごとに行を分けなくても検出力は
// 変わらない。
func TestLoad_AllFieldsOverridden(t *testing.T) {
	path := writeConfig(t, allFieldsOverriddenConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{
			"server",
			cfg.Server,
			ServerConfig{
				Listen:             ":8080",
				AllowedHosts:       []string{"example.com", "rokuban.local"},
				TrustForwardedHost: true,
			},
		},
		{
			"db",
			cfg.DB,
			DBConfig{
				Host: "db.example.com", Port: 5433, User: "admin", Password: "hunter2",
				Database: "rokuban_prod", SSLMode: "require",
				MaxConns: 20, APIStatementTimeout: 45 * time.Second, PoolerCompat: true,
			},
		},
		{
			"mirakcs",
			cfg.Mirakcs,
			[]MirakcSite{{Site: "tokyo", URL: "http://10.0.0.1:40772"}},
		},
		{
			"storage",
			cfg.Storage,
			StorageConfig{MediaDir: "/data/media", ScratchDir: "/data/scratch", AccelLocation: "/_media/"},
		},
		{
			"ingest",
			cfg.Ingest,
			IngestConfig{Concurrency: 4, StallTimeout: 2 * time.Minute},
		},
		{
			"epg",
			cfg.Epg,
			EpgConfig{SyncInterval: 30 * time.Minute, RetentionGrace: 48 * time.Hour},
		},
		{
			"ruler",
			cfg.Ruler,
			RulerConfig{MaxDeletesPerPass: 77, RetractGrace: durPtr(90 * time.Minute)},
		},
		{
			"reconciler",
			cfg.Reconciler,
			ReconcilerConfig{StartDelayGrace: 5 * time.Minute},
		},
		{
			"worker",
			cfg.Worker,
			WorkerConfig{PeriodicJobs: false, Queues: []string{"ruler", "epg"}},
		},
		{
			"encode",
			cfg.Encode,
			EncodeConfig{
				FFmpeg: "/usr/local/bin/ffmpeg", FFprobe: "/usr/local/bin/ffprobe",
				Concurrency: 3, ThumbnailConcurrency: 2,
				Profiles: []EncodeProfile{
					{
						Name: "h264", Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
						Height: 1080, Scaler: ffargs.ScalerSoftware, CRF: intPtr(23), Preset: "medium",
						InputExtraArgs: []string{"-analyzeduration", "10M"},
						ExtraArgs:      []string{"-movflags", "+faststart"},
					},
					{
						Name: "h265_vaapi", Container: "mkv", VideoCodec: "hevc_vaapi", AudioCodec: "aac",
						Height: 720, Scaler: ffargs.ScalerVAAPI, QP: intPtr(28),
						HWAccel: &ffargs.HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128", OutputFormat: "vaapi"},
					},
				},
			},
		},
		{
			"live",
			cfg.Live,
			LiveConfig{
				Enabled: true, FFmpeg: "/usr/local/bin/ffmpeg-live", SegmentDir: "/tmp/hls",
				MaxSessions: 8, IdleTimeout: 45 * time.Second, TunerPriority: 5,
				HWAccel:        &ffargs.HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128", OutputFormat: "vaapi"},
				InputExtraArgs: []string{"-re"},
				Profiles: []LiveProfile{
					{
						Name: "high", VideoCodec: "libx264", AudioCodec: "aac", Height: 720,
						Scaler: ffargs.ScalerSoftware, CRF: intPtr(24), Preset: "veryfast",
						SegmentSeconds: 4, PlaylistSize: 8,
						ExtraArgs: []string{"-movflags", "+faststart"},
					},
					{
						Name: "low", VideoCodec: "libx264", AudioCodec: "aac", Height: 360,
						QP: intPtr(30), SegmentSeconds: 3, PlaylistSize: 7,
					},
				},
			},
		},
		{
			"webhook",
			cfg.Webhook,
			WebhookConfig{
				URL: "https://hooks.example.com/rokuban", Secret: "s3cret", Timeout: 10 * time.Second,
				Events: []string{"recording.finished", "recording.failed"},
			},
		},
		{
			"cleanup",
			cfg.Cleanup,
			CleanupConfig{
				TrashRetention: 240 * time.Hour, OrphanMTimeGrace: 100 * time.Hour, OrphanAge: 300 * time.Hour,
				MaxDeletesPerPass: 55, MissingAssetAge: 12 * time.Hour,
			},
		},
		{
			"log",
			cfg.Log,
			LogConfig{Level: "debug", Format: "text"},
		},
	}

	for _, c := range cases {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %+v, want %+v", c.name, c.got, c.want)
		}
	}
}

func TestLoad_EncodeProfileValidation(t *testing.T) {
	base := `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
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
mirakcs:
  - site: default
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

// buildEncodeHWConfig は encode.profiles[0] に extra を追記した完全な config を返す
// （extra は 6 スペースインデント、末尾改行込みの YAML 行）。
func buildEncodeHWConfig(extra string) string {
	return `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
encode:
  profiles:
    - name: h264
      container: mp4
      video_codec: libx264
      audio_codec: aac
` + extra
}

// buildLiveHWConfig は live.profiles[0] に extra を追記した完全な config を返す
// （extra は 6 スペースインデント、末尾改行込みの YAML 行）。
func buildLiveHWConfig(extra string) string {
	return `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
live:
  enabled: true
  segment_dir: /dev/shm/rokuban-live
  profiles:
    - name: h264
      video_codec: libx264
      audio_codec: aac
` + extra
}

// TestLoad_HWEncodeFields_EncodeAndLive は encode.profiles と live.profiles の
// 両方で同じ ffargs 検査（crf/qp 排他・scaler の許容集合・scaler without
// height）が働くことを、同じテーブルで両方に対して回して確認する。片側だけ
// 検査を実装し忘れる事故をこのテーブルが検出する（issue #321 決定コメント §6
// 「encode と live を同じテーブルで回して片側の書き忘れを不可能にする」）。
// 壊し方は各サブテストのコメント参照。
//
// **hwaccel はここに含めない。** encode.profiles[].hwaccel はプロファイル毎
// だが live.hwaccel は live セクション直下（issue #321 決定コメント §1: ライブは
// 入力 1 本なのでプロファイル毎には表現しない）で YAML 上の置き場所が違うため、
// 「profile 本体に追記する」という同じテーブルの形が使えない。hwaccel の検査は
// TestLoad_EncodeProfileHWAccel / TestLoad_LiveHWAccel で別に固定する。
func TestLoad_HWEncodeFields_EncodeAndLive(t *testing.T) {
	cases := []struct {
		name    string
		extra   string
		wantErr bool
		wantMsg string
	}{
		{
			// 壊し方: crf を勝たせて nil を返す（ValidateVideo の crf!=nil&&qp!=nil を消す）。
			name:    "crf and qp together is a startup error",
			extra:   "      crf: 23\n      qp: 24\n",
			wantErr: true,
			wantMsg: "qp",
		},
		{
			// 壊し方: 未知値を software に落とす（Scaler.allowed の判定を消す）。
			name:    "unknown scaler value names the allowed set",
			extra:   "      height: 720\n      scaler: qsv\n",
			wantErr: true,
			wantMsg: "vaapi",
		},
		{
			// 壊し方: height<=0 の検査を消す。
			name:    "scaler without height is a startup error",
			extra:   "      scaler: vaapi\n",
			wantErr: true,
			wantMsg: "height",
		},
		{
			// 壊し方: 明示的な software も height 0 を許すよう分岐を変える。
			name:    "explicit software scaler without height is still an error",
			extra:   "      scaler: software\n",
			wantErr: true,
			wantMsg: "height",
		},
		{
			name:    "negative crf is an error",
			extra:   "      crf: -1\n",
			wantErr: true,
			wantMsg: "crf",
		},
		{
			name:    "negative qp is an error",
			extra:   "      qp: -1\n",
			wantErr: true,
			wantMsg: "qp",
		},
	}

	builders := []struct {
		name  string
		build func(extra string) string
	}{
		{"encode", buildEncodeHWConfig},
		{"live", buildLiveHWConfig},
	}

	for _, b := range builders {
		for _, c := range cases {
			t.Run(b.name+"/"+c.name, func(t *testing.T) {
				path := writeConfig(t, b.build(c.extra))
				_, err := Load(path)
				if c.wantErr && err == nil {
					t.Fatal("expected error, got nil")
				}
				if !c.wantErr && err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if c.wantErr && c.wantMsg != "" && !strings.Contains(err.Error(), c.wantMsg) {
					t.Errorf("error = %v, want mention of %q", err, c.wantMsg)
				}
			})
		}
	}
}

// TestLoad_EncodeProfileHWAccel は encode.profiles[].hwaccel（プロファイル毎の
// ブロック）の検査を固定する。壊し方: HWAccel.Validate 呼び出しを消す /
// kind の空チェックを消す。
func TestLoad_EncodeProfileHWAccel(t *testing.T) {
	cases := []struct {
		name    string
		extra   string
		wantErr bool
		wantMsg string
	}{
		{
			name:    "hwaccel: {} (kind missing) is a startup error",
			extra:   "      hwaccel: {}\n",
			wantErr: true,
			wantMsg: "kind",
		},
		{
			name:    "hwaccel device without kind is still an error",
			extra:   "      hwaccel:\n        device: /dev/dri/renderD128\n",
			wantErr: true,
			wantMsg: "kind",
		},
		{
			// ブロック経由でフラグを密輸させない。
			name:    "hwaccel kind smuggling a flag is an error",
			extra:   "      hwaccel:\n        kind: \"-y\"\n",
			wantErr: true,
		},
		{
			name:    "hwaccel with kind only is accepted",
			extra:   "      height: 720\n      scaler: vaapi\n      hwaccel:\n        kind: vaapi\n",
			wantErr: false,
		},
		{
			name:    "hwaccel with kind, device, output_format is accepted",
			extra:   "      height: 720\n      scaler: vaapi\n      hwaccel:\n        kind: vaapi\n        device: /dev/dri/renderD128\n        output_format: vaapi\n",
			wantErr: false,
		},
		{
			// bare `hwaccel:` (no value) はブロックを「書いていない」扱いになる
			// （*ffargs.HWAccel が nil のまま。goccy/go-yaml が null をポインタの
			// nil にデコードすることに乗った挙動）。
			name:    "bare hwaccel key with no value is not written",
			extra:   "      hwaccel:\n",
			wantErr: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeConfig(t, buildEncodeHWConfig(c.extra))
			_, err := Load(path)
			if c.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantErr && c.wantMsg != "" && !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error = %v, want mention of %q", err, c.wantMsg)
			}
		})
	}
}

// TestLoad_LiveHWAccel は live.hwaccel（live セクション直下、プロファイル毎では
// ない）の検査を固定する。壊し方は TestLoad_EncodeProfileHWAccel と同じ。
func TestLoad_LiveHWAccel(t *testing.T) {
	baseWithProfile := `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
live:
  enabled: true
  segment_dir: /dev/shm/rokuban-live
`
	profile := `  profiles:
    - name: h264
      video_codec: libx264
      audio_codec: aac
`

	cases := []struct {
		name    string
		extra   string
		wantErr bool
		wantMsg string
	}{
		{
			name:    "hwaccel: {} (kind missing) is a startup error",
			extra:   "  hwaccel: {}\n",
			wantErr: true,
			wantMsg: "kind",
		},
		{
			name:    "hwaccel device without kind is still an error",
			extra:   "  hwaccel:\n    device: /dev/dri/renderD128\n",
			wantErr: true,
			wantMsg: "kind",
		},
		{
			name:    "hwaccel kind smuggling a flag is an error",
			extra:   "  hwaccel:\n    kind: \"-y\"\n",
			wantErr: true,
		},
		{
			name:    "hwaccel with kind, device, output_format is accepted",
			extra:   "  hwaccel:\n    kind: vaapi\n    device: /dev/dri/renderD128\n    output_format: vaapi\n",
			wantErr: false,
		},
		{
			name:    "bare hwaccel key with no value is not written",
			extra:   "  hwaccel:\n",
			wantErr: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeConfig(t, baseWithProfile+c.extra+profile)
			_, err := Load(path)
			if c.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantErr && c.wantMsg != "" && !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error = %v, want mention of %q", err, c.wantMsg)
			}
		})
	}
}

// TestLoad_ArgumentAllowlist_AllLists は 4 つの追加引数リストが実際の YAML
// 位置から allowlist 検査に到達することを固定する。特に live の入力側は
// profile 内ではなく live 直下にある。
func TestLoad_ArgumentAllowlist_AllLists(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "encode profile extra_args",
			yaml: buildEncodeHWConfig("      extra_args: [\"-i\", \"in\"]\n"),
			want: "-i",
		},
		{
			name: "encode profile input_extra_args",
			yaml: buildEncodeHWConfig("      input_extra_args: [\"-y\"]\n"),
			want: "-y",
		},
		{
			name: "live profile extra_args",
			yaml: buildLiveHWConfig("      extra_args: [\"-i\", \"in\"]\n"),
			want: "-i",
		},
		{
			name: "live input_extra_args",
			yaml: strings.Replace(buildLiveHWConfig(""), "  profiles:\n", "  input_extra_args: [\"-y\"]\n  profiles:\n", 1),
			want: "-y",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, c.yaml))
			if err == nil {
				t.Fatalf("expected app-owned flag %q to be rejected", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want mention of %q", err, c.want)
			}
		})
	}
}

// TestLoad_ExtraArgsRejectOutputInjection は値を取らないフラグの直後に 2 本目の
// 出力パスを置く経路を VOD / live の両方で拒否する。
func TestLoad_ExtraArgsRejectOutputInjection(t *testing.T) {
	for _, c := range []struct {
		name string
		yaml string
	}{
		{"encode", buildEncodeHWConfig("      extra_args: [\"-an\", \"/tmp/evil.mp4\"]\n")},
		{"live profile", buildLiveHWConfig("      extra_args: [\"-shortest\", \"/tmp/evil.mp4\"]\n")},
		{
			"live input",
			strings.Replace(buildLiveHWConfig(""), "  profiles:\n", "  input_extra_args: [\"-vn\", \"/tmp/evil.mp4\"]\n  profiles:\n", 1),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, c.yaml)); err == nil {
				t.Fatal("expected second output path to be rejected")
			}
		})
	}
}

// TestLoad_BarePositionalArgs_EncodeAndLive は裸の位置引数（2 個目の出力
// ファイルパスの密輸経路）を拒否し、正当な形（許可済みの値付きフラグ・
// ブールフラグ・ストリームセレクタ値）は通すことを固定する。
func TestLoad_BarePositionalArgs_EncodeAndLive(t *testing.T) {
	cases := []struct {
		name    string
		extra   string
		wantErr bool
	}{
		{
			name:    "single bare positional argument",
			extra:   "      extra_args: [\"/tmp/evil.mp4\"]\n",
			wantErr: true,
		},
		{
			name:    "bare positional argument trailing a flag value",
			extra:   "      extra_args: [\"-movflags\", \"+faststart\", \"/tmp/evil.mp4\"]\n",
			wantErr: true,
		},
		{
			name:    "flag with a value is allowed",
			extra:   "      extra_args: [\"-movflags\", \"+faststart\"]\n",
			wantErr: false,
		},
		{
			name:    "bare boolean flag is allowed",
			extra:   "      extra_args: [\"-an\"]\n",
			wantErr: false,
		},
		{
			name:    "stream selector value is allowed",
			extra:   "      extra_args: [\"-map\", \"0:a:1\"]\n",
			wantErr: false,
		},
	}

	for _, b := range []struct {
		name  string
		build func(extra string) string
	}{
		{"encode", buildEncodeHWConfig},
		{"live", buildLiveHWConfig},
	} {
		for _, c := range cases {
			t.Run(b.name+"/"+c.name, func(t *testing.T) {
				path := writeConfig(t, b.build(c.extra))
				_, err := Load(path)
				if c.wantErr && err == nil {
					t.Fatal("expected error, got nil")
				}
				if !c.wantErr && err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			})
		}
	}
}

// TestLoad_RejectsCmdKey_EncodeAndLive は encode.profiles / live.profiles の
// どちらでも `cmd:` が未知キーとして落ちることを固定する（自由形式の cmd
// 文字列は今日すでに強制的に拒否されている --- loadFromString が
// yaml.Strict() を通しており、strict は配列要素の中まで再帰するため。
// 実測: 現 HEAD の loader に食わせると
// `parsing config: [16:7] unknown field "cmd"` のように行・桁付きで落ちる）。
// 壊し方: loadFromString から yaml.Strict() を外す（両方が素通りする）。
func TestLoad_RejectsCmdKey_EncodeAndLive(t *testing.T) {
	for _, b := range []struct {
		name  string
		build func(extra string) string
	}{
		{"encode", buildEncodeHWConfig},
		{"live", buildLiveHWConfig},
	} {
		t.Run(b.name, func(t *testing.T) {
			path := writeConfig(t, b.build("      cmd: \"ffmpeg -i {{input}} {{output}}\"\n"))
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error for unknown field cmd")
			}
			if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "cmd") {
				t.Errorf("error = %v, want mention of unknown field \"cmd\"", err)
			}
		})
	}
}

// TestLoad_VAAPIExample_LoadsWithoutDeviceCheck は config.example.yml の VAAPI
// プロファイル相当を、`/dev/dri` の無い環境（この CI 含む）で Load してエラーに
// ならないことを主張する。壊し方: hwaccel.device の os.Stat 検査を足す（罠が
// 禁じている実装を入れた瞬間に落ちる --- device の存在は起動時に検査しない。
// issue #321 決定コメント §2-8）。
func TestLoad_VAAPIExample_LoadsWithoutDeviceCheck(t *testing.T) {
	if _, err := os.Stat("/dev/dri"); err == nil {
		t.Skip("this environment has /dev/dri; the point of this test is to run where it does not exist")
	}

	path := writeConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
encode:
  profiles:
    - name: h264_vaapi
      container: mp4
      video_codec: h264_vaapi
      audio_codec: aac
      height: 720
      scaler: vaapi
      qp: 24
      hwaccel:
        kind: vaapi
        device: /dev/dri/renderD128
        output_format: vaapi
      input_extra_args: []
live:
  enabled: true
  segment_dir: /dev/shm/rokuban-live
  hwaccel:
    kind: vaapi
    device: /dev/dri/renderD128
    output_format: vaapi
  input_extra_args: []
  profiles:
    - name: h264_vaapi
      video_codec: h264_vaapi
      audio_codec: aac
      height: 720
      scaler: vaapi
      qp: 26
      segment_seconds: 2
      playlist_size: 6
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load with a VAAPI profile referencing a nonexistent device must not fail: %v", err)
	}
}

func TestLoad_Live(t *testing.T) {
	base := `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: default
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`

	t.Run("disabled by default", func(t *testing.T) {
		path := writeConfig(t, base)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Live.Enabled {
			t.Error("live.enabled should default to false")
		}
		// enabled=false のときは profiles が空でも segment_dir が無くても
		// 検証で落ちない（公式イメージで streamer を起動する構成を壊さない）。
		if len(cfg.Live.Profiles) != 0 {
			t.Errorf("live.profiles = %v, want empty", cfg.Live.Profiles)
		}
	})

	t.Run("enabled applies defaults", func(t *testing.T) {
		path := writeConfig(t, base+`
live:
  enabled: true
  segment_dir: /dev/shm/rokuban-live
  profiles:
    - name: h264
      video_codec: libx264
      audio_codec: aac
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Live.FFmpeg != "ffmpeg" {
			t.Errorf("live.ffmpeg = %q, want %q", cfg.Live.FFmpeg, "ffmpeg")
		}
		if cfg.Live.MaxSessions != 4 {
			t.Errorf("live.max_sessions = %d, want 4", cfg.Live.MaxSessions)
		}
		if cfg.Live.IdleTimeout != 30*time.Second {
			t.Errorf("live.idle_timeout = %v, want 30s", cfg.Live.IdleTimeout)
		}
		if cfg.Live.TunerPriority != 1 {
			t.Errorf("live.tuner_priority = %d, want 1", cfg.Live.TunerPriority)
		}
		if len(cfg.Live.Profiles) != 1 {
			t.Fatalf("live.profiles len = %d, want 1", len(cfg.Live.Profiles))
		}
		p := cfg.Live.Profiles[0]
		if p.SegmentSeconds != 2 {
			t.Errorf("profiles[0].segment_seconds = %d, want 2", p.SegmentSeconds)
		}
		if p.PlaylistSize != 6 {
			t.Errorf("profiles[0].playlist_size = %d, want 6", p.PlaylistSize)
		}
	})

	t.Run("enabled requires profiles", func(t *testing.T) {
		path := writeConfig(t, base+`
live:
  enabled: true
  segment_dir: /dev/shm/rokuban-live
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error: live.enabled without profiles")
		}
		if !strings.Contains(err.Error(), "profiles") {
			t.Errorf("error = %v, want mention of profiles", err)
		}
	})

	t.Run("enabled requires segment_dir", func(t *testing.T) {
		path := writeConfig(t, base+`
live:
  enabled: true
  profiles:
    - name: h264
      video_codec: libx264
      audio_codec: aac
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error: live.enabled without segment_dir")
		}
		if !strings.Contains(err.Error(), "segment_dir") {
			t.Errorf("error = %v, want mention of segment_dir", err)
		}
	})

	t.Run("rejects unsafe profile name", func(t *testing.T) {
		path := writeConfig(t, base+`
live:
  enabled: true
  segment_dir: /dev/shm/rokuban-live
  profiles:
    - name: "../etc"
      video_codec: libx264
      audio_codec: aac
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error: unsafe profile name")
		}
	})

	t.Run("rejects duplicate profile name", func(t *testing.T) {
		path := writeConfig(t, base+`
live:
  enabled: true
  segment_dir: /dev/shm/rokuban-live
  profiles:
    - name: h264
      video_codec: libx264
      audio_codec: aac
    - name: h264
      video_codec: libx265
      audio_codec: aac
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error: duplicate profile name")
		}
		if !strings.Contains(err.Error(), "duplicate name") {
			t.Errorf("error = %v, want mention of duplicate name", err)
		}
	})

	t.Run("rejects missing video_codec", func(t *testing.T) {
		path := writeConfig(t, base+`
live:
  enabled: true
  segment_dir: /dev/shm/rokuban-live
  profiles:
    - name: h264
      audio_codec: aac
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error: missing video_codec")
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

// TestEncodeConfig_ProfileNames_EmptyIsNonNil は「プロファイルが 1 つも無い設定でも
// ProfileNames() は non-nil を返す」という契約を固定する。`slices.Equal` は nil と
// 空スライスを等しいと見るので、上のテストではこの違いを捕まえられない。
//
// 依存している側（nil にすると壊れる側）:
//   - internal/worker: encode の定期 reconcile がこの結果を SQL に渡す。
//     Postgres の `x = ANY(NULL::text[])` は false ではなく **NULL** なので、
//     nil を渡すと候補（EXISTS が偽になる）も検出（NOT NULL も NULL）も同時に
//     落ち、バックストップが**無症状で**死ぬ（空スライスなら「候補ゼロ + 未設定
//     プロファイルの警告」が出て気付ける）。実挙動は
//     TestEncodeReconcileWorker_EmptyProfileConfigIsVisibleNotSilent が見る
//   - internal/api/handler.go: nil を「検証スキップ」の合図に使っている
//     （そこのコメント参照）ので、nil を返すと未知プロファイル名の 400 判定が
//     まるごと無効になる
func TestEncodeConfig_ProfileNames_EmptyIsNonNil(t *testing.T) {
	if got := (EncodeConfig{}).ProfileNames(); got == nil {
		t.Error("ProfileNames() on an empty config returned nil; callers rely on non-nil (see the doc comment on this test)")
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

func TestLiveConfig_ValidateTools(t *testing.T) {
	cfg := LiveConfig{FFmpeg: "rokuban-no-such-ffmpeg-binary"}
	if err := cfg.ValidateTools(); err == nil {
		t.Fatal("expected error for missing ffmpeg")
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

// 機微情報を `${VAR}` で受けるとき、**どの囲み方が値の中身に耐えるか**。
//
// 展開（envsubst）は YAML パースの前に生テキスト置換で走るので、展開後の文字列が
// YAML の構文として解釈されうる。パスワードのような「中身を選べない値」を config に
// 通す構成（k8s の ConfigMap + Secret。deploy/k8s/base/config.yml）は、この表の
// どこに乗るかで壊れ方が決まる。
//
// 実測の要点:
//   - 無クォート: `*` `{` で始まる値・`: ` を含む値でパースエラー
//   - 単一引用符: `'` を含む値でパースエラー
//   - 二重引用符: `"` と `\` を含む値でパースエラー
//   - 折り畳みブロックスカラー（`>-`）: 記号はすべて通る。前後の空白は落ち、
//     値の中の改行は構造を壊す（または別のキーとして読まれる）
func TestSecretExpansionQuotingForms(t *testing.T) {
	const tmpl = `
db:
  host: localhost
  user: rokuban
  database: rokuban
  password: %s
mirakcs:
  - site: default
    url: http://mirakc:40772
storage:
  media_dir: /mnt/media
`
	forms := map[string]string{
		"bare":   "${POSTGRES_PASSWORD}",
		"single": "'${POSTGRES_PASSWORD}'",
		"double": `"${POSTGRES_PASSWORD}"`,
		"folded": ">-\n    ${POSTGRES_PASSWORD}",
	}
	// 各形式で「通らない」パスワード。ここに挙げた値はパースエラーになるか、
	// 別の値として読まれる（どちらも「運べない」）。
	broken := map[string][]string{
		"bare":   {"*abc", "{abc}", "a: b"},
		"single": {"pa'ss"},
		"double": {`pa"ss`, `pa\ss`},
		"folded": {},
	}
	// どの形式でも通ってほしい値。
	safe := []string{"s3cret", "p@ss#word", "pa=ss", "12345678"}

	for form, placeholder := range forms {
		t.Run(form, func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf(tmpl, placeholder))

			for _, pw := range safe {
				t.Setenv("POSTGRES_PASSWORD", pw)
				cfg, err := Load(path)
				if err != nil {
					t.Errorf("Load with %q = %v, want it to load", pw, err)
					continue
				}
				if cfg.DB.Password != pw {
					t.Errorf("db.password = %q, want %q", cfg.DB.Password, pw)
				}
			}

			for _, pw := range broken[form] {
				t.Setenv("POSTGRES_PASSWORD", pw)
				cfg, err := Load(path)
				if err == nil && cfg.DB.Password == pw {
					t.Errorf("Load with %q succeeded; this form is documented as unable to carry it", pw)
				}
			}

			if form != "folded" {
				return
			}
			// 折り畳みブロックスカラーだけの性質（deploy/k8s/base/config.yml が
			// この形を選んだ根拠）。記号は通り、空白は落ち、改行は運べない。
			for _, pw := range []string{"*abc", "{abc}", "a: b", "pa'ss", `pa"ss`, `pa\ss`} {
				t.Setenv("POSTGRES_PASSWORD", pw)
				cfg, err := Load(path)
				if err != nil || cfg.DB.Password != pw {
					t.Errorf("folded form failed to carry %q: err=%v", pw, err)
				}
			}
			t.Setenv("POSTGRES_PASSWORD", " padded ")
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load with a padded password: %v", err)
			}
			if cfg.DB.Password != "padded" {
				t.Errorf("db.password = %q, want %q (前後の空白は落ちる)", cfg.DB.Password, "padded")
			}
		})
	}
}

// live の値域は enabled に関わらず検査する。`enabled: false` のまま値だけ先に
// 書く構成（config.compose.yml がその形で出荷している）で書き間違えた負値が、
// ライブを有効にした日まで隠れないようにする。
func TestLoad_LiveRangesCheckedEvenWhenDisabled(t *testing.T) {
	for _, tt := range []struct{ key, value, want string }{
		{"max_sessions", "-5", "live.max_sessions"},
		{"tuner_priority", "-1", "live.tuner_priority"},
	} {
		t.Run(tt.key, func(t *testing.T) {
			path := writeConfig(t, minimalConfig+`
live:
  enabled: false
  `+tt.key+`: `+tt.value+`
`)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error for live.%s=%s with enabled:false, got nil", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %s", err, tt.want)
			}
		})
	}
}

// 空文字の log.level / log.format は「未設定」として通す。defaults() が埋めるのは
// キーが無いときだけなので、`level: ${VAR}`（`:-` 無し）の展開で空文字が入る
// 構成は実在する。ここで落とすとその構成が起動しなくなる。
func TestLoad_EmptyLogValuesAreTreatedAsUnset(t *testing.T) {
	path := writeConfig(t, minimalConfig+`
log:
  level: ""
  format: ""
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("empty log values should load, got %v", err)
	}
}
