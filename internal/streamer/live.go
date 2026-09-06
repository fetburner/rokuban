// ライブ視聴（mirakc → ffmpeg → HLS）の実装（issue #91、#56 の決定を実現する側）。
//
// 資源同定・スケール方針は docs/api.md §ライブ視聴の HLS と docs/operations.md §5
// 「streamer のスケール」で決まっている。ここで守る 3 点:
//
//   - URL はセッション ID を持たない。
//     `/api/sites/{site}/networks/{networkId}/services/{serviceId}/live/...`
//     から正規表現 1 本で (site, networkId, serviceId) が取り出せる固定深さ
//   - idle GC の粒度はサービス単位（クライアント 1 人ごとの生存は追わない）
//   - 同時セッション上限はプロセスローカル（グローバルな天井はチューナー数で、
//     裁定者は mirakc）
//
// **DB を引かない**（issue #91 の決定 3）。パスの (networkId, serviceId) は
// SI の値そのもの（`GET /api/sites/{site}/services` が返すのと同じ id 空間）で、
// mirakc が要求する合成 id への変換は programid.ServiceID による純関数（issue #217）。
// セッションはインメモリの使い捨て --- crash-only の唯一の例外
// （docs/overview.md §crash-only）。
package streamer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/programid"
)

// LiveConfig はライブ視聴の設定。streamer.Config が config.StorageConfig を
// 直接使わないのと同じ理由で、config.LiveConfig を直接使わず必要なフィールドだけを
// 独自に持つ（cmd/rokuban/server.go で変換する）。
type LiveConfig struct {
	// Enabled が false なら Mount はライブのルートを一切登録しない。
	Enabled bool

	FFmpeg  string
	FFprobe string

	// Captions は ARIB 字幕を HLS の字幕レンディションとして出力する。
	Captions bool

	// SegmentDir は HLS セグメント/プレイリストの書き出し先ルート。録画バッファとは
	// 別ディスク（tmpfs 前提）。
	SegmentDir string

	// MaxSessions はこのプロセスが同時に持てるライブセッション数（プロセスローカル）。
	MaxSessions int

	// IdleTimeout はサービス単位の idle GC の猶予。
	IdleTimeout time.Duration

	// TunerPriority は mirakc への X-Mirakurun-Priority に載せる値。
	TunerPriority int

	// HWAccel は `-i` より前に出す唯一のブロック（プロファイル毎ではなく
	// LiveConfig 直下。config.LiveConfig.HWAccel と同じ理由 --- 1 回の ffmpeg
	// で入力 1 本・出力 N 本のため、プロファイル毎には表現しない）。
	HWAccel *ffargs.HWAccel

	// InputExtraArgs は `-f mpegts -i pipe:0` の直前に追加する引数。
	InputExtraArgs []string

	Profiles []LiveProfile
}

// LiveProfile は 1 レンディションの HLS トランスコード設定。
type LiveProfile struct {
	Name       string
	VideoCodec string
	AudioCodec string
	Height     int

	// Scaler はスケール filter の系統（config.LiveProfile.Scaler と同じ ffargs.Scaler）。
	Scaler ffargs.Scaler

	// CRF / QP は品質指定（config.LiveProfile と同じく相互排他。両方 nil も可）。
	CRF *int
	QP  *int

	Preset         string
	SegmentSeconds int
	PlaylistSize   int
	ExtraArgs      []string
}

// profile は name に一致するプロファイルを返す。name が空文字なら先頭のプロファイル
// （既定プロファイル）を返す。
func (c LiveConfig) profile(name string) (LiveProfile, bool) {
	if name == "" {
		if len(c.Profiles) == 0 {
			return LiveProfile{}, false
		}
		return c.Profiles[0], true
	}
	for _, p := range c.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return LiveProfile{}, false
}

// mirakcLiveClient はライブ視聴が必要とする mirakc クライアントの最小面。
// 本番は *mirakc.Client、テストは差し替えて開始失敗・切断のタイミングを制御する。
type mirakcLiveClient interface {
	StreamService(ctx context.Context, serviceID int64, priority int) (io.ReadCloser, error)
}

var (
	// errSessionLimit はプロセスローカルな同時セッション上限に達したことを示す。
	// **プロセスローカル**であり、グローバルな天井（チューナー数、mirakc が裁定）
	// ではない（docs/operations.md §5）。
	errSessionLimit = errors.New("live session limit reached (process-local)")
	// errShuttingDown は Run の ctx が既に完了し、新規セッションを受け付けないことを示す。
	errShuttingDown = errors.New("streamer is shutting down")
	// errStartupTimeout は getOrCreateSession の `<-s.ready` 待ちが
	// playlistStartupTimeout を超えたことを示す（issue #286）。mirakc への接続
	// （StreamService、全体タイムアウト無し）がハングすると close(s.ready) に
	// 到達しないため、ハンドラ側の待ちだけを打ち切る。**セッションの起動 goroutine
	// （sessionCtx 由来の runSession）やセッション自体の状態には影響しない**
	// --- 諦めるのはこの呼び出しの待ちだけで、セッションはそのまま起動を続ける。
	errStartupTimeout = errors.New("live session did not become ready in time")
)

// liveUpstreamStartError は mirakc の stream 要求が拒否された起動失敗を表す。
// ffmpeg の起動失敗などとは、idle セッションを退避して再試行できる点が異なる。
type liveUpstreamStartError struct {
	err error
}

func (e *liveUpstreamStartError) Error() string {
	return fmt.Sprintf("requesting mirakc live stream: %v", e.err)
}

func (e *liveUpstreamStartError) Unwrap() error {
	return e.err
}

const (
	// liveCaptionProbeBytes は live MPEG-TS の先頭を ffprobe に渡すサイズ。
	// PAT/PMT は入力の先頭付近に現れるため、ライブ本体を先に消費しすぎずに
	// 字幕 PID の有無を判定できる。読み取ったバイトは必ず ffmpeg に戻す
	// （runSession が io.MultiReader で prefix を先頭に戻す）。地上波 HD で
	// 概ね 0.3 秒ぶんの読み取り（未検証。ビットレートに依存する見積もり）。
	liveCaptionProbeBytes = 512 * 1024
	// liveCaptionProbeTimeout は probeLiveCaptionStream（ffprobe 起動）の暴走を
	// 止める上限。prefix の読み取り自体は runSession が同期に待つため、ここでは
	// timeout しない --- クライアントは playlistStartupTimeout（15s）でセッション
	// 起動全体を待つので、prefix 読み取り専用の timeout は不要（B の決定）。
	liveCaptionProbeTimeout = 5 * time.Second
	// liveMirakcReleaseWait は、退避したセッションの mirakc 接続を Close して
	// `<-s.done` を待った後、1 回だけ再試行する前の待ち時間。実 mirakc
	// 4.0.0-dev.0 + fixture tuner 2 本 + 録画 1 本で、旧ライブの Close から次の
	// 異なる波のライブ要求が通るまでを測ったところ 2.35〜4.18 秒だった（2026-09-06、
	// internal/mirakc/conformance/live_release_test.go）。5 秒にして、mirakc 側の
	// 非同期な tuner プロセス終了の揺れを吸収する。ここは再試行の回数を増やすための
	// backoff ではなく、退避後に 1 回だけ行う解放待ちである。
	liveMirakcReleaseWait = 5 * time.Second
)

// LiveStreamer はライブ視聴の HLS ルートを配信する。
//
// 1 サービス = 1 セッション = 1 ffmpeg プロセス = mirakc の 1 チューナー。同じ
// サービスを複数クライアントが見ても共有する。**セッションキー（`sessions
// map[int64]*liveSession`）が site を含まないのは、この LiveStreamer 自身が
// 単一の site（下記 site フィールド）にしか対応しないため** --- 1 プロセスが
// N site を束縛できるようになった今（issue #532）も、それは cmd/rokuban が
// site ごとに別々の LiveStreamer インスタンスを作ることで満たしている
// （cmd/rokuban/live_sites.go の newLiveStreamersBySite）。全プロファイルを
// 1 回の ffmpeg 起動で同時に出す（issue #91 の決定 1:
// 「1 つの Pod の中で 1 チューナーから複数プロファイルを出す」を、プロファイルごとに
// ffmpeg を分けずに満たす。トレードオフ: 見られていないプロファイルの CPU も使う）。
type LiveStreamer struct {
	mirakc mirakcLiveClient
	site   string
	cfg    LiveConfig

	mu       sync.Mutex
	sessions map[int64]*liveSession
	closed   bool
}

