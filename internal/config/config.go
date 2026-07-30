package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/drone/envsubst"
	"github.com/go-playground/validator/v10"
	"github.com/goccy/go-yaml"
)

// Config はアプリケーション全体の設定。
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	DB         DBConfig         `yaml:"db"`
	Mirakc     MirakcConfig     `yaml:"mirakc"`
	Storage    StorageConfig    `yaml:"storage"`
	Ingest     IngestConfig     `yaml:"ingest"`
	Epg        EpgConfig        `yaml:"epg"`
	Ruler      RulerConfig      `yaml:"ruler"`
	Reconciler ReconcilerConfig `yaml:"reconciler"`
	Worker     WorkerConfig     `yaml:"worker"`
	Encode     EncodeConfig     `yaml:"encode"`
	Webhook    WebhookConfig    `yaml:"webhook"`
	Cleanup    CleanupConfig    `yaml:"cleanup"`
	Log        LogConfig        `yaml:"log"`
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

	// StallTimeout は転送中の無進捗検知タイムアウト。進捗がこの時間止まると
	// 切断扱いにして Range 再開する（River の総時間タイムアウトは無効化している
	// ため、これが ingest の唯一のタイムアウト）。0 なら worker 側の既定値（30 秒）。
	StallTimeout time.Duration `yaml:"stall_timeout"`
}

// EpgConfig は EPG プロジェクションの設定。
type EpgConfig struct {
	// SyncInterval は mirakc から EPG を全量取得する間隔。
	SyncInterval time.Duration `yaml:"sync_interval"`

	// RetentionGrace は放送終了からこの時間が経った番組をローリングウィンドウから刈り取る。
	RetentionGrace time.Duration `yaml:"retention_grace"`
}

// RulerConfig は ruler（ルール評価パス）の設定。
type RulerConfig struct {
	// MaxDeletesPerPass は 1 サイト・1 パスあたりの導出削除許容数（大量削除サーキット
	// ブレーカーの閾値。internal/ruler.Config.MaxDeletesPerPass、docs/recording.md
	// §3.2「大量削除サーキットブレーカー」）。超えたら削除を一切実行せず発動し、
	// 手動で再開するまで止まり続ける（ラッチ。issue #24 M2-5）。
	// 0 なら ruler 側の既定値（50）を使う。
	MaxDeletesPerPass int `yaml:"max_deletes_per_pass"`
}

// ReconcilerConfig は reconciler（宣言的同期パス）の設定。
type ReconcilerConfig struct {
	// StartDelayGrace は開始遅延検出器（internal/reconciler.Config.StartDelayGrace、
	// docs/recording.md §3.3「開始遅延検出器」）の猶予。開始時刻からこの時間が
	// 経っても recordings.started_at が観測されない予約を「開始遅延」として
	// 検出し、slog.Error とゲージ（rokuban_reconcile_start_delayed）に出す。
	// mirakc 側の未知の不具合への保険（EPGStation#724 の実例あり）。
	// 0 なら reconciler 側の既定値（3 分）を使う。
	StartDelayGrace time.Duration `yaml:"start_delay_grace"`
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
//
// プロファイルはデプロイ属性（その環境の ffmpeg ビルドと HW で何ができるか）なので
// config 側に置き、DB のルールからは名前参照する（docs/configuration.md
// 「config と DB の境界」、issue #64 M3-2）。
type EncodeConfig struct {
	FFmpeg  string `yaml:"ffmpeg"`
	FFprobe string `yaml:"ffprobe"`

	// Concurrency は encode キューの MaxWorkers。0 / 未設定は Load 後の既定値 1。
	Concurrency int `yaml:"concurrency"`

	// ThumbnailConcurrency は thumbnail キューの MaxWorkers。0 / 未設定は Load 後の既定値 1。
	ThumbnailConcurrency int `yaml:"thumbnail_concurrency"`

	Profiles []EncodeProfile `yaml:"profiles"`
}

// EncodeProfile は構造化エンコードプロファイルの定義。
//
// 自由形式の cmd 文字列は採らない（EPGStation の命令的テンプレートを繰り返さない。
// issue #64）。worker がこのフィールドから ffmpeg 引数を組み立てる（M3-3）。
type EncodeProfile struct {
	// Name はルール / overrides から参照する一意な名前。
	Name string `yaml:"name"`

	// Container は出力コンテナ。mp4 または mkv（拡張子と -f に対応）。
	Container string `yaml:"container"`

	// VideoCodec は -c:v に渡すコーデック名（例: libx264）。
	VideoCodec string `yaml:"video_codec"`

	// AudioCodec は -c:a に渡すコーデック名（例: aac）。
	AudioCodec string `yaml:"audio_codec"`

	// Height はスケール先の高さ。0 または省略ならスケールしない。
	Height int `yaml:"height"`

	// CRF は品質指定（任意。未設定は nil）。
	CRF *int `yaml:"crf"`

	// Preset はエンコーダの preset（任意。空なら付けない）。
	Preset string `yaml:"preset"`

	// ExtraArgs は組み立てた ffmpeg 引数の末尾に追加する引数（任意）。
	// 自由形式のコマンド全体は受け取らない。
	ExtraArgs []string `yaml:"extra_args"`
}

// Profile は name に一致するプロファイルを返す。見つからなければ ok=false。
func (c EncodeConfig) Profile(name string) (EncodeProfile, bool) {
	for _, p := range c.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return EncodeProfile{}, false
}

// ProfileNames は定義済みプロファイル名を定義順で返す。
func (c EncodeConfig) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for _, p := range c.Profiles {
		names = append(names, p.Name)
	}
	return names
}

