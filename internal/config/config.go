package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/drone/envsubst"
	"github.com/go-playground/validator/v10"
	"github.com/goccy/go-yaml"
)

// Config はアプリケーション全体の設定。
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	DB      DBConfig      `yaml:"db"`
	Mirakc  MirakcConfig  `yaml:"mirakc"`
	Storage StorageConfig `yaml:"storage"`
	Ingest  IngestConfig  `yaml:"ingest"`
	Epg     EpgConfig     `yaml:"epg"`
	Worker  WorkerConfig  `yaml:"worker"`
	Encode  EncodeConfig  `yaml:"encode"`
	Log     LogConfig     `yaml:"log"`
}

// ServerConfig は HTTP サーバーの設定。
type ServerConfig struct {
	Listen       string   `yaml:"listen"`
	AllowedHosts []string `yaml:"allowed_hosts"`
}

// DBConfig は PostgreSQL 接続設定。
type DBConfig struct {
	Host     string `yaml:"host"     validate:"required"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"     validate:"required"`
	Password string `yaml:"password" validate:"required"`
	Database string `yaml:"database" validate:"required"`
	SSLMode  string `yaml:"sslmode"`
}

// DSN は libpq 形式の接続文字列を返す。
func (c DBConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		quoteDSNValue(c.Host), c.Port, quoteDSNValue(c.User),
		quoteDSNValue(c.Password), quoteDSNValue(c.Database), quoteDSNValue(c.SSLMode),
	)
}

// quoteDSNValue は libpq のキーワード/値形式の値を単一引用符で囲む。
//
// 引用しないと空値と空白入りの値が壊れる。たとえば password が空だと
// `password= dbname=x` となり、libpq は次のトークン（`dbname=x`）を
// パスワードの値として読んでしまう。結果 dbname が未指定になり、
// ユーザー名と同名のデータベースへ黙って接続する。
func quoteDSNValue(v string) string {
	escaped := strings.ReplaceAll(v, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return "'" + escaped + "'"
}

// MirakcConfig は mirakc 接続設定。
type MirakcConfig struct {
	URL string `yaml:"url" validate:"required,url"`
}

// StorageConfig はメディアファイルの保存先設定。
type StorageConfig struct {
	MediaDir   string `yaml:"media_dir"   validate:"required"`
	ScratchDir string `yaml:"scratch_dir"`

	// AccelLocation を設定すると録画ファイルの配信を X-Accel-Redirect で
	// リバースプロキシに委ねる（認可判定はアプリ、バイト転送は nginx）。
	// 値は nginx の internal location（例: /_media/）。空なら Go が直接配る。
	AccelLocation string `yaml:"accel_location"`
}

// IngestConfig は ingest ジョブの設定。
type IngestConfig struct {
	Concurrency int `yaml:"concurrency"`
}

// EpgConfig は EPG プロジェクションの設定。
type EpgConfig struct {
	// SyncInterval は mirakc から EPG を全量取得する間隔。
	SyncInterval time.Duration `yaml:"sync_interval"`

	// RetentionGrace は放送終了からこの時間が経った番組をローリングウィンドウから刈り取る。
	RetentionGrace time.Duration `yaml:"retention_grace"`
}

// WorkerConfig は worker ロールの River クライアント設定。
type WorkerConfig struct {
	// PeriodicJobs はプロセス内で定期ジョブ（epg_sync / ruler_pass / reconcile_pass /
	// record_sweep）を投入するか。
	// k8s では false にし、CronJob から `rokuban enqueue` で投入する。
	// River の PeriodicJobs はリーダーに選出されたクライアントだけが投入するため、
	// worker を KEDA で 0 にスケールすると誰も投入しなくなる（docs/data.md §2
	// 「定期実行の契機はデプロイ形態に委ねる」）。
	PeriodicJobs bool `yaml:"periodic_jobs"`

	// Queues は引くキューを絞る。空なら全部。ロールを増やさずに「ruler / reconciler だけ別 Pod」を
	// 実現するための knob（docs/overview.md「ロールは『プロセスの形』を表し、
	// 『どの仕事をするか』は表さない」）。未知のキュー名は起動時エラーになる。
	Queues []string `yaml:"queues"`
}

// EncodeConfig はエンコード設定。
type EncodeConfig struct {
	FFmpeg   string          `yaml:"ffmpeg"`
	FFprobe  string          `yaml:"ffprobe"`
	Profiles []EncodeProfile `yaml:"profiles"`
}

// EncodeProfile はエンコードプロファイルの定義。
type EncodeProfile struct {
	Name string `yaml:"name"`
}

// LogConfig はログ出力の設定。
type LogConfig struct {
	Level  string `yaml:"level"  validate:"omitempty,oneof=debug info warn error"`
	Format string `yaml:"format" validate:"omitempty,oneof=json text"`
}

func defaults() Config {
	return Config{
		Server: ServerConfig{
			Listen: ":40773",
		},
		DB: DBConfig{
			Port:    5432,
			SSLMode: "disable",
		},
		Storage: StorageConfig{
			ScratchDir: "/var/tmp/rokuban",
		},
		Ingest: IngestConfig{
			Concurrency: 2,
		},
		Epg: EpgConfig{
			SyncInterval:   10 * time.Minute,
			RetentionGrace: 24 * time.Hour,
		},
		Worker: WorkerConfig{
			PeriodicJobs: true,
		},
		Encode: EncodeConfig{
			FFmpeg:  "ffmpeg",
			FFprobe: "ffprobe",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
	}
}

var vld = validator.New(validator.WithRequiredStructEnabled())

// Load reads a config file, expands ${VAR} references using environment
// variables, and parses the result with strict mode enabled.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	return loadFromString(string(raw))
}

func loadFromString(raw string) (*Config, error) {
	expanded, err := envsubst.EvalEnv(raw)
	if err != nil {
		return nil, fmt.Errorf("expanding variables: %w", err)
	}

	// defaults() の戻り値にマージすることでデフォルト値を提供する
	cfg := defaults()
	// Strict: 未知キーで起動失敗させ、typo を早期検出する
	if err := yaml.UnmarshalWithOptions([]byte(expanded), &cfg, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := vld.Struct(&cfg); err != nil {
		var ve validator.ValidationErrors
		if errors.As(err, &ve) {
			return nil, &ValidationError{fieldErrors: ve}
		}
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// ValidationError は設定バリデーション失敗時のエラー。
type ValidationError struct {
	fieldErrors validator.ValidationErrors
}

// Error は検証エラーのメッセージを返す。
func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.fieldErrors))
	for i, fe := range e.fieldErrors {
		msgs[i] = fmt.Sprintf("%s is required", yamlFieldPath(fe))
	}
	return fmt.Sprintf("config validation failed:\n  - %s", strings.Join(msgs, "\n  - "))
}

// FieldErrors は個別のフィールドエラーを返す。
func (e *ValidationError) FieldErrors() validator.ValidationErrors {
	return e.fieldErrors
}

func yamlFieldPath(fe validator.FieldError) string {
	ns := fe.Namespace()
	parts := strings.SplitN(ns, ".", 2)
	if len(parts) < 2 {
		return strings.ToLower(ns)
	}
	return strings.ToLower(parts[1])
}