// NewLive は LiveStreamer を生成する。cfg.Enabled が false なら Mount は
// 何も登録しない（ffmpeg 無しの公式イメージで streamer ロールを起動する構成を
// 壊さない。issue #91 の決定 2）。
//
// **cfg.SegmentDir の中身を掃く（crash-only の後始末）。** ライブセッションは
// このプロセスが唯一の書き手であり使い捨てなので、前回プロセスの残骸
// （tmpfs はコンテナ再起動をまたいで残る --- ノード再起動でなければ消えない。
// docs/api.md の従来の記述はここが誤りだった。レビューで指摘）が残っていても
// 安全に消してよい。HTTP リスナーが立つ前（Mount 前）に同期的に行うことで、
// 起動直後に飛んできたリクエストが作ったセッションのディレクトリを
// 後から誤って掃除してしまう競合を避ける。
//
// ponytail: cfg.SegmentDir は site 間で共有の 1 ディレクトリで（site ごとの
// 書き込み先は SegmentDir/{site}/ に分かれる）、cmd/rokuban.newLiveStreamersBySite
// は束縛サイトごとにこの NewLive を呼ぶため、N site 束縛では起動時にこの
// 掃除が N 回走る（2 回目以降は 1 回目が既に空にした後の中身の無い掃除）。
// 全呼び出しが HTTP リスナーが立つ前・同期的に終わる今の配線では無害（他の
// サイトの掃除が割り込む競合は起きない）だが、掃除自体は本来プロセス起動で
// 1 回でよい仕事。site 数が増えて無視できないコストになったら
// newLiveStreamersBySite 側で 1 回だけ呼ぶ形に上げる。
func NewLive(mirakcClient *mirakc.Client, site string, cfg LiveConfig) *LiveStreamer {
	return newLiveStreamer(mirakcClient, site, cfg)
}

func newLiveStreamer(client mirakcLiveClient, site string, cfg LiveConfig) *LiveStreamer {
	if cfg.Enabled && cfg.SegmentDir != "" {
		sweepStaleLiveSegments(cfg.SegmentDir)
	}
	return &LiveStreamer{
		mirakc:   client,
		site:     site,
		cfg:      cfg,
		sessions: make(map[int64]*liveSession),
	}
}

// sweepStaleLiveSegments は dir の中身だけを消す（dir 自体には触れない。issue #189）。
//
// **dir 自体を os.RemoveAll すると、dir が k8s emptyDir を直接マウントした
// マウントポイントそのものである構成で毎起動 slog.Warn が出る。** Linux では
// マウントポイントに対する rmdir が EBUSY を返す（rmdir(2) の仕様どおり。
// Linux コンテナで実測: 中身を全部消した後でも `os.RemoveAll(mountpoint)` は
// "unlinkat ...: device or resource busy" を返した）。docs の推奨値
// `/dev/shm/rokuban-live` のように tmpfs の**サブディレクトリ**を使う構成では
// 該当しない --- サブディレクトリ自体はマウントポイントではないので rmdir できる。
// 中身だけを個別に RemoveAll すれば、dir 自体の rmdir を一切試みないのでこの
// 失敗が起きない（同じ Linux コンテナで実測して確認済み）。
func sweepStaleLiveSegments(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 前回プロセスの残骸が無い（初回起動等）。掃く対象が無いだけで異常ではない。
			return
		}
		slog.Warn("streamer: sweeping stale live segment dir at startup", "dir", dir, "err", err)
		return
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			slog.Warn("streamer: sweeping stale live segment dir entry at startup",
				"dir", dir, "entry", entry.Name(), "err", err)
		}
	}
}

// LiveRoutePattern はライブ視聴のルートの固定深さパターン（issue #56 の決定。
// 1 つの nginx 変数で (site, networkId, serviceId) を取り出せる）。OpenAPI には
// 載せない（バイナリ + 長寿命という原本 /file と同じ理由）。
//
// **1 プロセスが N site を束縛できるようになったため（issue #532）、この定数を
// 経由するルート登録者は 2 種類ある**: この Mount（1 site 専用、production では
// 呼ばれない --- cmd/rokuban は複数 LiveStreamer を同じパターンに重ねて Mount
// すると chi が黙って最後の登録で上書きするため、cmd/rokuban.liveSites.Mount が
// URL の {site} で正しいインスタンスへ委譲する形でパターンを 1 回だけ登録する）と、
// その liveSites.Mount 自身。パターン文字列を 2 か所に手書きすると、どちらかだけ
// 変えて食い違う経路ができる（qualifyQueueName のコメントが警告するのと同じ族の
// 罠）ので、export してどちらもこの定数を参照する。
const LiveRoutePattern = "/api/sites/{site}/networks/{networkId}/services/{serviceId}/live"

// Mount はライブ視聴のルートを登録する（cfg.Enabled が true のときだけ）。
//
// **production では呼ばれない。** cmd/rokuban は 1 プロセスが束縛する site ごとに
// 1 つの LiveStreamer を作るが（issue #532）、どれも同じ LiveRoutePattern を
// 登録するため、この Mount を site の数だけ呼ぶと chi が黙って最後の登録で
// 上書きしてしまう（cmd/rokuban.liveSites の doc コメント参照）。production の
// 配線は cmd/rokuban.liveSites.Mount がパターンを 1 回だけ登録し、URL の
// {site} で正しいインスタンスの Playlist/Segment/Leave に委譲する。この Mount は
// このパッケージ自身のルートテスト（1 インスタンスだけを相手にする単体テスト）
// のために残してある。
func (ls *LiveStreamer) Mount(r chi.Router) {
	if !ls.cfg.Enabled {
		return
	}
	r.Get(LiveRoutePattern+"/playlist.m3u8", ls.Playlist)
	r.Get(LiveRoutePattern+"/segments/{name}", ls.Segment)
	if ls.cfg.Captions {
		// ffmpeg の variant playlist は master と同じディレクトリに置かれる。
		// master の相対 URI（playlist_0.m3u8）をそのまま解決できるようにする。
		r.Get(LiveRoutePattern+"/{name}", ls.Segment)
	}
	r.Post(LiveRoutePattern+"/leave", ls.Leave)
}

// Run は idle GC ループを ctx が Done になるまで回す。ctx が Done になったら
// 保持している全セッションを止めて（mirakc の接続も閉じる = チューナー解放）から
// 返る。notifier.EventHub.Run と同じ形で eg.Go から呼ぶことを想定する。
func (ls *LiveStreamer) Run(ctx context.Context) error {
	ticker := time.NewTicker(ls.gcInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			ls.shutdown()
			return nil
		case <-ticker.C:
			ls.reapIdle()
		}
	}
}

// gcInterval は idle GC ループの刻みを返す。
//
// **刻みは「先に来る方の期限」に合わせる = min(IdleTimeout, leaveGrace) / 2。**
// IdleTimeout/2 のままだと、離脱ヒントで idle 期限を「いま + 猶予」に詰めても
// 次の GC パスが来るまで（既定 30s/2 = 15 秒）回収されず、ヒントの効果が刻みに
// 飲まれる。逆に leaveGrace だけを見ると、猶予が IdleTimeout より長い設定
// （ヒントが no-op になる設定）で刻みが IdleTimeout より粗くなり、**ヒントと
// 無関係な通常の idle GC まで遅くなる** --- min はどちらの劣化も防ぐ。
// 下限 1 秒は、極端に短い設定でループが busy loop 化するのを防ぐため。
func (ls *LiveStreamer) gcInterval() time.Duration {
	interval := ls.cfg.IdleTimeout
	if grace := ls.cfg.leaveGrace(); grace < interval {
		interval = grace
	}
	interval /= 2
	if interval < time.Second {
		interval = time.Second
	}
	return interval
}

// leaveGrace は離脱ヒント（Leave）を受けたときに idle 期限を詰める先までの猶予。
//
// **設定キーにせず `live.profiles[].segment_seconds` から導出する。** 守るべき
// 性質は「猶予 > 生きている視聴者の次の要求が来るまでの間隔」で、**定常状態の**
// その間隔を決めているのはセグメント長そのもの（プレイリスト再取得もセグメント
// 取得も last-access を更新し、どちらもおおむねセグメント長の周期で来る）。
//
// **定常状態でない区間 --- セッションの起動待ち --- では、間隔を決めているのは
// セグメント長ではなく playlistStartupTimeout（最大 15 秒）である。**そこは
// この値を大きくして守るのではなく、待っている側が touch し続けることで
// 「無音区間」自体を無くして守る（waitReadyTouching の doc コメント。
// レビュー指摘で 504 を実測した経路）--- 猶予に起動待ちを織り込むと、
// ヒントの効き（既定 8 秒での解放）がその分そのまま鈍る。独立した
// 設定キーにすると `segment_seconds: 6` と `leave_grace: 1s` のような組み合わせが
// 書けてしまい、**leave が「他人の視聴を切る道具」に化ける**（issue #191 の罠）。
// 導出にすればその組み合わせは表現不可能になる。
//
// 係数 3 + マージン 2 秒は「連続 3 回ぶんの取りこぼしを許す」という選択で、
// 既定（segment_seconds: 2）で 8 秒。**実クライアントの要求間隔を測った値では
// ない**（未検証）--- セグメント長より短くならないことだけが要件で、そこには
// 大きな余裕がある。
//
// **IdleTimeout でクリップしない（レビュー指摘。issue #191）。** 初版は
// `min(3×segment+2s, IdleTimeout)` にしていたが、これは `segment_seconds: 6` +
// `idle_timeout: 2s`（`internal/api/api_test.go` に実在する組み合わせ）のような
// 設定で猶予 2 秒 < セグメント長 6 秒となり、**この関数が持つべき唯一の性質
// （猶予 > セグメント長）を、docs がそう書いている当の場所で破っていた**。
//
// 実害が出ていなかったのは hintLeave の「前へ進めない」clamp が吸収していた
// ためで（クリップ後は必ず `grace == IdleTimeout` になり、詰め先が「いま」に
// なって no-op に落ちる。`TestLiveStreamer_LeaveHint_ClippedGraceIsNoOp` で
// 固定）、**2 つの安全装置が絡んで初めて安全**という状態だった。clamp を将来
// 触った瞬間に「他人の視聴を切る道具」が出現する。ここでは値そのものが常に
// 性質を満たすようにし、猶予が IdleTimeout 以上になる設定では
// **ヒントが no-op になる**（= 何も起こらない）方へ倒す。
//
// 設定バリデーションで `idle_timeout > 3×segment+2s` を要求する案は採らない。
// 「速く解放したいので idle_timeout を短くする」は運用者の正当な意思であり、
// **config の値の組で起動を止めるのは重い**（[docs/configuration.md] の
// 「config と DB の境界」= 運用者の意思を否定しない）。導出側で守れば、危険な
// 組み合わせの帰結は起動失敗ではなく「ヒントが効かないだけ」で済む。
func (c LiveConfig) leaveGrace() time.Duration {
	return time.Duration(3*c.longestSegmentSeconds())*time.Second + 2*time.Second
}

