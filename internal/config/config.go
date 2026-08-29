package config

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/drone/envsubst"
	"github.com/goccy/go-yaml"

	"github.com/fetburner/rokuban/internal/ffargs"
)

// Config はアプリケーション全体の設定。
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	DB         DBConfig         `yaml:"db"`
	Mirakc     MirakcConfig     `yaml:"mirakc"`
	Mirakcs    []MirakcSite     `yaml:"mirakcs"`
	Storage    StorageConfig    `yaml:"storage"`
	Ingest     IngestConfig     `yaml:"ingest"`
	Epg        EpgConfig        `yaml:"epg"`
	Ruler      RulerConfig      `yaml:"ruler"`
	Reconciler ReconcilerConfig `yaml:"reconciler"`
	Worker     WorkerConfig     `yaml:"worker"`
	Encode     EncodeConfig     `yaml:"encode"`
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
	// roles を渡さない単発 CLI コマンド（rescue/enqueue/shadow-diff）では
	// pgxpool の既定値（max(4, NumCPU)）がそのまま使われる。
	MaxConns int `yaml:"max_conns"`

	// APIStatementTimeout は api ロールを含むプロセスのプールにだけ適用する
	// statement_timeout（docs/operations.md §3「API 系クエリに statement_timeout」）。
	// クエリ単位の context timeout ではなく接続の RuntimeParams で一括適用する
	// —— クエリ単位だと「付け忘れた 1 本」が必ず生まれるため（issue #90）。
	// 0（未指定）なら既定値（30s）を使う。api ロールを含まないプロセス
	// （worker/watcher 単独等）には適用しない。
	APIStatementTimeout time.Duration `yaml:"api_statement_timeout"`

	// PoolerCompat を true にすると PgBouncer / Neon pooler の transaction pooling
	// 越しでも壊れないよう pgx の prepared statement キャッシュを無効化する
	// （DefaultQueryExecMode を QueryExecModeExec にする）。
	//
	// **pooler を通せるのは api ロールと streamer ロールだけ**（デプロイの契約。docs/operations.md §3）。
	// worker（River 内部の LISTEN）/ watcher（advisory lock）/ notifier（LISTEN）は
	// セッション状態に依存するため transaction pooling 越しでは構造的に壊れる。
	// db.NewPool はこれらのロールと PoolerCompat=true の組み合わせを起動時エラーにする。
	PoolerCompat bool `yaml:"pooler_compat"`
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

// MirakcConfig は mirakc 接続設定（単一サイト構成の糖衣）。
//
// `mirakc: {url, site}` は `mirakcs: [{site, url}]` の 1 要素と等価に解決される
// （Config.Registry）。**`mirakc:` と `mirakcs:` の同時指定は起動エラー**
// （Config.validateMirakcRegistry、どちらが勝つかを覚えさせない。issue #183 M4-11）。
// 「同時指定」はキーを書いたかで判定する（detectMirakcKeyWritten）。Site は
// defaults() が埋めるため、値の非ゼロ性では書いたかどうかを判定できない。
//
// **URL を missingRequired に載せてはならない。** `mirakcs:` を使う構成では
// `mirakc:` を書かないため、無条件の必須検査は正しい構成を起動失敗させる
// （issue #183 の「罠」）。required 相当の検査は validateMirakcRegistry が
// 「どちらか一方は必須」という形で行う。
type MirakcConfig struct {
	URL string `yaml:"url"`

	// Site はこの mirakc インスタンスのサイト名。programId / record id は
	// mirakc インスタンス単位のスコープしか持たないため、DB の全テーブルと
	// API のパスがこの名前でスコープされる（docs/schema.md §1-5、issue #31）。
	// 空なら既定値（"default"）を使う。
	Site string `yaml:"site"`
}