// ValidateTools は ffmpeg / ffprobe が PATH（または絶対パス）で解決できることを
// 検査する。worker ロールの起動時だけ呼ぶ（不変条件 4: ffmpeg/ffprobe の exec は
// worker / streamer パッケージのみ。api は呼ばない）。
func (c EncodeConfig) ValidateTools() error {
	if _, err := exec.LookPath(c.FFmpeg); err != nil {
		return fmt.Errorf("encode.ffmpeg %q not found in PATH: %w", c.FFmpeg, err)
	}
	if _, err := exec.LookPath(c.FFprobe); err != nil {
		return fmt.Errorf("encode.ffprobe %q not found in PATH: %w", c.FFprobe, err)
	}
	return nil
}

func (c *EncodeConfig) applyDefaults() {
	// 0 / 未設定だけ既定に寄せる。負値は validate で弾く（黙って 1 にしない）。
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.ThumbnailConcurrency == 0 {
		c.ThumbnailConcurrency = 1
	}
}

// validate はプロファイル定義の妥当性を検査する（Load 時）。
func (c EncodeConfig) validate() error {
	if c.Concurrency < 1 {
		return fmt.Errorf("encode.concurrency must be >= 1, got %d", c.Concurrency)
	}
	if c.ThumbnailConcurrency < 1 {
		return fmt.Errorf("encode.thumbnail_concurrency must be >= 1, got %d", c.ThumbnailConcurrency)
	}
	seen := make(map[string]struct{}, len(c.Profiles))
	for i, p := range c.Profiles {
		if p.Name == "" {
			return fmt.Errorf("encode.profiles[%d].name is required", i)
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("encode.profiles: duplicate name %q", p.Name)
		}
		seen[p.Name] = struct{}{}

		switch p.Container {
		case "mp4", "mkv":
		default:
			return fmt.Errorf("encode.profiles[%d] (%s): container must be mp4 or mkv, got %q",
				i, p.Name, p.Container)
		}
		if p.VideoCodec == "" {
			return fmt.Errorf("encode.profiles[%d] (%s): video_codec is required", i, p.Name)
		}
		if p.AudioCodec == "" {
			return fmt.Errorf("encode.profiles[%d] (%s): audio_codec is required", i, p.Name)
		}
		if p.Height < 0 {
			return fmt.Errorf("encode.profiles[%d] (%s): height must be >= 0, got %d",
				i, p.Name, p.Height)
		}
	}
	return nil
}

// WebhookConfig は外部通知用の単一 HTTP webhook 設定（M3-11）。
//
// EPGStation の複数種外部コマンドフックを 1 本の HTTP POST に置き換える。
// URL が空なら no-op（配送しない）。本処理（ingest / encode 等）は webhook の
// 成否で止めない（at-least-once の最小配送。失敗はログ）。
type WebhookConfig struct {
	// URL は POST 先。空なら webhook を送らない。
	URL string `yaml:"url"`

	// Secret が非空なら X-Rokuban-Webhook-Secret ヘッダに載せる。
	// 受け側の共有秘密。URL にクエリで載せない。
	Secret string `yaml:"secret"`

	// Timeout は 1 回の HTTP 要求のタイムアウト。0 なら 5s。
	Timeout time.Duration `yaml:"timeout"`

	// Events は配送するイベント type の allowlist。空なら既知の全イベントを有効とみなす。
	// 例: recording.finished, recording.failed, encode.finished, encode.failed, recording.deleted
	Events []string `yaml:"events"`
}

// CleanupConfig は削除 reconcile（M3-8、docs/storage.md §7）の設定。
type CleanupConfig struct {
	// TrashRetention はごみ箱（recordings.deleted_at）の猶予期間。
	// 0 なら既定値（30 日）。
	TrashRetention time.Duration `yaml:"trash_retention"`

	// OrphanMTimeGrace は孤児候補にするまでの mtime 猶予。この時間より新しい
	// ファイルは孤児候補にしない（正常系の録画→ingest→エンコードは数時間で
	// 完結するため）。0 なら既定値（7 日）。
	OrphanMTimeGrace time.Duration `yaml:"orphan_mtime_grace"`

	// OrphanAge は孤児候補が `orphan_files` に記録されてから実削除されるまでの
	// エイジング期間。DB リストアで first_seen ごと失われるため窓は開き直る。
	// 0 なら既定値（14 日）。
	OrphanAge time.Duration `yaml:"orphan_age"`

	// MaxDeletesPerPass は 1 パスで実行してよい物理削除数の上限（一括削除
	// サーキットブレーカーの閾値。ソースを問わず 1 パス全体の合計に対して働く。
	// docs/storage.md §7「一括削除サーキットブレーカーはループ全体に 1 つ」）。
	// 0 なら既定値（100）を使う。
	MaxDeletesPerPass int `yaml:"max_deletes_per_pass"`
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
			Concurrency:  2,
			StallTimeout: 30 * time.Second,
		},
		Epg: EpgConfig{
			SyncInterval:   10 * time.Minute,
			RetentionGrace: 24 * time.Hour,
		},
		Worker: WorkerConfig{
			PeriodicJobs: true,
		},
		Encode: EncodeConfig{
			FFmpeg:               "ffmpeg",
			FFprobe:              "ffprobe",
			Concurrency:          1,
			ThumbnailConcurrency: 1,
		},
		Webhook: WebhookConfig{
			Timeout: 5 * time.Second,
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

	// 0 / 未設定は既定 1 に寄せてからプロファイル定義を検査する。
	// concurrency の 0 を「既定」として許すのは ingest と同じ慣習で、
	// 明示の負値や不正プロファイルはここで落とす。
	cfg.Encode.applyDefaults()
	if err := cfg.Encode.validate(); err != nil {
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