func (c LiveConfig) longestSegmentSeconds() int {
	longest := 0
	for _, p := range c.Profiles {
		if p.SegmentSeconds > longest {
			longest = p.SegmentSeconds
		}
	}
	return longest
}

// idleEvictionThreshold は起動失敗時の退避候補に要求する idle 時間。
// leaveGrace とは別の導出値で、正常な視聴者を起動失敗の圧力で切らないために
// 最長セグメント 2 本ぶんを要求する。
func (c LiveConfig) idleEvictionThreshold() time.Duration {
	return time.Duration(2*c.longestSegmentSeconds()) * time.Second
}

// playlistStartupTimeout は元々「ffmpeg がプレイリストの初回書き出しを終える
// までの待ち時間」（waitForPlaylist、セッションが ready になった**後**の
// ファイル出現待ち）として導入された値だが、現在はセッションの起動待ち
// （mirakc への接続 + ffmpeg exec が終わる = `close(s.ready)` を待つ経路）にも
// 同じ値を掛けている --- 参照する箇所は次の 4 つ:
//
//   - getOrCreateSession の既存セッション経路の `<-s.ready` 待ち（Playlist が呼ぶ。issue #286）
//   - getOrCreateSession の新規作成経路の `<-s.ready` 待ち（同上。issue #286 --- この
//     2 経路は互いに独立したコードパスであり、片方だけ直すと非対称が残る。
//     実際にレビューで新規作成経路側だけテストが無いまま気付かれず、指摘された）
//   - Segment の `<-s.ready` 待ち（issue #189）
//   - waitForPlaylist（Playlist、ready になった後のプレイリストファイル出現待ち）
//
// **4 箇所が同じ 1 つの変数を参照することが本質。** 分けると、どれか 1 つだけ
// 直したときに非対称が残っても気付けない --- 実際に #189 で Segment だけ
// 直したときに Playlist 側（getOrCreateSession）の非対称が見過ごされ、
// レビューで #286 として指摘された。新しい待ちを足すときもここを増やさず
// この変数を再利用すること。
//
// **Playlist ハンドラ 1 本の最悪応答時間はこの値の 1 回分ではない。**
// getOrCreateSession の起動待ち（最大この値）が終わってから waitForPlaylist
// （さらに最大この値）が直列で走るため、両方が上限いっぱいまでかかると
// Playlist の合計は**この値の 2 倍**（既定なら 30s）になる。Segment は
// getOrCreateSession を経由しない分、この値 1 回分（既定 15s）で済む。
//
// var にしてあるのはテストからの上書き用（15 秒の実待ちはテストを不必要に
// 遅くする）。運用者向けの設定キーではない。
var playlistStartupTimeout = 15 * time.Second

const playlistPollInterval = 100 * time.Millisecond

