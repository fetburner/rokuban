package config

// Config 本体・既定値・読み込みと検証。

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/drone/envsubst"
	"github.com/goccy/go-yaml"
)

// Config はアプリケーション全体の設定。
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	DB         DBConfig         `yaml:"db"`
	Mirakcs    []MirakcSite     `yaml:"mirakcs"`
	Storage    StorageConfig    `yaml:"storage"`
	Ingest     IngestConfig     `yaml:"ingest"`
	Epg        EpgConfig        `yaml:"epg"`
	Ruler      RulerConfig      `yaml:"ruler"`
	Reconciler ReconcilerConfig `yaml:"reconciler"`
	Worker     WorkerConfig     `yaml:"worker"`
	Encode     EncodeConfig     `yaml:"encode"`
	CMDetect   CMDetectConfig   `yaml:"cm_detect"`
	Live       LiveConfig       `yaml:"live"`
	Webhook    WebhookConfig    `yaml:"webhook"`
	Cleanup    CleanupConfig    `yaml:"cleanup"`
	Log        LogConfig        `yaml:"log"`
}

// ServerConfig は HTTP サーバーの設定。
type ServerConfig struct {
	Listen       string   `yaml:"listen"`
	AllowedHosts []string `yaml:"allowed_hosts"`

	// TrustForwardedHost は `X-Forwarded-Host` を allowed_hosts の検証対象に
	// するかどうか（既定 false）。信頼できるリバースプロキシが必ず前段に
	// 居り、かつプロキシが外来の `X-Forwarded-Host` を上書きする構成でのみ
	// true にする。opt-in にする理由と直接露出構成でのリスクは
	// docs/configuration.md §server.allowed_hosts を参照（issue #216）。
	TrustForwardedHost bool `yaml:"trust_forwarded_host"`
}

// DBConfig は PostgreSQL 接続設定。
type DBConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
	SSLMode  string `yaml:"sslmode"`

	// MaxConns はこのプロセスが持つ唯一のコネクションプールの上限（issue #90）。
	// プロセスは常に 1 個のプールしか持たない（全ロールがそれを共有する。
	// docs/operations.md §3「輻輳時の隔離」）ため、「ロール別プール上限」は
	// 複数プールを作ることではなく、この 1 個の上限を決めることを指す。
	// 0（未指定）なら db.NewPool がプロセスの roles 集合から自動算出する。
	// roles を渡さない単発 CLI コマンド（rescue/enqueue）では
	// pgxpool の既定値（max(4, NumCPU)）がそのまま使われる。
	MaxConns int `yaml:"max_conns"`

	// APIStatementTimeout は api ロールを含むプロセスのプールにだけ適用する
	// statement_timeout（docs/operations.md §3「API 系クエリに statement_timeout」）。
	// クエリ単位の context timeout ではなく接続の RuntimeParams で一括適用する
	// —— クエリ単位だと「付け忘れた 1 本」が必ず生まれるため（issue #90）。
	// 0（未指定）なら既定値（30s）を使う。api ロールを含まないプロセス
	// （worker/watcher 単独等）には適用しない。
	APIStatementTimeout time.Duration `yaml:"api_statement_timeout"`
}