// MirakcSite は `mirakcs:` レジストリの 1 要素。site 名と URL の 2 つだけを持つ。
//
// **storage / worker / ingest 等のチューニング値は要素に入れない。** アーカイブは
// 単一（`media_assets` テーブルに site 列が無い）
// であり、`worker.queues` / `worker.periodic_jobs` 等はデプロイ時のパラメータで
// あって site の属性ではない。site ごとのチューニング値は、それを読むコードが
// できたときに足す（不変条件 11: 形を固定する前に判定基準を書く。issue #183 M4-11）。
type MirakcSite struct {
	Site string `yaml:"site"`
	URL  string `yaml:"url"`
}

// Registry はこの Config が指す mirakc サイトの一覧を返す。
//
// `mirakcs:` が非空ならそれをそのまま返す。空なら `mirakc:` を 1 要素のレジストリ
// として解決する（`mirakc.url` が空なら Mirakc も未設定と見なし、空のレジストリを
// 返す）。両方が同時に非空になるケースは Load が起動エラーにするので、Load を
// 経た Config では実質どちらか一方だけが反映される。
//
// **Load を経た Config では空にならない**（どちらも未設定・url を欠いた `mirakc:`
// はいずれも Load が起動エラーにする）。逆に validateMirakcRegistry は検査対象を
// ここから取らない —— url が空の Mirakc を捨てる挙動が、まさに検査したい
// 「url を欠いた `mirakc:`」を検査対象から外してしまうため。
func (c Config) Registry() []MirakcSite {
	if len(c.Mirakcs) > 0 {
		return c.Mirakcs
	}
	if c.Mirakc.URL == "" {
		return nil
	}
	return []MirakcSite{{Site: c.Mirakc.Site, URL: c.Mirakc.URL}}
}

// mirakcSiteNamePattern は site 名の構文制約。
//
// **River のキュー名の制約（`validateQueueName`、river@v0.40.0/client.go:2335）
// と同一で、緩めない。** M4-13 がキュー名を `ingest_<site>` の形に site で修飾する
// ため、ここを緩めると M4-13 が site 名をキュー名として弾くことになる
// （issue #183 の「罠」）。
var mirakcSiteNamePattern = regexp.MustCompile(`^[a-z0-9]([_-]?[a-z0-9])*$`)

// MirakcSiteNameMaxLen は site 名の最大長。
//
// River のキュー名の上限（internal/worker.riverQueueNameMaxLen、64 文字）から、
// site 単位のキュー（internal/worker.siteBoundQueueNames = ingest/epg/
// reconciler/watcher）を qualifyQueueName で `<base>_<site>` に修飾したときの
// prefix のうち最長のもの（`reconciler_`、11 文字。base の中で `reconciler` が
// 最長のため）を引いた値: 64 - 11 = 53。**siteBoundQueueNames に `reconciler`
// より長い論理名が増えたら、この 53 を引き直す必要がある**
// （internal/worker.TestSiteBoundQueueNames_FitWithinMirakcSiteNameMaxLen が
// この関係を機械的に固定している）。
const MirakcSiteNameMaxLen = 53

// reservedSiteNames は実在する予約ディレクトリと衝突する site 名。
//
// `catalog/`（internal/catalog.Subdir）は削除 reconcile の孤児回収
// （internal/worker/delete_reconcile.go の walkMediaFiles）と rescue スキャン
// （internal/catalog/rescue_scan.go）が SkipDir する対象で、`thumbnails/` は
// サムネイルの名前空間（internal/worker/thumbnail.go）。M4-14 が `rel_path` に
// `{site}/` を前置するようになると、この 2 つと衝突する site 名はそのサイトの
// 原本が孤児回収からも rescue からも見えなくなる（issue #183 の「罠」）。
var reservedSiteNames = map[string]bool{
	"catalog":    true,
	"thumbnails": true,
}

// validateSiteName は site 名の構文制約・上限長・予約名を検査する。
// 見つかった問題を全件返す（規約 4: エラーは全件列挙）。
func validateSiteName(name string) []string {
	var errs []string
	if !mirakcSiteNamePattern.MatchString(name) {
		errs = append(errs, fmt.Sprintf("site name %q must match %s", name, mirakcSiteNamePattern.String()))
	}
	if len(name) > MirakcSiteNameMaxLen {
		errs = append(errs, fmt.Sprintf(
			"site name %q exceeds %d characters (River's queue name limit is 64 characters, and "+
				"site-bound queues carry a prefix such as \"reconciler_\"; the site name itself "+
				"must leave room for that prefix)",
			name, MirakcSiteNameMaxLen))
	}
	if reservedSiteNames[name] {
		errs = append(errs, fmt.Sprintf("site name %q is reserved", name))
	}
	return errs
}