// Playlist は GET /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/playlist.m3u8
// を処理する。
// `?profile=` が無ければ既定（先頭）プロファイル。
func (ls *LiveStreamer) Playlist(w http.ResponseWriter, r *http.Request) {
	serviceID, ok := ls.resolveRequest(w, r)
	if !ok {
		return
	}

	profile, ok := ls.cfg.profile(r.URL.Query().Get("profile"))
	if !ok {
		http.Error(w, "unknown live profile", http.StatusBadRequest)
		return
	}

	s, err := ls.getOrCreateSession(r.Context(), serviceID)
	if err != nil {
		if errors.Is(err, errStartupTimeout) {
			slog.Error("streamer: live playlist session did not become ready in time",
				"service_id", serviceID)
		}
		writeSessionError(w, err)
		return
	}
	s.touch()

	playlistName := profile.Name + ".m3u8"
	if ls.cfg.Captions {
		playlistName = "playlist.m3u8"
	}
	playlistPath := filepath.Join(s.dir, playlistName)
	readyMarker := "#EXTINF"
	if ls.cfg.Captions {
		readyMarker = "#EXT-X-STREAM-INF"
	}
	content, ok := waitForPlaylist(r.Context(), s, playlistPath, playlistStartupTimeout, readyMarker)
	if !ok {
		slog.Error("streamer: live playlist did not appear in time",
			"service_id", serviceID, "profile", profile.Name, "dir", s.dir)
		http.Error(w, "live stream did not start in time", http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	// ライブは毎回内容が変わるので immutable もキャッシュも不可。
	w.Header().Set("Cache-Control", "no-store")
	// waitForPlaylist が読んだ内容をそのまま返す（http.ServeFile で再度開くと、
	// -hls_flags temp_file の rename と競合する窓が理論上もう 1 つ増える）。
	_, _ = w.Write(content)
}

// segmentNamePattern はセグメントファイル名として許す文字集合。
// '/' を含まないので、これだけでパストラバーサルを防げる（filepath.Join の相手が
// 常に 1 階層のファイル名になる）。
var segmentNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.(?:ts|vtt|m3u8)$`)

// Segment は GET /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/segments/{name}
// を処理する。
//
// name にプロファイルは含まれない代わりに、ffmpeg が書き出すファイル名自体に
// プロファイル名を接頭辞として焼く（BuildLiveFFmpegArgs）。セグメント URL に
// `?profile=` は不要 --- プレイリストが相対パスで正しいファイル名を指す。
func (ls *LiveStreamer) Segment(w http.ResponseWriter, r *http.Request) {
	serviceID, ok := ls.resolveRequest(w, r)
	if !ok {
		return
	}

	name := chi.URLParam(r, "name")
	if !segmentNamePattern.MatchString(name) || (!ls.cfg.Captions && !strings.HasSuffix(name, ".ts")) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}

	ls.mu.Lock()
	s, ok := ls.sessions[serviceID]
	ls.mu.Unlock()
	if !ok {
		// idle GC で回収済み、または未開始。hls.js はプレイリストを再取得しにいき、
		// そこで新しいセッションが起きる（レベルトリガーと同じ形。セッション ID を
		// 持たないので「詰む」経路が無い。docs/api.md §ライブ視聴の HLS）。
		http.NotFound(w, r)
		return
	}

	// **s.ready を待つまで s.dir を読まない。** マップに入っていても起動処理
	// （runSession）がまだ s.dir を書いていない可能性があり、同期無しで読むと
	// データ競合になる（レビューで指摘）。通常は起こらない
	// （クライアントはプレイリストで ready 待ちを経てからでないとセグメント名を
	// 知り得ない）が、起動が異常に遅い・クライアントが古いセグメント名を
	// 使い回す等の窓を防御的に塞ぐ。
	//
	// **playlistStartupTimeout で打ち切る（issue #189）。** セッションの起動は
	// mirakc への接続（streamClient、全体タイムアウト無し）を含むため、mirakc が
	// 応答しないと ready が閉じない。ctx.Done() だけだとこのリクエストのクライアントが
	// 切るまでハンドラが占有し続ける --- Playlist 側の waitForPlaylist が同じ期限を
	// 持つのに Segment だけ無期限なのは非対称なので揃える。
	//
	// **close(s.ready) の性質は変えない。** ここで諦めるのはこのハンドラの待ちだけで、
	// セッションの起動 goroutine（runSession）はそのまま走り続ける。既に走っている
	// 起動が完了すれば、後続の別リクエストは通常どおりそのセッションを使える。
	if err := waitReadyTouching(r.Context(), s, playlistStartupTimeout); err != nil {
		if errors.Is(err, errStartupTimeout) {
			slog.Error("streamer: live segment session did not become ready in time",
				"service_id", serviceID)
			// Playlist 側の起動失敗と同じ扱い（同じステータス・同じ文言）に揃える。
			http.Error(w, "live stream did not start in time", http.StatusGatewayTimeout)
		}
		// ctx のキャンセル（クライアントが切った）は何も書かずに戻る。
		return
	}
	if s.startErr != nil {
		// 起動失敗（すぐ map から外れるはずだが、その直前の窓を防御的に処理する）。
		http.NotFound(w, r)
		return
	}
	s.touch()

	path := filepath.Join(s.dir, "segments", name)
	if ls.cfg.Captions && (strings.HasSuffix(name, ".m3u8") || strings.HasSuffix(name, ".vtt")) {
		path = filepath.Join(s.dir, name)
	}

	if ls.cfg.Captions && strings.HasSuffix(name, ".m3u8") {
		// **master と同じ readiness 待ちを、1 段下の variant / 字幕 playlist にも
		// 掛ける。** waitForPlaylist の doc コメント（「書き込み途中の空/不完全な
		// 内容を配らない」「CI が確率的に flaky になった原因」）は master
		// （Playlist ハンドラ）にしか効いていなかった --- master に
		// EXT-X-STREAM-INF があることは、そこが指す variant playlist に
		// セグメントが書かれていることを保証しない。クライアントは master を
		// 受け取った直後にこの playlist_0.m3u8 等を取りに来るので、同じ窓が
		// 1 段下で復活する。既存のヘルパをそのまま再利用する（新しい待機機構は
		// 作らない）。
		content, ok := waitForPlaylist(r.Context(), s, path, playlistStartupTimeout, "#EXTINF")
		if !ok {
			slog.Error("streamer: live variant playlist did not appear in time",
				"service_id", serviceID, "name", name, "dir", s.dir)
			http.Error(w, "live stream did not start in time", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(content)
		return
	}

	// **この Content-Type にフロントの再生経路判定が依存している。変えるなら
	// `web/src/lib/live.ts` の `supportsNativeHls` も同時に変える。** あちらは
	// 「`<video>` がプレイリストとセグメントの両方の MIME を再生できるか」で
	// ネイティブ HLS と hls.js を振り分けており、MPEG-2 TS を demux できるのが
	// WebKit だけであることが唯一の判別子になっている（m3u8 の MIME に対する
	// `canPlayType` の戻り値は WebKit も Chrome も同じなので区別できない）。
	// 例えばセグメントを fMP4（`video/mp4`）に変えると、Chrome の
	// `canPlayType('video/mp4')` は `'maybe'` なのでネイティブ対応と誤判定され、
	// **Chrome が沈黙して再生できなくなる**（M4-4 のレビューで 2 度踏んだ形）。
	// `web/e2e/live.mjs` はこのハンドラをモックするため、この非互換を検出できない
	switch filepath.Ext(name) {
	case ".vtt":
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "video/mp2t")
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

// Leave は POST /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/leave
// を処理する。
//
// **これは停止命令ではなく「離脱のヒント」である。** ライブセッションはサービス
// 単位で共有される（同じチャンネルを別の部屋で見ている視聴者は同じ ffmpeg・同じ
// チューナーを使う。docs/api.md §「資源同定: セッション ID を持たない」）ので、
// 離れた側の要求でセッションを止める形にすると**別の視聴者の再生を一方的に切れて
// しまう**。代わりに idle 期限を「いま + leaveGrace」に詰めるだけにする ---
// 他に視聴者がいれば、その人の次のセグメント / プレイリスト要求が last-access を
// 更新して期限が元に戻る。ヒントは収束を速めるだけで、「誰かが見ているか」という
// 真実は既存の観測（セグメント要求）が持つ --- レベルトリガー（不変条件 5）と
// 同じ形。
//
// **セッションを作らない。** 該当サービスのセッションが無ければ何もしない
// （未開始・回収済みのどちらでも同じ）。**常に 204 を返す**（存在の有無を
// 漏らさず、`navigator.sendBeacon` に再送の材料も与えない）。
//
// 宛先は**プレイリスト / セグメントと同じ資源同定**（`(site, networkId, serviceId)`。
// id は SI の値で、mirakc 合成 id への変換は resolveRequest が行う。issue #217）---
// セッション ID は URL にもクッキーにも置かない（issue #56）。この口も DB を
// 引かない（issue #91 の決定 3）。
func (ls *LiveStreamer) Leave(w http.ResponseWriter, r *http.Request) {
	serviceID, ok := ls.resolveRequest(w, r)
	if !ok {
		return
	}

	ls.mu.Lock()
	s, ok := ls.sessions[serviceID]
	ls.mu.Unlock()
	if !ok {
		metrics.LiveLeaveHints.WithLabelValues("no_session").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	grace := ls.cfg.leaveGrace()
	if !s.hintLeave(time.Now(), grace, ls.cfg.IdleTimeout) {
		// 期限は動かなかった（設定上ヒントが効かない / 連打の 2 発目以降）。
		// **`deadline_shortened` に数えない** --- 数えると「ヒントで詰めた数」と
		// 「実際に効いた数」が混ざり、idle GC 回収数と対で読めなくなる。
		metrics.LiveLeaveHints.WithLabelValues("no_effect").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	metrics.LiveLeaveHints.WithLabelValues("deadline_shortened").Inc()
	slog.Info("streamer: live leave hint received, shortening idle deadline",
		"service_id", serviceID, "grace", grace)
	w.WriteHeader(http.StatusNoContent)
}

// resolveRequest はパスから (site, networkId, serviceId) を取り出し、site が
// このプロセスの担当（`--sites` で束縛された site）と一致することを確かめたうえで、
// mirakc に渡す合成 service id を返す。DB は引かない（issue #91 の決定 3）---
// 合成は programid.ServiceID の純関数。
//
// **mirakc へ渡るのは常にここで組み立てた整数であり、URL の文字列ではない。**
// パスセグメントを 16 bit 符号なし整数として解析できなければ 400 を返して
// 打ち切るので、細工した値が mirakc の別エンドポイントへの要求に化ける経路が無い
// （TestLiveStreamer_RejectsHostileIDSegments が %2F・クエリ注入・符号付き・
// 桁あふれ・全角数字を、TestLiveStreamer_MirakcPathIsComposedFromPathSegments が
// 実際に mirakc が受け取るパスとクエリを固定する）。「不明な id は mirakc が拒否
// する」という mirakc 側の挙動には依存しない --- 起動に失敗した理由が何であれ
// writeSessionError が 503 にまとめる（issue #217）。
func (ls *LiveStreamer) resolveRequest(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return 0, false
	}
	networkID, ok := parseSIID(chi.URLParam(r, "networkId"))
	if !ok {
		http.Error(w, "invalid network id", http.StatusBadRequest)
		return 0, false
	}
	serviceID, ok := parseSIID(chi.URLParam(r, "serviceId"))
	if !ok {
		http.Error(w, "invalid service id", http.StatusBadRequest)
		return 0, false
	}
	return programid.ServiceID(networkID, serviceID), true
}

// parseSIID は SI の network_id / service_id を表すパスセグメントを解析する。
//
// いずれも SI 上は 16 bit 符号なし整数なので上限をそこに取る。合成
// （programid.ServiceID = networkID*100_000 + serviceID）が可逆であるためには
// serviceID < 100_000 が必要で、16 bit 上限（65535）はそれを満たす。
// strconv.ParseUint(s, 10, 16) は空文字・符号付き・基数接頭辞・アンダースコア
// 区切り・全角数字・65535 超をすべて弾く。
//
// **十進の正準形だけを受ける（先頭ゼロを弾く）。** ParseUint は `01024` を 1024 と
// して受けるが、**前段の consistent hash の鍵は URL の文字列**である
// （docs/operations/k8s.md §5 の `map $uri $live_key`）ため、`1024` と `01024` は
// 同じチャンネルを指しながら別 Pod に落ちる --- そこで ffmpeg とチューナーが
// 2 本になり、「同じチャンネルの視聴者は同じ Pod に落ちるので 1 本で済む」という
// 鍵の取り方の前提そのものが崩れる。streamer 内部の鍵（合成後の整数）は同一に
// なるので単体プロセスでは症状が出ない --- 弾くのは URL の別名を作らないため。
func parseSIID(s string) (int, bool) {
	if len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

func writeSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSessionLimit):
		http.Error(w, "too many concurrent live sessions on this process", http.StatusServiceUnavailable)
	case errors.Is(err, errShuttingDown):
		http.Error(w, "streamer is shutting down", http.StatusServiceUnavailable)
	case errors.Is(err, errStartupTimeout):
		// Segment ハンドラの起動待ちタイムアウトと同じ扱い（同じステータス・
		// 同じ文言）に揃える（issue #286）。
		http.Error(w, "live stream did not start in time", http.StatusGatewayTimeout)
	default:
		// mirakc 側のチューナー枯渇・ffmpeg 起動失敗などをまとめて 503 にする。
		// 詳細はログと rokuban_live_session_start_failures_total{reason} 側で見る。
		http.Error(w, "live stream unavailable", http.StatusServiceUnavailable)
	}
}

// waitForPlaylist は path に有効な HLS プレイリストが書かれるまでポーリングし、
// 読めたらその内容を返す。タイムアウトまたは ctx のキャンセルで ok=false を返す。
//
// **ポーリングのたびに s を touch する**（待っている客も客。waitReadyTouching の
// doc コメントに理由と実測）。
//
// **存在だけでなく内容も見る。** `os.Stat` の成否だけを見ると、ffmpeg が
// `-hls_flags temp_file` を使わずに（あるいは偽 ffmpeg がアトミックでない書き方を
// していて）ファイルへ直接書き込み中の途中の内容を配ってしまう窓がある
// （レビューで発見。CI が確率的に flaky になった原因）。少なくとも 1 本の
// セグメントを指す `#EXTINF` 行が現れるまで待つことで、書き込み途中の空/不完全な
// 内容を配らない。
func waitForPlaylist(ctx context.Context, s *liveSession, path string, timeout time.Duration, readyMarker string) ([]byte, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, []byte(readyMarker)) {
			return data, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(playlistPollInterval):
			// **待っている客も客**（waitReadyTouching の doc コメント参照）。
			// ここが無音のままだと、この区間に届いた離脱ヒントが idle 期限を
			// 詰め、プレイリストを待っている視聴者ごとセッションが回収される
			// （実測: 504。issue #191 のレビュー指摘）。
			s.touch()
		}
	}
}