// validate は DB 設定のうち、値の範囲で決まるものを検査する（Load 時）。
// 必須キーの欠落は missingRequired が別に全件列挙する。
func (c DBConfig) validate() error {
	if c.MaxConns < 0 {
		return fmt.Errorf("db.max_conns must be >= 0, got %d", c.MaxConns)
	}
	return nil
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

// StorageConfig はメディアファイルの保存先設定。
//
// MediaDir は ingest の強いファイルシステム契約（file fsync、Close のエラー報告、
// 同一 FS 内の atomic rename、rename 後の親ディレクトリ fsync）を満たす root に
// 限る。worker は ingest キューを購読するとき起動時に操作プローブを行うが、
// パス文字列から FS の種類を推測しない。geesefs / s3fs / Mountpoint のような
// FUSE S3 を原本 ingest 先には使わず、派生物専用の領域でのみ使う。
type StorageConfig struct {
	MediaDir   string `yaml:"media_dir"`
	ScratchDir string `yaml:"scratch_dir"`

	// AccelLocation を設定すると録画ファイルの配信を X-Accel-Redirect で
	// リバースプロキシに委ねる（認可判定はアプリ、バイト転送は nginx）。
	// 値は nginx の internal location（例: /_media/）。空なら Go が直接配る。
	AccelLocation string `yaml:"accel_location"`
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

	// MissingAssetAge は「state='active' なのに実体ファイルが無い」候補が
	// `missing_media_assets` に記録されてから、確認済みとして報告（メトリクス /
	// ログ）されるまでのエイジング期間。孤児回収の OrphanAge と同じ理由 ---
	// 単発の走査揺れ・DB リストア直後の一時的な不整合を確認済みの異常と
	// 区別する。0 なら既定値（24 時間）。自動削除の閾値ではない
	// （docs/storage/retention.md §7「孤児回収の逆」。この検出は削除を一切
	// 行わない）。
	MissingAssetAge time.Duration `yaml:"missing_asset_age"`
}

// LogConfig はログ出力の設定。
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// logLevels / logFormats は受け付ける値。**空文字はここに含めない。**
// `level: ${VAR}` の展開結果が空文字になる構成があり、それは validate が
// 別途「未設定」として通す（validate の doc コメント参照）。defaults() が
// 埋めるのはキーが無いときだけなので、空文字はここまで残って来る。
var (
	logLevels  = []string{"debug", "info", "warn", "error"}
	logFormats = []string{"json", "text"}
)

// validate はログ設定の値が既知の集合に入っているかを検査する（Load 時）。
// 見つかった問題は全件返す（規約 4: エラーは全件列挙。level と format の
// 両方が不正なとき、直して再起動して次のエラーを見る往復を強いない）。
//
// **空文字は「未設定」として通す。** `defaults()` が埋めるのは**キーが無い**
// ときだけなので、`level: ${ROKUBAN_LOG_LEVEL}`（`:-` 無し）のような展開で
// 空文字が入る構成が実在する。ここで落とすと、その構成は起動しなくなる。
func (c LogConfig) validate() error {
	var errs []string
	if c.Level != "" && !slices.Contains(logLevels, c.Level) {
		errs = append(errs, fmt.Sprintf("log.level must be one of %s, got %q",
			strings.Join(logLevels, "/"), c.Level))
	}
	if c.Format != "" && !slices.Contains(logFormats, c.Format) {
		errs = append(errs, fmt.Sprintf("log.format must be one of %s, got %q",
			strings.Join(logFormats, "/"), c.Format))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
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
			Concurrency:  3,
			StallTimeout: 30 * time.Second,
		},
		Epg: EpgConfig{
			SyncInterval:   10 * time.Minute,
			RetentionGrace: 24 * time.Hour,
		},
		Worker: WorkerConfig{
			PeriodicJobs:         true,
			RescueStuckJobsAfter: defaultRescueStuckJobsAfter,
		},
		Encode: EncodeConfig{
			FFmpeg:               "ffmpeg",
			FFprobe:              "ffprobe",
			Concurrency:          1,
			ThumbnailConcurrency: 1,
		},
		CMDetect: CMDetectConfig{
			BinaryDir: "/usr/local/bin",
		},
		Live: LiveConfig{
			FFmpeg:  "ffmpeg",
			FFprobe: "ffprobe",
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

// missingRequired は「無ければ起動できない」設定キーのうち空のものを、YAML の
// パス表記で全件返す（規約 4: エラーは全件列挙）。
//
// **struct タグではなくここに並べる。** `mirakcs:` は空配列を許容する型なので
// struct タグの required 相当では表現できない。必須かどうかが要素数で決まる
// 検査は、それを知っている場所（validateMirakcRegistry）に置く。
func (c Config) missingRequired() []string {
	var missing []string
	for _, k := range []struct{ path, value string }{
		{"db.host", c.DB.Host},
		{"db.user", c.DB.User},
		{"db.password", c.DB.Password},
		{"db.database", c.DB.Database},
		{"storage.media_dir", c.Storage.MediaDir},
	} {
		if k.value == "" {
			missing = append(missing, k.path)
		}
	}
	return missing
}

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

	// ruler.retract_grace は「未設定」（既定 1h）と明示的な 0（無効化）を区別する
	// 必要があるため、他のフィールドのように defaults() の値をそのまま Unmarshal に
	// 上書きさせるのではなく、Unmarshal 後に nil のときだけここで埋める
	// （RulerConfig.RetractGrace のコメント参照。defaults() 側にポインタの既定値を
	// 置くと、goccy/go-yaml がポインタの参照先を書き換えた場合に defaults() が
	// 呼ばれるたびに使い回される同一の変数を意図せず共有しうるため、Load するたびに
	// 新しいポインタをここで割り当てる）。
	if cfg.Ruler.RetractGrace == nil {
		d := time.Hour
		cfg.Ruler.RetractGrace = &d
	}

	if missing := cfg.missingRequired(); len(missing) > 0 {
		return nil, &ValidationError{missing: missing}
	}

	if err := cfg.DB.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}
	if err := cfg.Log.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}
	if err := cfg.Ingest.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}
	if err := cfg.Epg.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}
	if err := cfg.Ruler.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	// mirakcs: の非空性・site 名の構文制約・予約名・重複・url を検査する
	// （mirakcs は空配列を許すため missingRequired に載せられない。
	// missingRequired のコメント参照）。
	if err := cfg.validateMirakcRegistry(); err != nil {
		return nil, err
	}

	// 0 / 未設定は既定 1 に寄せてからプロファイル定義を検査する。
	// concurrency の 0 を「既定」として許すのは ingest と同じ慣習で、
	// 明示の負値や不正プロファイルはここで落とす。
	cfg.Encode.applyDefaults()
	if err := cfg.Encode.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	// live.enabled が false のときは検査しない（未設定のプロファイルを検査対象に
	// しない。LiveConfig.Enabled のコメント参照）。
	cfg.Live.applyDefaults()
	if err := cfg.Live.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// ValidationError は必須キーが欠けているときのエラー。
type ValidationError struct {
	missing []string
}

// Error は欠けているキーを全件並べたメッセージを返す。
func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.missing))
	for i, path := range e.missing {
		msgs[i] = path + " is required"
	}
	return fmt.Sprintf("config validation failed:\n  - %s", strings.Join(msgs, "\n  - "))
}

// MissingKeys は欠けている設定キーを YAML のパス表記で返す。
func (e *ValidationError) MissingKeys() []string {
	return e.missing
}