// validateMirakcRegistry は `mirakc:`/`mirakcs:` の相互排他、site 名の構文制約・
// 予約名・重複、各要素の url を検査する。見つかった問題を全件列挙して返す
// （規約 4）。問題が無ければ nil を返す。
//
// mirakcWritten は設定ファイルに `mirakc:` キーが書かれていたか
// （detectMirakcKeyWritten）。**Config の値からは判定できない**ので呼び出し元が
// 渡す —— defaults() が Mirakc.Site に "default" を入れるため、Unmarshal 後の
// Config では「書かれていない `mirakc:`」と「site だけ書かれた `mirakc:`」が
// 同じ値になる。かつて相互排他を `c.Mirakc.URL != ""` で判定していたときは、
// url を欠いた `mirakc: {site: tokyo}` と `mirakcs:` の併記が検査を素通りし、
// 書いた `mirakc.site` が黙って無視されていた（TestLoad_MirakcRegistry の
// "mirakc without url plus mirakcs is an error, not a silent ignore"）。
func (c Config) validateMirakcRegistry(mirakcWritten bool) error {
	mirakcsSet := len(c.Mirakcs) > 0

	var errs []string
	switch {
	case mirakcWritten && mirakcsSet:
		errs = append(errs, "mirakc and mirakcs must not both be set (mirakc is sugar for a one-element mirakcs)")
	case !mirakcWritten && !mirakcsSet:
		errs = append(errs, "one of mirakc.url or mirakcs is required")
	default:
		// `mirakc:` 側は Registry() を経由しない。Registry() は url が空の
		// Mirakc を「未設定」として捨てるので、url を欠いた `mirakc: {site: ...}`
		// が検査対象ゼロ件になり、下の "url is required" に到達しない。
		registry := c.Mirakcs
		if !mirakcsSet {
			registry = []MirakcSite{{Site: c.Mirakc.Site, URL: c.Mirakc.URL}}
		}
		seen := make(map[string]bool, len(registry))
		for i, s := range registry {
			label := "mirakc"
			if mirakcsSet {
				label = fmt.Sprintf("mirakcs[%d]", i)
			}
			for _, e := range validateSiteName(s.Site) {
				errs = append(errs, fmt.Sprintf("%s: %s", label, e))
			}
			if s.Site != "" && seen[s.Site] {
				errs = append(errs, fmt.Sprintf("%s: duplicate site %q", label, s.Site))
			}
			seen[s.Site] = true

			switch {
			case s.URL == "":
				errs = append(errs, fmt.Sprintf("%s.url is required", label))
			case !isAbsoluteURL(s.URL):
				errs = append(errs, fmt.Sprintf("%s.url %q is not a valid URL", label, s.URL))
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("mirakc registry validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

// isAbsoluteURL は mirakc の url として使える形か（scheme と host を持つ絶対 URL）
// を返す。**HTTP クライアントに渡せるか**だけを見る。
//
// scheme や host を欠いても mirakc.NewClient は文字列を保持するだけで失敗せず、
// `http.NewRequest` も通る。最初に失敗するのは `Client.Do`（RoundTrip）で、
// 実測ではメッセージも入力ごとに違う（`/api/tuners` は
// `unsupported protocol scheme ""`、`http://` は `http: no Host in request URL`）。
// つまり起動は通り、録画のたびに違う理由で失敗する。設定の誤りは起動時に出す。
//
// 到達性は検査しない（起動時に mirakc が落ちていても起動は通す。レベル
// トリガーで後から収束する）。
func isAbsoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

// StorageConfig はメディアファイルの保存先設定。
type StorageConfig struct {
	MediaDir   string `yaml:"media_dir"`
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
// issue #64）。worker がこのフィールドから ffmpeg 引数を組み立てる（M3-3、
// M3-3 拡張版 issue #321 が HW エンコードの構造化フィールドを追加）。
//
// # 追加したキーの命名の理由（issue #321 決定コメント）
//
//  1. hwaccel はネストしたブロック（*ffargs.HWAccel）であり、hwaccel_kind の
//     ようなフラット 3 本にしない。ブロックの存在そのものが「-i の前に出す」と
//     いう主張になる（不変条件 10）。フラットだと「device だけ書いた」状態が
//     「何も出さない」と区別できず、掃除する規則が要る。ポインタなので
//     `hwaccel:`（値なし）は nil、`hwaccel: {}` は「書いた」で kind is required
//     になる（detectMirakcKeyWritten が固定している goccy/go-yaml の挙動と同じ）。
//  2. scaler は「系統の名前」であって filter 文字列ではない。`-vf` /
//     `video_filter` というキーは永久に作らない。filtergraph は第 2 の
//     コマンド言語で、`scale_vaapi=...,drawtext=...` と書けた時点で cmd を
//     別名で解禁したのと同じになる。幾何の入力は height 1 本に保つ。
//  3. height + HW スケールでソフトの scale=-2:H が出ないのは検査ではなく構造。
//     filter を作る経路が ffargs.ScaleArgs(scaler, height) の 1 本だけで、
//     返るのは常に filter 1 個。「両方 append する」コードが書けなければ
//     両方は出ない。
//  4. 品質は crf / qp の 2 キー排他。quality: {mode, value} は採らない。
//     キー名がエンコーダ自身のオプション名そのものなので、系統が増えるたびに
//     腐るマッピング表が要らない。両方書いたら起動エラー（優先順位を
//     覚えさせない。ffargs.ValidateVideo）。
//  5. -global_quality / -cq / -q:v はキーにしない。extra_args が届く位置
//     （コーデック指定より後ろ）には構造を足さない、という基準（下記）に従う。
//     届く綴りを今フィールドにすると、テストされていない綴りが増えるだけ
//     （不変条件 11: 書き手のいない形は決めない）。
//  6. extra_args は改名しない。位置が変わっていないから意味も変わっていない
//     （**ただし位置は 1 点だけ変わる**: `-f`（コンテナ）の後ろから前に移した
//     ---VOD と live で「ユーザーのオプションはコーデック/品質/スケール指定の
//     後・アプリ所有の末尾の前」という 1 つの規則にするため。`-f` は許可済み
//     オプションに含まれないので、ユーザーが旧位置に依存する余地は無い）。
//     対称性のために既存の全 config を壊す価値はないので、新しい方（input 側）
//     の名前に位置を入れる: input_extra_args（ffmpeg 用語の input options の位置）。
//  7. アプリが握り続けるもの: -y / -i / 入出力パス / -f / -progress pipe:1 /
//     -loglevel error。ユーザーが書けるのは ffargs.ValidateExtraArgs が値の個数まで
//     把握する allowlist のオプション列だけで、コマンド文字列ではない。値を取らない
//     `-an` 等も明示するため、直後に 2 本目の出力パスを密輸できない。
//  8. device の存在は起動時に検査しない。公式イメージと device の無い CI が
//     落ちる。無い device を書いたプロファイルはジョブ失敗でよい（マウントは
//     k8s resources.limits / Docker --device の話でこの構造体の外）。
//
// scaler が受け付ける値の集合は「filter の綴りを実際に確かめた系統」に限る
// （ffargs.AllowedScalers の doc コメント参照。未検証の綴りを黙って許すより
// 系統ごと除外する）。
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

	// Scaler はスケール filter の系統（既定 ""=software。ffargs.Scaler）。
	// height が 0 のときに書くと起動エラー（何も主張しないキーを黙って無視
	// しない。不変条件 10 と同じ形）。
	Scaler ffargs.Scaler `yaml:"scaler"`

	// CRF は品質指定（任意。未設定は nil）。qp との同時指定は起動エラー。
	CRF *int `yaml:"crf"`

	// QP は品質指定（任意。未設定は nil）。VAAPI 等 crf を解さないエンコーダ用。
	// crf との同時指定は起動エラー（優先順位を実行時に決めさせない）。
	QP *int `yaml:"qp"`

	// Preset はエンコーダの preset（任意。空なら付けない）。
	Preset string `yaml:"preset"`

	// HWAccel は -i より前に出す唯一のブロック（任意。nil なら何も出さない）。
	HWAccel *ffargs.HWAccel `yaml:"hwaccel"`

	// InputExtraArgs は -i の直前に追加する許可済み引数（任意。入力側）。
	InputExtraArgs []string `yaml:"input_extra_args"`

	// ExtraArgs は組み立てた ffmpeg 引数に追加する許可済み引数（任意。出力側 ---
	// コーデック/品質/スケール指定の後、アプリ所有の末尾（-f/-progress/出力
	// パス）の前）。自由形式のコマンド全体は受け取らない。
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
		if err := validateEncodeProfileFFArgs(p); err != nil {
			return fmt.Errorf("encode.profiles[%d] (%s): %w", i, p.Name, err)
		}
	}
	return nil
}

// validateEncodeProfileFFArgs は VOD プロファイル 1 件ぶんの ffargs 検査
// （scaler/height/crf/qp、hwaccel ブロック、extra_args/input_extra_args の
// allowlist）をまとめる。live.profiles 側も同じ ffargs 関数を通すことで、
// 片側だけ直る事故を防ぐ（issue #321 決定コメント §5）。
func validateEncodeProfileFFArgs(p EncodeProfile) error {
	var errs []string
	if err := ffargs.ValidateVideo(p.Scaler, p.Height, p.CRF, p.QP); err != nil {
		errs = append(errs, err.Error())
	}
	if err := p.HWAccel.Validate(); err != nil {
		errs = append(errs, err.Error())
	}
	// extra_args と input_extra_args の両方を検査し、1 回のエラーに全件出す
	// （どちらか片方だけを検査する実装ミスをテストで検出できるように）。
	if err := ffargs.ValidateExtraArgs("extra_args", p.ExtraArgs); err != nil {
		errs = append(errs, err.Error())
	}
	if err := ffargs.ValidateExtraArgs("input_extra_args", p.InputExtraArgs); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// LiveConfig はライブ視聴（HLS streamer、issue #91）の設定。
//
// **DB を引かない。** ライブセッションはインメモリの使い捨てで（crash-only の
// 唯一の例外。docs/overview.md §設計原則）、認可はリバースプロキシ委譲、同時上限も
// プロセスローカル。config だけで完結する（docs/configuration.md「config と DB の
// 境界」）。
type LiveConfig struct {
	// Enabled が false ならライブ視聴のルートを一切登録しない。既定 false。
	//
	// **ffmpeg の LookPath 検査もこれが true のときだけ行う**（cmd/rokuban/server.go）。
	// 公式イメージ（ffmpeg 無し、docs/overview.md §イメージ戦略）で streamer ロールを
	// 起動する構成（録画配信 / サムネイルのみ）を、ライブを設定していないという理由で
	// 壊さない。
	Enabled bool `yaml:"enabled"`

	FFmpeg string `yaml:"ffmpeg"`

	// SegmentDir は HLS セグメント/プレイリストの書き出し先。**録画バッファ
	// （mirakc recording.basedir）と同じディスクに置かない**（視聴が録画の I/O を
	// 飽和させうる。docs/operations.md §5「ライブのセグメントを録画バッファと同じ
	// ディスクに置かない」）。tmpfs 前提（k8s なら `emptyDir: {medium: Memory}`）。
	SegmentDir string `yaml:"segment_dir"`

	// MaxSessions はこのプロセスが同時に持てるライブセッション（≒ ffmpeg プロセス）数。
	//
	// **プロセスローカルな上限であり、グローバルな天井ではない。** グローバルな天井は
	// チューナー数で、裁定者は mirakc（docs/operations.md §5「既定を 1 にする根拠と、
	// 増やす判定基準」）。レプリカを増やしてもこの値は上がらない。0 なら既定値（4）。
	MaxSessions int `yaml:"max_sessions"`

	// IdleTimeout はサービス単位の idle GC の猶予。そのサービスへのセグメント要求が
	// この時間来なければ ffmpeg を止める（docs/api.md §ライブ視聴の HLS。「クライアント
	// 1 人ごとの生存」は追わない）。0 なら既定値（30s）。
	IdleTimeout time.Duration `yaml:"idle_timeout"`

	// TunerPriority は mirakc への各ライブ要求に載せる X-Mirakurun-Priority。
	//
	// ruler が生成する schedule の既定 priority（10）より低く保つことで、チューナー
	// 枯渇時に mirakc が録画側を常に勝たせる（docs/recording/delegation.md §2
	// 「チューナー調停」、issue #91 の決定コメント）。0 なら既定値（1）。
	//
	// **`TunerPriority < rules.priority` はここでは検証しない。** 前者は config
	// （この構造体）、後者は DB（ユーザーが自由に編集できる）で、両者を跨いで
	// 検証する権威がどちらの層にも無い。ルールの priority を既定 10 未満に下げる
	// 運用では、この既定値のままだとライブが録画に勝つ（docs/api.md §ライブ視聴の
	// HLS §実装 参照）。
	TunerPriority int `yaml:"tuner_priority"`

	// HWAccel は -i より前に出す唯一のブロック（任意。nil なら何も出さない）。
	//
	// **プロファイル毎ではなく live セクション直下に置く。** ライブは 1 回の
	// ffmpeg で入力 1 本・出力 N 本であり、-hwaccel は入力側のオプション。
	// プロファイル毎に持たせると「プロファイル 2 つが別の hwaccel を要求する」
	// という表現できない設定が書けてしまう --- セクション直下に置けばそれが
	// 表現不可能になる（不変条件 10「CHECK で禁止するより表現不可能にする」。
	// issue #321 決定コメント §1）。
	HWAccel *ffargs.HWAccel `yaml:"hwaccel"`

	// InputExtraArgs は `-i` の直前に追加する許可済み引数（任意。入力側。
	// HWAccel と同じ理由でプロファイル毎ではなく live セクション直下）。
	InputExtraArgs []string `yaml:"input_extra_args"`

	Profiles []LiveProfile `yaml:"profiles"`
}

// LiveProfile は HLS トランスコードの構造化プロファイル。
//
// **`encode.profiles`（VOD 派生物）を流用しない。** HLS はセグメント長・プレイリスト
// 長・キーフレーム間隔という VOD には無い制約を持ち、共有構造体に足すと VOD 側に
// 無関係なフィールドが増える。ISDB-T 地上波の映像は MPEG-2 で、ブラウザの HLS
// 経路（hls.js/MSE）は事実上再生できないため、H.264 へのトランスコードは前提とする
// （mirakc フィルタ + `-c copy` では受信端末を満たさない。issue #91 の決定コメント）。
// 自由形式の cmd 文字列は採らない（encode.profiles と同じ方針）。
//
// **scaler / crf / qp はプロファイル毎に持つ**（HWAccel/InputExtraArgs とは対照的
// --- これらは出力側オプションなので出力ごとに違ってよい。issue #321 決定コメント §1）。
type LiveProfile struct {
	// Name はクエリ（`?profile=`）から参照する一意な名前。ライブのセグメント
	// ファイル名の接頭辞にも使う（1 プロセス内で複数プロファイルの出力を同じ
	// サービスディレクトリに平置きするため。internal/streamer 参照）ため、
	// パス成分として安全な文字だけに制限する（validate）。
	Name string `yaml:"name"`

	VideoCodec string `yaml:"video_codec"`
	AudioCodec string `yaml:"audio_codec"`

	// Height はスケール先の高さ。0 または省略ならスケールしない。
	Height int `yaml:"height"`

	// Scaler はスケール filter の系統（既定 ""=software。ffargs.Scaler）。
	// height が 0 のときに書くと起動エラー。
	Scaler ffargs.Scaler `yaml:"scaler"`

	// CRF は品質指定（任意。未設定は nil）。qp との同時指定は起動エラー。
	CRF *int `yaml:"crf"`

	// QP は品質指定（任意。未設定は nil）。crf との同時指定は起動エラー。
	QP *int `yaml:"qp"`

	Preset string `yaml:"preset"`

	// SegmentSeconds は 1 セグメントの長さ。0 なら既定値（2）。
	SegmentSeconds int `yaml:"segment_seconds"`

	// PlaylistSize はプレイリストに保持するセグメント数（-hls_list_size）。
	// 古いセグメントは削除する（-hls_flags delete_segments）。0 なら既定値（6）。
	PlaylistSize int `yaml:"playlist_size"`

	// ExtraArgs は組み立てた ffmpeg 引数に追加する許可済み引数（任意。
	// 出力側 --- コーデック/品質/スケール指定の後、`-f hls` の前）。
	ExtraArgs []string `yaml:"extra_args"`
}

// ValidateTools は ffmpeg が PATH（または絶対パス）で解決できることを検査する。
// live.enabled が true の streamer ロール起動時だけ呼ぶ
// （不変条件 4、LiveConfig.Enabled のコメント参照）。
func (c LiveConfig) ValidateTools() error {
	if _, err := exec.LookPath(c.FFmpeg); err != nil {
		return fmt.Errorf("live.ffmpeg %q not found in PATH: %w", c.FFmpeg, err)
	}
	return nil
}

func (c *LiveConfig) applyDefaults() {
	if c.MaxSessions == 0 {
		c.MaxSessions = 4
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 30 * time.Second
	}
	if c.TunerPriority == 0 {
		c.TunerPriority = 1
	}
	for i := range c.Profiles {
		if c.Profiles[i].SegmentSeconds == 0 {
			c.Profiles[i].SegmentSeconds = 2
		}
		if c.Profiles[i].PlaylistSize == 0 {
			c.Profiles[i].PlaylistSize = 6
		}
	}
}

// liveProfileNamePattern は LiveProfile.Name のパス安全な文字集合
// （セグメントファイル名の接頭辞に使うため。英数字・ハイフン・アンダースコアのみ）。
var liveProfileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validate は live 設定の妥当性を検査する（Load 時、applyDefaults の後）。
func (c LiveConfig) validate() error {
	// **値域は enabled に関わらず見る。** `enabled: false` のまま値だけ先に
	// 書いておく構成は実在し（`config.compose.yml` が「後で true にする」形で
	// 出荷している）、そこに書き間違えた負値が入ると、ライブを有効にした日に
	// 初めて起動しなくなる。設定ファイルの誤りは書いた時点で出す。
	if c.MaxSessions < 1 {
		return fmt.Errorf("live.max_sessions must be >= 1, got %d", c.MaxSessions)
	}
	if c.TunerPriority < 0 {
		return fmt.Errorf("live.tuner_priority must be >= 0, got %d", c.TunerPriority)
	}
	// プロファイル・セグメント先の必須性は enabled のときだけ（未設定の
	// プロファイルを検査対象にしない）。
	if !c.Enabled {
		return nil
	}
	if len(c.Profiles) == 0 {
		return fmt.Errorf("live.profiles is required when live.enabled is true")
	}
	if c.SegmentDir == "" {
		return fmt.Errorf("live.segment_dir is required when live.enabled is true")
	}
	if c.IdleTimeout <= 0 {
		return fmt.Errorf("live.idle_timeout must be > 0, got %v", c.IdleTimeout)
	}
	if err := c.HWAccel.Validate(); err != nil {
		return fmt.Errorf("live.hwaccel: %w", err)
	}
	if err := ffargs.ValidateExtraArgs("live.input_extra_args", c.InputExtraArgs); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(c.Profiles))
	for i, p := range c.Profiles {
		if p.Name == "" {
			return fmt.Errorf("live.profiles[%d].name is required", i)
		}
		if !liveProfileNamePattern.MatchString(p.Name) {
			return fmt.Errorf("live.profiles[%d].name %q must match %s",
				i, p.Name, liveProfileNamePattern.String())
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("live.profiles: duplicate name %q", p.Name)
		}
		seen[p.Name] = struct{}{}

		if p.VideoCodec == "" {
			return fmt.Errorf("live.profiles[%d] (%s): video_codec is required", i, p.Name)
		}
		if p.AudioCodec == "" {
			return fmt.Errorf("live.profiles[%d] (%s): audio_codec is required", i, p.Name)
		}
		if p.Height < 0 {
			return fmt.Errorf("live.profiles[%d] (%s): height must be >= 0, got %d", i, p.Name, p.Height)
		}
		if p.SegmentSeconds < 1 {
			return fmt.Errorf("live.profiles[%d] (%s): segment_seconds must be >= 1, got %d",
				i, p.Name, p.SegmentSeconds)
		}
		if p.PlaylistSize < 1 {
			return fmt.Errorf("live.profiles[%d] (%s): playlist_size must be >= 1, got %d",
				i, p.Name, p.PlaylistSize)
		}
		if err := ffargs.ValidateVideo(p.Scaler, p.Height, p.CRF, p.QP); err != nil {
			return fmt.Errorf("live.profiles[%d] (%s): %w", i, p.Name, err)
		}
		if err := ffargs.ValidateExtraArgs("extra_args", p.ExtraArgs); err != nil {
			return fmt.Errorf("live.profiles[%d] (%s): %w", i, p.Name, err)
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
		Mirakc: MirakcConfig{
			Site: "default",
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
		Live: LiveConfig{
			FFmpeg: "ffmpeg",
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
// **struct タグではなくここに並べる。** タグは「その型が読まれたら必ず走る」ので、
// 構成によっては書かないキー（`mirakcs:` を使う構成の `mirakc.url`）に付けると
// 正しい構成を起動失敗させる（issue #183 の「罠」）。必須かどうかが他のキーの
// 有無で決まる検査は、それを知っている場所（validateMirakcRegistry）に置く。
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

// detectMirakcKeyWritten は展開済み YAML に `mirakc:` キーが書かれていたかを返す。
//
// **キーだけ書いて値が無い `mirakc:`（null）は「書かれていない」、空マップの
// `mirakc: {}` は「書かれた」になる。** goccy/go-yaml が null をポインタの nil に
// デコードするかどうかに乗った挙動なので、リポジトリ側で固定しておく
// （TestLoad_MirakcRegistry の "bare mirakc: key with no value counts as unwritten"
// / "empty mirakc: {} counts as written"）。
//
// defaults() が Mirakc.Site を埋めてしまうため、相互排他の判定に必要な「書いたか
// どうか」は Unmarshal 後の Config からは復元できない。ここだけキーの有無を見る。
//
// **Strict は使わない。** 未知キーの検出は本体の Unmarshal が済ませており、
// ここで再度落とすと同じ typo が二重に報告される。またこの probe は `mirakc:`
// しか持たないので、Strict にすると他のキーを書いた正しい設定が全部落ちる。
func detectMirakcKeyWritten(expanded string) (bool, error) {
	var probe struct {
		Mirakc *MirakcConfig `yaml:"mirakc"`
	}
	if err := yaml.Unmarshal([]byte(expanded), &probe); err != nil {
		return false, fmt.Errorf("parsing config: %w", err)
	}
	return probe.Mirakc != nil, nil
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

	if missing := cfg.missingRequired(); len(missing) > 0 {
		return nil, &ValidationError{missing: missing}
	}

	if err := cfg.DB.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}
	if err := cfg.Log.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	// mirakc:/mirakcs: の相互排他・site 名の構文制約・予約名・重複・url を検査する
	// （mirakc.url は missingRequired に載せられない。missingRequired の
	// コメントと issue #183 の「罠」参照）。
	mirakcWritten, err := detectMirakcKeyWritten(expanded)
	if err != nil {
		return nil, err
	}
	if err := cfg.validateMirakcRegistry(mirakcWritten); err != nil {
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