// liveSession は 1 サービスぶんのライブセッション（1 mirakc 接続 + 1 ffmpeg プロセス）。
//
// crash-only の唯一の例外（使い捨てのインメモリ状態）。DB には一切書かない。
type liveSession struct {
	serviceID int64
	dir       string // SegmentDir/site/serviceID。segments/ 配下にセグメント、直下に <profile>.m3u8

	ready chan struct{} // startSession が終わったら閉じる（成功でも失敗でも）
	done  chan struct{} // ffmpeg プロセスが完全に終了したら閉じる

	startErr error // ready が閉じた後にだけ読む

	cancel context.CancelFunc

	mu         sync.Mutex
	lastAccess time.Time
}

func (s *liveSession) touch() {
	s.mu.Lock()
	s.lastAccess = time.Now()
	s.mu.Unlock()
}

// hintLeave は離脱ヒントを反映する。idle 期限が「now + grace」になるところまで
// lastAccess を**巻き戻す**。
//
// **前へ進める方向には決して動かさない。** grace が idleTimeout 以上の設定
// （あるいは既にもっと古い lastAccess を持つセッション）でこれを無条件に代入
// すると、ヒントが**延命の道具**になる --- 「離れた」と言うだけでセッションを
// 引き延ばせてしまい、意味が反転する。巻き戻しだけを許すことで、ヒントの
// 最悪ケースは「何も起こらない」になる。
//
// この後に誰かが touch() すれば lastAccess は now に戻り、猶予も元の
// idleTimeout に戻る（他の視聴者がいる場合の自己修復。Leave の doc コメント参照）。
//
// 戻り値は**実際に期限を動かしたか**。動かさなかった（＝ヒントが no-op だった）
// ケースは 2 つあり、どちらもメトリクスでは `no_effect` として数える:
// 猶予が IdleTimeout 以上の設定（leaveGrace のコメント参照）と、連打の 2 発目
// 以降（既に詰めた期限より後ろにしか詰められない）。
func (s *liveSession) hintLeave(now time.Time, grace, idleTimeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	shortened := now.Add(grace - idleTimeout)
	if !shortened.Before(s.lastAccess) {
		return false
	}
	s.lastAccess = shortened
	return true
}

// waitReadyTouching は s の起動完了（close(s.ready)）を、**待っている間ずっと
// s を touch しながら**待つ。timeout / ctx.Done で打ち切る。
//
// **待っている客も客である**（issue #191 のレビュー指摘）。ハンドラが
// `<-s.ready` や waitForPlaylist で待っている区間は「誰も要求していない無音区間」
// に見えるが、実際にはそのセッションを待っている視聴者がそこにいる。last-access が
// 止まったままだと、その区間に届いた離脱ヒント（他人のものでも、自分のタブが
// hidden になったものでも）が idle 期限を猶予まで詰め、**起動待ちの視聴者ごと
// セッションが回収される**（実測: 起動待ち 4 秒・猶予 2 秒の構成で、ヒント送出の
// 約 2 秒後に回収され、待っていた視聴者は 504 を受け取った。
// `TestLiveStreamer_LeaveHint_DoesNotKillASessionThatIsStillStartingUp`）。
//
// **GC 側に「起動中は回収しない」という例外を作る形は採らない。** 実測した失敗は
// ready が閉じた**後**のプレイリスト待ちで起きており、「起動中」を ready で
// 判定する例外はそこを覆えない。加えて、例外は「回収されない状態」を新設する
// ので、mirakc がハングして ready が永久に閉じないときにセッションが回収不能に
// なる（チューナーを掴んだまま max_sessions を食い潰す）。ここで touch すれば、
// 真実は last-access 1 つのまま（不変条件 5 のレベルトリガー）で、待ちが
// 終われば自動的に通常の idle 判定に戻る --- 待ちは playlistStartupTimeout で
// 上限が付いているので、これで延命できるのも高々その時間である。
func waitReadyTouching(ctx context.Context, s *liveSession, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(playlistPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ready:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errStartupTimeout
		case <-ticker.C:
			s.touch()
		}
	}
}

func (s *liveSession) idleSince(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Sub(s.lastAccess)
}

// stop は mirakc 接続と ffmpeg プロセスを止め、終了を待つ。ctx キャンセルで
// io.Reader からの読み取り（ffmpeg の stdin コピー）が中断し、
// exec.CommandContext の既定動作でプロセスが kill される。
func (s *liveSession) stop() {
	s.cancel()
	<-s.done
}

// sessionCount はロックを取って現在のセッション数を返す（メトリクス用）。
func (ls *LiveStreamer) sessionCount() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.sessions)
}

// getOrCreateSession は serviceID のセッションを返す。無ければ作る。
//
// **同じ serviceID への同時リクエストは 1 本の ffmpeg に収束する。** マップへの
// 挿入をロック内で行い、実際の起動（mirakc 接続 + ffmpeg exec、時間がかかる）は
// ロック外で行う。後発のリクエストは ready チャネルで起動完了を待つだけで、
// 2 本目の ffmpeg を起動しない。
//
// ctx は**セッション自体の生存**（sessionCtx）ではなく、**この呼び出しがどれだけ
// 待つか**にだけ使う。呼び出し元のリクエストが切れても他の同時リクエストが待って
// いる可能性があるセッションの起動を巻き込んで中断しない --- 待つのをやめるだけ。
//
// **`<-s.ready` 待ちは playlistStartupTimeout で打ち切る（issue #286）。** mirakc
// への接続（StreamService、全体タイムアウト無し）がハングすると close(s.ready) に
// 到達せず、ctx（呼び出し元のリクエストの ctx）だけでは呼び出し元が切断するまで
// 戻らない。**この期限は呼び出し元の ctx に `context.WithTimeout` を被せる形では
// 実装しない** --- getOrCreateSessionOnce の 2 か所の select にそれぞれ
// `case <-time.After(...)` を足すだけに留める。ctx を包んで下位の sessionCtx にまで
// 渡してしまうと、この
// 呼び出しの待ちを諦めるだけのつもりが起動中のセッションそのものを巻き込んで
// 中断してしまう（sessionCtx は `context.Background()` 由来で、ctx とは独立して
// いなければならない。issue #189 の罠と同じ形）。
func (ls *LiveStreamer) getOrCreateSession(ctx context.Context, serviceID int64) (*liveSession, error) {
	s, err := ls.getOrCreateSessionOnce(ctx, serviceID)
	if err == nil {
		return s, nil
	}

	reason, retryable := liveEvictionReason(err)
	if !retryable {
		return nil, err
	}

	// 上流拒否で ready が閉じても、runSession は map からの削除とディレクトリの
	// 掃除を defer で行う。自分の失敗セッションを先に完全終了させないと、再試行が
	// そのセッションを既存セッションとして拾って同じ startErr を返す。
	if s != nil {
		<-s.done
	}

	victim := ls.takeIdleSessionForRetry(time.Now())
	if victim == nil {
		return nil, err
	}

	slog.Info("streamer: evicting idle live session before retry",
		"service_id", victim.serviceID, "reason", reason)
	victim.stop()
	// mirakc は HTTP body の Close と tuner プロセスの解放を同期していない。
	// stop が done まで待っても、直後の要求が容量エラーになる窓が実物で観測された。
	time.Sleep(liveMirakcReleaseWait)

	retry, retryErr := ls.getOrCreateSessionOnce(ctx, serviceID)
	if retryErr != nil && retry != nil {
		// 再試行自身が ready 後に失敗した場合も、次の要求が同じ startErr を
		// 拾わないように、そのセッションの後片付けを待ってから返す。
		<-retry.done
	}
	result := "retry_failed"
	if retryErr == nil {
		result = "retry_succeeded"
	}
	metrics.LiveSessionEvictions.WithLabelValues(reason, result).Inc()
	return retry, retryErr
}

func liveEvictionReason(err error) (string, bool) {
	if errors.Is(err, errSessionLimit) {
		return "session_limit", true
	}
	var upstreamErr *liveUpstreamStartError
	if errors.As(err, &upstreamErr) && !errors.Is(err, context.Canceled) {
		return "upstream", true
	}
	return "", false
}

// getOrCreateSessionOnce は 1 回のセッション取得・起動だけを行う。
// 起動失敗からの退避と再試行は外側の getOrCreateSession が 1 回だけ担当する。
func (ls *LiveStreamer) getOrCreateSessionOnce(ctx context.Context, serviceID int64) (*liveSession, error) {
	ls.mu.Lock()
	if s, ok := ls.sessions[serviceID]; ok {
		ls.mu.Unlock()
		if err := waitReadyTouching(ctx, s, playlistStartupTimeout); err != nil {
			return nil, err
		}
		if s.startErr != nil {
			return s, s.startErr
		}
		return s, nil
	}
	if ls.closed {
		ls.mu.Unlock()
		return nil, errShuttingDown
	}
	if len(ls.sessions) >= ls.cfg.MaxSessions {
		ls.mu.Unlock()
		metrics.LiveSessionStartFailures.WithLabelValues("session_limit").Inc()
		return nil, errSessionLimit
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	s := &liveSession{
		serviceID:  serviceID,
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
		lastAccess: time.Now(),
		cancel:     cancel,
	}
	ls.sessions[serviceID] = s
	ls.mu.Unlock()

	go ls.runSession(sessionCtx, s)

	if err := waitReadyTouching(ctx, s, playlistStartupTimeout); err != nil {
		return nil, err
	}
	if s.startErr != nil {
		return s, s.startErr
	}
	metrics.LiveActiveSessions.Set(float64(ls.sessionCount()))
	return s, nil
}

// takeIdleSessionForRetry は起動失敗時に退避するセッションを 1 本選び、選択と
// map からの削除を同じロック内で行う。呼び出し側は返ったセッションの stop を
// 完了させてから再試行する。
//
// ready 前のセッションは、待っているハンドラが playlistStartupTimeout の間 touch
// し続けるため候補から除外する。waiter がいなくなって idleSince が同じ timeout を
// 超えた起動待ちだけは、mirakc を掴んだままのハングとして退避を許す。ready 済みの
// セッションは最長 segment_seconds の 2 倍より長く idle であることを要求し、その
// 中で最も古いものを選ぶ。leaveGrace は lastAccess を十分に巻き戻すので、離脱ヒント
// を受けたセッションもこの規則で最古の候補になる。
func (ls *LiveStreamer) takeIdleSessionForRetry(now time.Time) *liveSession {
	threshold := ls.cfg.idleEvictionThreshold()
	ls.mu.Lock()
	var victim *liveSession
	var oldest time.Duration
	for _, s := range ls.sessions {
		idle := s.idleSince(now)
		if idle <= threshold {
			continue
		}
		if !sessionReady(s) && idle <= playlistStartupTimeout {
			continue
		}
		if victim == nil || idle > oldest {
			victim = s
			oldest = idle
		}
	}
	if victim != nil {
		delete(ls.sessions, victim.serviceID)
	}
	ls.mu.Unlock()

	if victim != nil {
		metrics.LiveActiveSessions.Set(float64(ls.sessionCount()))
	}
	return victim
}

func sessionReady(s *liveSession) bool {
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

// runSession は 1 セッションの全生涯（mirakc 接続 → ffmpeg 起動 → 終了待ち →
// 後片付け）を担う。呼び出し元は go で起動し、s.ready / s.done で同期する。
func (ls *LiveStreamer) runSession(ctx context.Context, s *liveSession) {
	// close(s.done) は必ず最後（他の全ての後片付けの後）に行う。stop() は
	// `<-s.done` が閉じたら「片付け完了」とみなして戻るので、途中の状態
	// （map から消す前・ディレクトリを消す前）で閉じると、呼び出し側が
	// 「もう消えている」つもりで見に行った os.Stat がまだ古いディレクトリを
	// 見つけてしまう（実際にテストで踏んだ競合）。
	defer close(s.done)
	defer func() {
		ls.mu.Lock()
		// idle GC が先にこの id を削除して新しいセッションに入れ替えていたら、
		// 新しいセッションを消さない（cur == s のときだけ削除）。
		if cur, ok := ls.sessions[s.serviceID]; ok && cur == s {
			delete(ls.sessions, s.serviceID)
		}
		ls.mu.Unlock()
		if s.dir != "" {
			if err := os.RemoveAll(s.dir); err != nil {
				slog.Warn("streamer: live segment cleanup failed", "service_id", s.serviceID, "dir", s.dir, "err", err)
			}
		}
		metrics.LiveActiveSessions.Set(float64(ls.sessionCount()))
	}()

	dir := filepath.Join(ls.cfg.SegmentDir, ls.site, strconv.FormatInt(s.serviceID, 10))
	if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
		s.startErr = fmt.Errorf("creating live segment dir: %w", err)
		metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
		close(s.ready)
		return
	}
	s.dir = dir

	body, err := ls.mirakc.StreamService(ctx, s.serviceID, ls.cfg.TunerPriority)
	if err != nil {
		s.startErr = &liveUpstreamStartError{err: err}
		metrics.LiveSessionStartFailures.WithLabelValues("upstream_error").Inc()
		slog.Error("streamer: requesting mirakc live stream", "service_id", s.serviceID, "err", err)
		close(s.ready)
		return
	}
	defer func() { _ = body.Close() }()

	input := io.Reader(body)
	captionInput := false
	if ls.cfg.Captions {
		// upstream の先頭を同期に読んで ffprobe に渡す。読んだバイトは
		// io.MultiReader で ffmpeg に戻すため 1 バイトも失わない。
		//
		// **専用の goroutine/バッファは持たない（B の決定）。** runSession は
		// 既に go で非同期に起動されており（getOrCreateSession）、待っている
		// クライアントは playlistStartupTimeout（15s）で打ち切られるので、
		// ここだけ独自の replay バッファ・timeout を持つ理由が無い。512 KiB は
		// 地上波 HD で概ね 0.3 秒（未検証。ビットレート依存の見積もり）。
		//
		// **ctx キャンセル時に body.Read が抜けるか**: StreamService は
		// http.NewRequestWithContext(ctx, ...) でリクエストを組み立てており、
		// net/http の契約上 ctx はレスポンスボディの読み取りまで含めて有効
		// （ctx が Done になると進行中の Read はエラーで返る）。sessionCtx が
		// stop() で cancel されたときも body.Read はブロックし続けない ---
		// 追加の goroutine は不要（internal/mirakc/client.go の StreamService
		// を読んで確認）。
		var prefix []byte
		var readErr error
		input, prefix, readErr = readLiveCaptionPrefix(body)
		if ctx.Err() != nil {
			s.startErr = ctx.Err()
			close(s.ready)
			return
		}
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
			slog.Warn("streamer: reading live probe prefix failed; continuing without captions",
				"service_id", s.serviceID, "err", readErr)
		}
		var probeErr error
		captionInput, probeErr = probeLiveCaptionStream(ctx, ls.cfg.FFprobe, prefix)
		if probeErr != nil {
			slog.Warn("streamer: probing live subtitle streams failed; continuing without captions",
				"service_id", s.serviceID, "err", probeErr)
			captionInput = false
		}
	}
	args := BuildLiveFFmpegArgs(ls.cfg, dir, captionInput)
	cmd := exec.CommandContext(ctx, ls.cfg.FFmpeg, args...)
	cmd.Stdin = input
	stderr := newCappedWriter(stderrCap)
	cmd.Stderr = stderr
	// ctx がキャンセルされてプロセスを kill した後、I/O をコピーするゴルーチン
	// （cmd.Stdin 用の内部パイプ）が終わるまで Wait は最大この時間だけ待つ。
	// ffmpeg が孫プロセスを fork していて標準入出力の fd を握ったまま残ると
	// （通常は起きないが）、Wait が無期限にブロックしうる。stop() は
	// idle GC / shutdown から呼ばれるので、ここが詰まるとチューナー解放も
	// 詰まる --- 上限を設けて必ず前に進めるようにする。
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		s.startErr = fmt.Errorf("starting live ffmpeg: %w", err)
		metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
		close(s.ready)
		return
	}

	slog.Info("streamer: live session started", "service_id", s.serviceID, "dir", dir,
		"profiles", len(ls.cfg.Profiles))
	close(s.ready)

	waitErr := cmd.Wait()
	if waitErr != nil && ctx.Err() == nil {
		if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			// ffmpeg 自体は exit 0 で完走したが、孫プロセスが stdin/stderr の
			// fd を握ったままで WaitDelay が先に切れた（internal/worker の
			// runEncode / commandOutput と同型のハングの exit 0 版）。正常な
			// セッション終了なので運用者向けの Error にはしない。
			slog.Warn("streamer: live ffmpeg exited successfully but WaitDelay expired before I/O completed",
				"service_id", s.serviceID, "wait_delay", cmd.WaitDelay)
		} else {
			// ctx.Err() == nil ということは idle GC / shutdown による意図した kill ではない
			// ---ffmpeg 自身が落ちた（mirakc 側の切断、コーデックエラー等）。
			slog.Error("streamer: live ffmpeg exited unexpectedly",
				"service_id", s.serviceID, "err", waitErr, "stderr", strings.TrimSpace(stderr.String()))
		}
	}
}

// reapIdle は idle timeout を超えたセッションを止める。「クライアント 1 人ごとの
// 生存」ではなく**サービス単位**（docs/api.md §ライブ視聴の HLS）。
//
// **パスの完走を `LiveIdleGCLastPass` に必ず記録する**（何も回収しなかった場合を
// 含む）。docs/operations.md の「ゲージには最後に成功した時刻を対で持つ」規律
// ---LiveActiveSessions だけでは、idle GC ループ自体が死んでいて「セッション数が
// 変わっていない」のか「本当に GC 対象が無かった」のかを区別できない
// （レビューで指摘。issue #91 の受け入れ条件）。
func (ls *LiveStreamer) reapIdle() {
	ls.reapIdleAt(time.Now())
}

// reapIdleAt は reapIdle の本体。「いま」を引数で受けるのは、離脱ヒントで詰めた
// 期限の前後（now+猶予 の直前と直後）をテストが実時間を待たずに踏むため。
func (ls *LiveStreamer) reapIdleAt(now time.Time) {
	defer metrics.LiveIdleGCLastPass.SetToCurrentTime()

	ls.mu.Lock()
	var idle []*liveSession
	for id, s := range ls.sessions {
		if s.idleSince(now) >= ls.cfg.IdleTimeout {
			idle = append(idle, s)
			// 即座にマップから外す。新しい要求が stop() の完了を待たずに
			// 別のセッションを起こせるようにする。
			delete(ls.sessions, id)
		}
	}
	ls.mu.Unlock()

	if len(idle) == 0 {
		return
	}

	// 並行に stop() する。直列だと 1 本の ffmpeg が kill に応答しない（ハング
	// した子プロセス等）と、他の回収可能なセッションまで足止めされる。
	var wg sync.WaitGroup
	for _, s := range idle {
		wg.Add(1)
		go func(s *liveSession) {
			defer wg.Done()
			slog.Info("streamer: live session idle, stopping", "service_id", s.serviceID)
			s.stop()
			metrics.LiveIdleGCReclaimed.Inc()
		}(s)
	}
	wg.Wait()

	metrics.LiveActiveSessions.Set(float64(ls.sessionCount()))
}

// shutdown はプロセス停止時に呼ぶ。新規セッションの受付を止め、既存の全セッションを
// 止めて mirakc の接続を閉じる（チューナー解放）。
//
// reapIdle と同じ理由で並行に stop() する（1 本が詰まっても他のチューナー解放を
// 遅らせない。SIGTERM の drain 猶予は有限）。
func (ls *LiveStreamer) shutdown() {
	ls.mu.Lock()
	ls.closed = true
	sessions := make([]*liveSession, 0, len(ls.sessions))
	for _, s := range ls.sessions {
		sessions = append(sessions, s)
	}
	ls.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *liveSession) {
			defer wg.Done()
			s.stop()
		}(s)
	}
	wg.Wait()
}

// stderrCap は ffmpeg の stderr から保持する末尾バイト数。encode.go の
// strings.Builder と違い、ライブの ffmpeg はセッションの生存中（数時間〜）ずっと
// 動くため、無制限バッファはエラーが出続けるとメモリを消費し続ける
// （レビューで指摘）。診断に十分な量だけ末尾を保持する。
const stderrCap = 8 * 1024

// cappedWriter は末尾 max バイトだけを保持する io.Writer（スレッドセーフ）。
type cappedWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newCappedWriter(max int) *cappedWriter {
	return &cappedWriter{max: max}
}

// Write は io.Writer を満たす。常に (len(p), nil) を返す（バッファへの追記は
// 失敗しない）。
func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// String は現在保持している内容を返す。
func (w *cappedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// BuildLiveFFmpegArgs は設定済みの全プロファイルを 1 回の ffmpeg 起動で HLS に
// 出す引数を組み立てる（issue #91 の決定 1: 1 チューナーから複数プロファイル）。
//
// 自由形式の cmd 文字列は受け取らない（encode.BuildFFmpegArgs と同じ方針）。
// ストリームは先頭の映像・先頭の音声だけを map する（単一映像・単一音声の放送
// サービス前提の MVP。複数音声・データ放送は特別扱いしない）。
// 字幕は通常は map しない。Captions=true の専用経路だけ optional に ARIB caption
// を map し、libaribcaption で WebVTT にする。既定経路は Debian 系の ffmpeg が
// arib_caption デコーダを持たない構成でも従来どおり動く。
//
// argv の順序（issue #321 決定コメント §3）:
//
//	-hide_banner -nostats -loglevel error
//	[cfg.HWAccel ブロック]                          # 入力 1 本ぶん、1 回だけ
//	-probesize 5M -analyzeduration 3M
//	[cfg.InputExtraArgs…]
//	-f mpegts -i pipe:0
//	  ── プロファイルごとに繰り返し ──
//	  -map 0:v:0 -map 0:a:0  -c:v  -c:a
//	  [-vf <scaler>]  [-crf|-qp]  [-preset]
//	  -force_key_frames expr:…
//	  [profile.extra_args…]                         # ユーザー（出力側）
//	  -f hls ... OUT.m3u8                            # アプリ所有の末尾
//
// **Captions=true のときは 1 つの master playlist（%v 展開）を出す形に分岐する。**
// withSubtitles は Captions=true のときだけ効き、起動前の ffprobe 判定結果を渡す
// （false なら字幕 map / rendition を完全に省き、字幕の無い番組でも映像・音声の
// HLS を継続できる）。Captions=false のときは無視される。
func BuildLiveFFmpegArgs(cfg LiveConfig, dir string, withSubtitles bool) []string {
	if cfg.Captions {
		return buildLiveCaptionFFmpegArgs(cfg, dir, withSubtitles)
	}
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "error",
	}
	args = append(args, cfg.HWAccel.Args()...)
	args = append(args,
		// pipe の MPEG-TS は PAT/PMT が揃うまで寸法 0x0 に見える窓がある。
		// 既定 probesize だと誤判定しやすいので少し延ばす。playlistStartupTimeout
		// （15s）を食いつぶさないよう、analyzeduration は数秒に留める。
		"-probesize", "5M",
		"-analyzeduration", "3M",
	)
	args = append(args, cfg.InputExtraArgs...)
	args = append(args, "-f", "mpegts", "-i", "pipe:0")
	for _, p := range cfg.Profiles {
		// 映像・音声だけ。字幕 / データ放送は捨てる（上記 arib_caption）。
		// -map は output 単位のオプションなので、ループの前に 1 組だけ置くと
		// 最初の .m3u8 にしか適用されず、2 本目以降は自動ストリーム選択に戻る。
		args = append(args, "-map", "0:v:0", "-map", "0:a:0")
		args = append(args, "-c:v", p.VideoCodec, "-c:a", p.AudioCodec)
		if filter, ok := ffargs.ScaleArgs(p.Scaler, p.Height); ok {
			args = append(args, "-vf", filter)
		}
		args = append(args, ffargs.QualityArgs(p.CRF, p.QP)...)
		if p.Preset != "" {
			args = append(args, "-preset", p.Preset)
		}
		// キーフレームをセグメント境界に合わせる。合わせないと HLS のセグメント
		// カットが GOP 境界を無視し、再生開始位置がずれる/コマ落ちする。
		args = append(args, "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", p.SegmentSeconds))
		if len(p.ExtraArgs) > 0 {
			args = append(args, p.ExtraArgs...)
		}
		args = append(args,
			"-f", "hls",
			"-hls_time", strconv.Itoa(p.SegmentSeconds),
			"-hls_list_size", strconv.Itoa(p.PlaylistSize),
			// delete_segments: プレイリスト長を超えた古いセグメントを削除する
			//（プロセスが落ちても残骸を溜め続けない。正常系の掃除）。
			// temp_file: 一時ファイルに書いてから rename するので、配信側が
			// 書き込み途中のファイルを読むことがない。
			"-hls_flags", "delete_segments+temp_file",
			"-hls_segment_filename", filepath.Join(dir, "segments", p.Name+"_seg%05d.ts"),
			// hls_base_url: プレイリストの各セグメント行に付ける接頭辞。
			// **これが無いと ffmpeg は basename だけを書く**（実機で確認済み）。
			// HLS クライアントはプレイリスト自身の URL 基準で相対解決するため、
			// basename のままだと `.../live/h264_seg00001.ts` を要求してしまい、
			// このサーバーが実際に配信するルート（`.../live/segments/{name}`）と
			// 食い違って 404 になる。`-hls_segment_filename` が書き込む物理パス
			// （`segments/` サブディレクトリ）と、プレイリストが指す論理 URI を
			// 一致させるための必須フラグ（issue #91 のレビューで発見）。
			"-hls_base_url", "segments/",
			filepath.Join(dir, p.Name+".m3u8"),
		)
	}
	return args
}

// buildLiveCaptionFFmpegArgs は HLS を 1 つの master playlist として出力する。
// %v はプロファイルごとの video/audio variant を表す。withSubtitles は起動前の
// ffprobe 判定結果で、false の場合は字幕 map / rendition を完全に省き、字幕なし
// 番組でも映像・音声の HLS を継続できる。
//
// **per-stream 指定子は必ず型付き（`:v:N` / `:a:N`）にする。** この経路の出力
// ストリーム順は v0, a0, [s0,] v1, a1 で、`-preset:N` / `-vf:N`（型無しの
// グローバル出力ストリーム index）は 2 本目以降のプロファイルでは音声側を指して
// しまい、preset もフィルタも掛からない（実 ffmpeg で測定・固定: レビュー指摘）。
//
// **フィルタは `-filter:v:N`（`-vf` の完全形）にする。**`-vf:v:N`（`-vf` に型
// 付き specifier を重ねる書き方）は ffmpeg 9.0.1 で単一出力・複数 video map の
// 構成において機能しない（specifier が意図通り分離されず、最後に指定した
// フィルタが両方の video ストリームに適用されて警告が出ることを実測で確認。
// `-c:v:N` や `-preset:v:N` のような型を伴わない他オプションでの `:v:N` 付与は
// 問題なく機能する --- `-vf`/`-filter:v` だけの挙動）。
func buildLiveCaptionFFmpegArgs(cfg LiveConfig, dir string, withSubtitles bool) []string {
	args := []string{"-hide_banner", "-nostats", "-loglevel", "error"}
	args = append(args, cfg.HWAccel.Args()...)
	args = append(args, "-probesize", "5M", "-analyzeduration", "3M")
	args = append(args, cfg.InputExtraArgs...)
	if withSubtitles {
		// **ARIB 字幕は duration を持たない。** これが無いと WebVTT の終了時刻が
		// 全 cue で約 1193 時間になり、字幕が一度出たら消えず積み重なる（実測:
		// NHK Eテレの実 TS で `00:21.605 --> 1193:03:08.900`）。
		// 入力側オプションなので -i より前に置く。
		args = append(args, "-fix_sub_duration")
	}
	args = append(args, "-f", "mpegts", "-i", "pipe:0")

	var variants []string
	for i, p := range cfg.Profiles {
		args = append(args, "-map", "0:v:0", "-map", "0:a:0")
		if i == 0 && withSubtitles {
			args = append(args, "-map", "0:s:0?")
		}
		args = append(args, "-c:v:"+strconv.Itoa(i), p.VideoCodec, "-c:a:"+strconv.Itoa(i), p.AudioCodec)
		if filter, ok := ffargs.ScaleArgs(p.Scaler, p.Height); ok {
			args = append(args, "-filter:v:"+strconv.Itoa(i), filter)
		}
		args = append(args, ffargs.QualityArgs(p.CRF, p.QP)...)
		if p.Preset != "" {
			args = append(args, "-preset:v:"+strconv.Itoa(i), p.Preset)
		}
		args = append(args, "-force_key_frames:v:"+strconv.Itoa(i), fmt.Sprintf("expr:gte(t,n_forced*%d)", p.SegmentSeconds))
		if i == 0 && withSubtitles {
			// -fix_sub_duration だけだと「次の字幕が来るまで現在の cue を出さない」
			// ので、ライブでは画面に出ている字幕がセグメントに載らない（実測:
			// 同じ 30 秒で cue 5 本 → 4 本に減る）。heartbeat を映像 variant 0 に
			// 付けると random access point で cue を分割して吐くため、途中参加した
			// 視聴者にも現在の字幕が届く（実測: 同じ 30 秒で 8 本、セグメント境界で
			// 分割される）。値を取らないフラグである。
			args = append(args, "-fix_sub_duration_heartbeat:v:0")
		}
		args = append(args, p.ExtraArgs...)
		mapping := fmt.Sprintf("v:%d,a:%d", i, i)
		if i == 0 && withSubtitles {
			mapping += ",s:0,sgroup:subs"
		}
		variants = append(variants, mapping)
	}
	args = append(args,
		"-var_stream_map", strings.Join(variants, " "),
		"-master_pl_name", "playlist.m3u8",
		"-f", "hls",
		"-hls_time", strconv.Itoa(cfg.Profiles[0].SegmentSeconds),
		"-hls_list_size", strconv.Itoa(cfg.Profiles[0].PlaylistSize),
		"-hls_flags", "delete_segments+temp_file",
		"-hls_base_url", "segments/",
		"-hls_segment_filename", filepath.Join(dir, "segments", "%v_seg%05d.ts"),
	)
	if withSubtitles {
		// hls_subtitle_path は字幕プレイリストのファイルパスであり、VTT
		// セグメント自体は muxer が通常の出力ディレクトリ（dir）へ書く。
		// master からの相対 URI は segments/ を付けるため、配信側では
		// .m3u8/.vtt を dir 直下から読む。
		//
		// **%v が要る。** variant が 2 本以上あると ffmpeg は
		// `-hls_subtitle_path` にも %v（またはサブディレクトリでの %v）を
		// 要求し、無いと `hls` マルチプレクサの初期化自体に失敗して
		// **HLS 出力を一切書かずに終了する**（実測: `More than 1 variant
		// streams are present, %v is expected...` で exit 234。字幕付き
		// ライブは複数プロファイルが既定の構成であり、%v を欠くと captions
		// 有効化そのものが機能しなくなる致命的な回帰だったため、G の一部として
		// ここで直す）。字幕 rendition は 1 本しか無い（variant 0 の s:0 だけを
		// map している）ので、実際に作られる字幕 playlist は `subtitles_0.m3u8`
		// 1 本だけで、他の variant 分のファイルは作られない（実測: 2 プロファイル
		// で `ls` したところ subtitles_0.m3u8 のみ）。
		//
		// **`sgroup:subs` を全 variant に付けてはならない。** ffmpeg 9.0.1 は
		// SIGSEGV で落ちる（実測: exit 139、master が .tmp のまま残る）。
		// variant 0 だけに付けても master の EXT-X-STREAM-INF は**全 variant**に
		// `SUBTITLES="subs"` を付けるので、プロファイルを切り替えても字幕
		// rendition は失われない（実測: 2/3 プロファイルで確認）。
		args = append(args, "-c:s", "webvtt", "-hls_subtitle_path", filepath.Join(dir, "subtitles_%v.m3u8"))
	}
	args = append(args, filepath.Join(dir, "playlist_%v.m3u8"))
	return args
}

// readLiveCaptionPrefix は body の先頭を liveCaptionProbeBytes だけ同期に読み、
// 読んだバイトを 1 つも失わずに ffmpeg へ戻す io.Reader（読んだ prefix +
// 残りの body）を組み立てる。upstream が liveCaptionProbeBytes に満たない
// （io.ReadFull が io.ErrUnexpectedEOF/io.EOF を返す）場合は読めた分だけを
// prefix にする --- 呼び出し側（runSession）が err を見て継続可否を判定する。
func readLiveCaptionPrefix(body io.Reader) (input io.Reader, prefix []byte, err error) {
	prefix = make([]byte, liveCaptionProbeBytes)
	n, err := io.ReadFull(body, prefix)
	prefix = prefix[:n]
	return io.MultiReader(bytes.NewReader(prefix), body), prefix, err
}

// probeLiveCaptionStream は ffprobe に MPEG-TS の有限な先頭部分だけを渡し、字幕
// ストリームの有無を調べる。アプリケーション自身は TS/PES を解釈しない。
func probeLiveCaptionStream(ctx context.Context, ffprobe string, prefix []byte) (bool, error) {
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	probeCtx, cancel := context.WithTimeout(ctx, liveCaptionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, ffprobe,
		"-v", "error", "-probesize", "5M", "-analyzeduration", "3M",
		"-select_streams", "s", "-show_entries", "stream=index", "-of", "csv=p=0", "-i", "pipe:0",
	)
	cmd.Stdin = bytes.NewReader(prefix)
	out, err := cmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return false, probeCtx.Err()
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}
