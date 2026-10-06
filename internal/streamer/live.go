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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/mediapath"
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

	// MediaDir は site streamer が参照する録画原本の root。原本 VOD はここから
	// read-only で開き、HLS 出力は SegmentDir にだけ書く（マウントも read-only でよい）。
	MediaDir string

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

	// InputExtraArgs は `-f mpegts [-ss N] -i ...` の直前に追加する引数。
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

	// Deinterlace はインターレース解除を有効にする系統スイッチ。filter の実体は
	// Scaler から導出する（software は yadif、vaapi は deinterlace_vaapi）。
	Deinterlace bool

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

// mirakcRecordClient is kept separate from mirakcLiveClient so existing live-only
// test doubles do not need to implement the recording endpoint. The production
// *mirakc.Client implements both interfaces.
type mirakcRecordClient interface {
	StreamRecordFollow(ctx context.Context, recordID string) (io.ReadCloser, error)
}

// mirakcSeekRecordClient is the additional mirakc surface required when a
// chase session starts after the recording head. StreamRecord returns a finite
// Range response; the streamer requests the next Range as it catches up.
type mirakcSeekRecordClient interface {
	mirakcRecordClient
	StreamRecord(ctx context.Context, recordID string, offset int64) (io.ReadCloser, int64, error)
	GetRecord(ctx context.Context, recordID string) (*mirakc.Record, error)
}

var (
	// errSessionLimit はプロセスローカルな同時セッション上限に達したことを示す。
	// **プロセスローカル**であり、グローバルな天井（チューナー数、mirakc が裁定）
	// ではない（docs/operations.md §5）。
	errSessionLimit = errors.New("live session limit reached (process-local)")
	// errChaseInputCoolingDown は同じ録画の追っかけ入力失敗後、再作成を一時停止している。
	errChaseInputCoolingDown = errors.New("chase input failure is cooling down")
	// errShuttingDown は Run の ctx が既に完了し、新規セッションを受け付けないことを示す。
	errShuttingDown = errors.New("streamer is shutting down")
	// errStartupTimeout は getOrCreateSession の `<-s.ready` 待ちが
	// playlistStartupTimeout を超えたことを示す（issue #286）。mirakc への接続
	// （StreamService、全体タイムアウト無し）がハングすると close(s.ready) に
	// 到達しないため、ハンドラ側の待ちだけを打ち切る。**セッションの起動 goroutine
	// （sessionCtx 由来の runSession）やセッション自体の状態には影響しない**
	// --- 諦めるのはこの呼び出しの待ちだけで、セッションはそのまま起動を続ける。
	errStartupTimeout = errors.New("live session did not become ready in time")
	// errChaseRecordNotReadyTimeout is deliberately not a liveUpstreamStartError:
	// repeated 204 responses are a normal recording-start state, not a failed tuner
	// connection. It becomes a 503 only after the shared startup budget expires.
	errChaseRecordNotReadyTimeout = errors.New("chase record did not become readable in time")
	// errChaseOffsetUnavailable means the requested recording-relative second is
	// not in the currently available recording range. It is translated to 416
	// by the HTTP handler, rather than starting a session at an invalid byte.
	errChaseOffsetUnavailable = errors.New("chase offset is outside the available recording range")
	// errOriginalVODOffsetUnavailable means the requested second is at or beyond
	// the completed original's measured duration. It becomes HTTP 416.
	errOriginalVODOffsetUnavailable = errors.New("original VOD offset is outside the recording range")
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
	// liveStreamProbeBytes は live MPEG-TS の先頭を ffprobe に渡すサイズ。
	// PAT/PMT は入力の先頭付近に現れる。読み取ったバイトは必ず ffmpeg に戻す
	// （runSession が prefix replay reader で先頭へ戻す）。地上波 HD で
	// 概ね 0.3 秒ぶんの読み取り（未検証。ビットレートに依存する見積もり）。
	liveStreamProbeBytes = 512 * 1024
	// liveStreamProbeWait は prefix の先読みを待つ上限。録画追従 source がまだ
	// データを出せないときに playlist の起動まで塞がない。
	liveStreamProbeWait = time.Second
	// liveStreamProbeTimeout は probeLiveStreamInfo（ffprobe 起動）の上限。
	// prefix の先読みは liveStreamProbeWait が別に制限する。
	liveStreamProbeTimeout = 5 * time.Second
)

// liveMirakcReleaseWait は、退避したセッションの mirakc 接続を Close して
// `<-s.done` を待った後、1 回だけ再試行する前の待ち時間。実 mirakc
// 4.0.0-dev.0 + fixture tuner 2 本 + 録画 1 本で、旧ライブの Close から次の
// 異なる波のライブ要求が通るまでを測ったところ 2.35〜4.18 秒だった（2026-09-06、
// internal/mirakc/conformance/live_release_test.go）。5 秒にして、mirakc 側の
// 非同期な tuner プロセス終了の揺れを吸収する。ここは再試行の回数を増やすための
// backoff ではなく、退避後に 1 回だけ行う解放待ちである。
//
// **ポーリングではなく固定待ちにしているのは単純さを取った選択。** 典型的な解放は
// 2.35 秒で終わる（上記実測の最小値）が、固定 5 秒はその典型ケースで最大 2.6 秒を
// 余分に払う。予算内で 100ms 間隔のポーリングに変える案もあるが、この PR の範囲
// （issue #677 の「再試行は 1 回だけ」---反復禁止であって解放待ちの実装方式では
// ない）を広げない。判定手段は internal/mirakc/conformance/live_release_test.go
// に既にある（100ms ポーリングで解放を検出している）ので、ポーリング化するときは
// そこを使って測り直す。
//
// var にしてあるのはテストからの上書き用（playlistStartupTimeout と同じ理由 ---
// 5 秒の実待ちはテストを不必要に遅くする）。運用者向けの設定キーではない。
var liveMirakcReleaseWait = 5 * time.Second

// LiveStreamer はライブ視聴の HLS ルートを配信する。
//
// 1 サービス = 1 セッション = 1 ffmpeg プロセス = mirakc の 1 チューナー。同じ
// サービスを複数クライアントが見ても共有する。ライブのセッションキーは
// `sessions map[int64]*liveSession`、追っかけは録画 ID と開始オフセットを含む。
// **これらが site を含まないのは、この LiveStreamer 自身が
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
	pool   *pgxpool.Pool

	mu            sync.Mutex
	sessions      map[int64]*liveSession
	chaseSessions map[sessionKey]*liveSession
	// failedChaseInputs は入力エラーで終わった追っかけ録画 ID と再試行可能時刻。
	// offset を含めないのは、フロントの再選択が新しい offset で来るため。
	failedChaseInputs map[int64]time.Time

	// afterEvictRelease はテスト専用: 退避を終えて evictMu を放した直後に呼ぶ。
	afterEvictRelease func()

	// afterOriginalVODOpen はテスト専用: 原本を open した直後、DB を再確認する前に呼ぶ。
	afterOriginalVODOpen func()
	closed               bool

	// evictMu は退避（takeIdleSessionForRetry → stop → 解放待ち）を直列化する。
	// getOrCreateSession の doc コメント参照 --- 「1 つの圧力イベントに対して退避は
	// 1 本」を、呼び出しごとの不変条件ではなく LiveStreamer 全体の不変条件にする
	// ためのロック。**保持区間に getOrCreateSessionOnce（最大 playlistStartupTimeout
	// の起動待ち）を含めない** --- 含めると別サービスの無関係な起動待ちまでこの
	// ロックで直列化されてしまう。
	evictMu sync.Mutex
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
	return newLiveStreamerWithPool(nil, mirakcClient, site, cfg)
}

func newLiveStreamer(client mirakcLiveClient, cfg LiveConfig) *LiveStreamer {
	return newLiveStreamerWithPool(nil, client, "default", cfg)
}

// NewLiveWithPool is the production constructor. Live and chase sessions share
// the same LiveStreamer so their process-local cap, idle GC, leave semantics,
// and active-session gauge describe one resource pool.
func NewLiveWithPool(pool *pgxpool.Pool, mirakcClient *mirakc.Client, site string, cfg LiveConfig) *LiveStreamer {
	return newLiveStreamerWithPool(pool, mirakcClient, site, cfg)
}

func newLiveStreamerWithPool(pool *pgxpool.Pool, client mirakcLiveClient, site string, cfg LiveConfig) *LiveStreamer {
	if cfg.Enabled && cfg.SegmentDir != "" {
		sweepStaleLiveSegments(cfg.SegmentDir)
	}
	return &LiveStreamer{
		mirakc:            client,
		site:              site,
		cfg:               cfg,
		pool:              pool,
		sessions:          make(map[int64]*liveSession),
		chaseSessions:     make(map[sessionKey]*liveSession),
		failedChaseInputs: make(map[int64]time.Time),
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

// ChaseRoutePattern は録画中の追っかけ再生の固定深さパターン。site は前段の
// site ごとの Service へ振り分けるために URL に含め、mirakc record id は DB から
// 解決する。OpenAPI には載せず、streamer がバイナリとして登録する。
const ChaseRoutePattern = "/api/sites/{site}/recordings/{id}/chase"

// OriginalVODRoutePattern は完成済み録画の原本を HLS 化する固定深さパターン。
// site streamer が recordings.id でセッションを共有し、クエリの profile は
// 出力 playlist だけを選ぶ。
const OriginalVODRoutePattern = "/api/sites/{site}/recordings/{id}/original-vod"

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
	// ffmpeg の variant playlist は master と同じディレクトリに置かれる。
	// master の相対 URI（`h264.0.m3u8` / `playlist_0.m3u8`）をそのまま解決できるようにする。
	r.Get(LiveRoutePattern+"/{name}", ls.Segment)
	r.Post(LiveRoutePattern+"/leave", ls.Leave)

	// 追っかけ再生も同じ LiveStreamer のセッションプールを使う。captions の
	// 設定は site ごとに異なり得るので、variant playlist の固定深さルートは
	// liveSites と同様に常に登録し、実際の可否は Segment 側で判定する。
	r.Get(ChaseRoutePattern+"/playlist.m3u8", ls.ChasePlaylist)
	r.Get(ChaseRoutePattern+"/offset/{offset}/playlist.m3u8", ls.ChasePlaylist)
	r.Get(ChaseRoutePattern+"/segments/{name}", ls.ChaseSegment)
	r.Get(ChaseRoutePattern+"/offset/{offset}/segments/{name}", ls.ChaseSegment)
	r.Get(ChaseRoutePattern+"/{name}", ls.ChaseSegment)
	r.Get(ChaseRoutePattern+"/offset/{offset}/{name}", ls.ChaseSegment)
	r.Post(ChaseRoutePattern+"/leave", ls.ChaseLeave)
	r.Post(ChaseRoutePattern+"/offset/{offset}/leave", ls.ChaseLeave)

	// 完了済み原本 VOD も live / chase と同じ process-local session pool を使う。
	r.Get(OriginalVODRoutePattern+"/playlist.m3u8", ls.OriginalVODPlaylist)
	r.Get(OriginalVODRoutePattern+"/offset/{offset}/playlist.m3u8", ls.OriginalVODPlaylist)
	r.Get(OriginalVODRoutePattern+"/segments/{name}", ls.OriginalVODSegment)
	r.Get(OriginalVODRoutePattern+"/offset/{offset}/segments/{name}", ls.OriginalVODSegment)
	r.Get(OriginalVODRoutePattern+"/{name}", ls.OriginalVODSegment)
	r.Get(OriginalVODRoutePattern+"/offset/{offset}/{name}", ls.OriginalVODSegment)
	r.Post(OriginalVODRoutePattern+"/leave", ls.OriginalVODLeave)
	r.Post(OriginalVODRoutePattern+"/offset/{offset}/leave", ls.OriginalVODLeave)
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
//   - getOrCreateSessionOnce の既存セッション経路の `<-s.ready` 待ち（Playlist が呼ぶ。issue #286）
//   - getOrCreateSessionOnce の新規作成経路の `<-s.ready` 待ち（同上。issue #286 --- この
//     2 経路は互いに独立したコードパスであり、片方だけ直すと非対称が残る。
//     実際にレビューで新規作成経路側だけテストが無いまま気付かれず、指摘された）
//   - Segment の `<-s.ready` 待ち（issue #189）
//   - waitForPlaylist（Playlist、ready になった後のプレイリストファイル出現待ち）
//
// **4 箇所が同じ 1 つの変数を参照することが本質。** 分けると、どれか 1 つだけ
// 直したときに非対称が残っても気付けない --- 実際に #189 で Segment だけ
// 直したときに Playlist 側（getOrCreateSessionOnce）の非対称が見過ごされ、
// レビューで #286 として指摘された。新しい待ちを足すときもここを増やさず
// この変数を再利用すること。**getOrCreateSession の退避 → 再試行経路は、上の
// 2 つ（getOrCreateSessionOnce の既存/新規セッション経路）を同じ呼び出しの中で
// 2 回通る**（1 回目の起動失敗判定と、退避後の再試行）--- 新しい select を
// 足すのではなく、同じ getOrCreateSessionOnce をもう一度呼ぶ形でこの変数を
// 再利用している（issue #677）。
//
// **Playlist ハンドラ 1 本の最悪応答時間はこの値の 1 回分でも 2 回分でもない。**
// 退避を経由しない通常経路は、getOrCreateSessionOnce の起動待ち（最大この値）の
// 後に waitForPlaylist（さらに最大この値）が直列で走るため、両方が上限いっぱい
// まで掛かると合計は**この値の 2 倍**（既定なら 30s）になる。**上流拒否/上限
// 到達からの退避（getOrCreateSession）を経由する経路はさらに長い** ---
// 1 回目の getOrCreateSessionOnce（最大この値）→ 退避の解放待ち
// （liveMirakcReleaseWait、既定 5s）→ 退避後の再試行 getOrCreateSessionOnce
// （最大この値）→ waitForPlaylist（最大この値）が直列に並び、全区間が上限
// いっぱいまで掛かると合計は**この値の 3 倍 + liveMirakcReleaseWait**（既定なら
// 15s×3 + 5s = 50s）になる。Segment は getOrCreateSession を経由しない
// （getOrCreateSessionOnce を直接呼ばず、既存セッションの `<-s.ready` だけを
// 待つ）分、通常経路はこの値 1 回分（既定 15s）で済む。
//
// var にしてあるのはテストからの上書き用（15 秒の実待ちはテストを不必要に
// 遅くする）。運用者向けの設定キーではない。
var playlistStartupTimeout = 15 * time.Second

// chaseInputFailureCooldown は追っかけ入力の失敗後、同じ録画の全 offset で新しい
// セッションを作らない期間。再生位置の offset で張り直されても、入力障害中に
// ffmpeg と mirakc の要求を作り直し続けないようにする。
//
// 判定基準: フロントの再生中の失敗は、hls.js のプレイリスト再取得が規定の再試行
// （playlistLoadPolicy の errorRetry: maxNumRetry 2、1 秒→2 秒の backoff で計 3 秒前後）を
// 使い切って fatal になり、再生元を選び直した probe が 502 を受ける順で進む。この全体が
// cooldown に収まらないと、hls.js の再取得自体が先頭から作り直しを起こす。
// そのため再試行が尽きるまでの時間より長くとる。10 秒はその 3 倍強の余裕で、実測値ではない。
var chaseInputFailureCooldown = 10 * time.Second

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

	// どちらの経路でも master playlist を返す（音声レンディションを載せるため。
	// BuildLiveFFmpegArgs）。
	playlistName := profile.Name + ".m3u8"
	if ls.cfg.Captions {
		playlistName = "playlist.m3u8"
	}
	playlistPath := filepath.Join(s.dir, playlistName)
	content, ok := waitForPlaylist(r.Context(), s, playlistPath, playlistStartupTimeout, "#EXT-X-STREAM-INF")
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

// servesFile は name が配信対象の形かを返す。`.vtt` は字幕を出す構成だけが書く。
func (c LiveConfig) servesFile(name string) bool {
	return segmentNamePattern.MatchString(name) && (c.Captions || filepath.Ext(name) != ".vtt")
}

// sessionFilePath は name の実体の場所を返す。`.ts` は segments/ に、variant /
// 字幕 playlist と `.vtt` は master と同じ s.dir 直下に置かれる（BuildLiveFFmpegArgs）。
func sessionFilePath(dir, name string) string {
	if filepath.Ext(name) == ".ts" {
		return filepath.Join(dir, "segments", name)
	}
	return filepath.Join(dir, name)
}

// captionVariantPattern は captions 経路の variant / 字幕 playlist の名前
// （buildLiveCaptionFFmpegArgsForPlaylistType の `playlist_%v.m3u8` / `subtitles_%v.m3u8`）。
var captionVariantPattern = regexp.MustCompile(`^(?:playlist|subtitles)_[0-9]+\.m3u8$`)

// isVariantPlaylist は name がこの構成の ffmpeg が書く variant playlist の名前かを返す。
// captions 無効時は `NAME.<n>.m3u8`（NAME は設定済みのプロファイル名）。master
// （`NAME.m3u8` / `playlist.m3u8`）は含まない --- master は Playlist の担当である。
func (c LiveConfig) isVariantPlaylist(name string) bool {
	if c.Captions {
		return captionVariantPattern.MatchString(name)
	}
	base, ok := strings.CutSuffix(name, ".m3u8")
	if !ok {
		return false
	}
	profile, index, ok := strings.Cut(base, ".")
	if !ok || index == "" || strings.Trim(index, "0123456789") != "" {
		return false
	}
	for _, p := range c.Profiles {
		if p.Name == profile {
			return true
		}
	}
	return false
}

// serveVariantPlaylist は master が指す variant / 字幕 playlist を返す。
//
// **セッションが無ければ作る（Playlist と同じ getOrCreateSession）。** hls.js が
// 取り直し続けるのは master ではなく variant である（master は最初の 1 回だけ）。
// ここで 404 を返すと、idle GC・ffmpeg の異常終了・Pod の入れ替えの後、
// hls.js は 4xx を再試行しないので fatal になり止まる --- 自己修復は「クライアントが
// ポーリングする URL がセッションを作る入口でもある」ことに依存している
// （docs/api/media.md §資源同定）。variant 名にセッション ID は無いので、宛先は
// サービスのままである。知らない名前は 404 で、セッションを起こさない。
//
// **master と同じ readiness 待ちを variant にも掛ける。** master に
// EXT-X-STREAM-INF があることは、variant にセグメントが書かれていることを
// 保証しない。クライアントは master を受け取った直後に variant を取りに来る。
func (ls *LiveStreamer) serveVariantPlaylist(w http.ResponseWriter, r *http.Request, serviceID int64, name string) {
	if !ls.cfg.isVariantPlaylist(name) {
		http.NotFound(w, r)
		return
	}
	s, err := ls.getOrCreateSession(r.Context(), serviceID)
	if err != nil {
		if errors.Is(err, errStartupTimeout) {
			slog.Error("streamer: live variant playlist session did not become ready in time",
				"service_id", serviceID)
		}
		writeSessionError(w, err)
		return
	}
	s.touch()
	content, ok := waitForPlaylist(r.Context(), s, sessionFilePath(s.dir, name), playlistStartupTimeout, "#EXTINF")
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
}

// Segment は GET /api/sites/{site}/networks/{networkId}/services/{serviceId}/live/segments/{name}
// を処理する（variant playlist は serveVariantPlaylist へ渡す）。
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
	if !ls.cfg.servesFile(name) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(name, ".m3u8") {
		ls.serveVariantPlaylist(w, r, serviceID, name)
		return
	}

	ls.mu.Lock()
	s, ok := ls.sessions[serviceID]
	ls.mu.Unlock()
	if !ok {
		// idle GC で回収済み、または未開始。hls.js は variant playlist を再取得しにいき、
		// そこで新しいセッションが起きる（serveVariantPlaylist。レベルトリガーと同じ形。
		// セッション ID を持たないので「詰む」経路が無い。docs/api.md §ライブ視聴の HLS）。
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

	path := sessionFilePath(s.dir, name)

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

// ChaseTarget is the durable-to-mirakc mapping needed by a chase session.
// recordingID is the canonical recordings.id from the URL; RecordID is the
// opaque id accepted by mirakc and is never exposed in the public URL.
type ChaseTarget struct {
	RecordingID      int64
	Site             string
	RecordID         string
	Status           string
	RecordingStatus  string
	HasOriginalAsset bool
}

// LookupChaseTarget resolves a recording id without starting a session. It
// deliberately returns finished recordings too: a completed chase session
// keeps its EVENT playlist until idle GC, and the browser still needs to fetch
// that playlist after the recording status changes. ChasePlaylistForTarget
// decides whether a missing session may be started from the returned statuses.
func (ls *LiveStreamer) LookupChaseTarget(ctx context.Context, recordingID int64) (ChaseTarget, error) {
	if ls.pool == nil {
		return ChaseTarget{}, errors.New("chase target database is unavailable")
	}
	row, err := sqlcgen.New(ls.pool).GetChaseTarget(ctx, recordingID)
	if err != nil {
		return ChaseTarget{}, err
	}
	if row.DeletedAt != nil || row.Site == "" || row.RecordID == "" {
		return ChaseTarget{}, pgx.ErrNoRows
	}
	return ChaseTarget{
		RecordingID:      recordingID,
		Site:             row.Site,
		RecordID:         row.RecordID,
		Status:           row.Status,
		RecordingStatus:  row.RecordingStatus,
		HasOriginalAsset: row.HasOriginalAsset,
	}, nil
}

// canStartChaseSession reports whether mirakc still owns an uncommitted original
// that a new chase session may follow. A media_assets row blocks a new session
// regardless of state because even a deleted row proves ingest already committed.
func (target ChaseTarget) canStartChaseSession() bool {
	canFollowRecord := target.Status == "recording" || target.Status == "finished"
	canFollowRecording := target.RecordingStatus == "recording" || target.RecordingStatus == "finished"
	return canFollowRecord && canFollowRecording && !target.HasOriginalAsset
}

// lookupOriginalVODTarget returns an active original for a completed recording
// owned by this site. The recording id is the durable resource key; no session
// identifier is exposed in the URL or persisted in the database.
func (ls *LiveStreamer) lookupOriginalVODTarget(ctx context.Context, recordingID int64) (sqlcgen.GetOriginalVODTargetRow, error) {
	if ls.pool == nil {
		return sqlcgen.GetOriginalVODTargetRow{}, errors.New("original VOD database is unavailable")
	}
	return sqlcgen.New(ls.pool).GetOriginalVODTarget(ctx, sqlcgen.GetOriginalVODTargetParams{
		RecordingID: recordingID,
		Site:        ls.site,
	})
}

// originalVODSource opens the original read-only and only then rechecks the DB
// row (open-then-verify), without the rel_path lock.
//
// Why this holds the right inode without a lock: target was read as active, so its
// canonical file was renamed into place before its row committed, and the path
// keeps that inode until delete_reconcile unlinks it, which happens only after
// MarkMediaAssetDeleting has committed (deleteMediaAsset). media_assets_rel_path_idx
// forbids another live row at the same rel_path, so nobody else publishes there
// while target is live. If the row is still active after open, the path cannot have
// been unlinked before the open; if it is not, we return pgx.ErrNoRows. A later
// unlink cannot change the descriptor FFmpeg already holds.
func (ls *LiveStreamer) originalVODSource(recordingID, offsetSeconds int64, target sqlcgen.GetOriginalVODTargetRow) sessionSource {
	return func(ctx context.Context) (io.ReadCloser, error) {
		if ls.cfg.MediaDir == "" {
			return nil, errors.New("original VOD media directory is unavailable")
		}
		path, err := mediapath.Resolve(ls.cfg.MediaDir, target.RelPath)
		if err != nil {
			return nil, fmt.Errorf("resolving original media path: %w", err)
		}
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, pgx.ErrNoRows
			}
			return nil, fmt.Errorf("opening original media: %w", err)
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			if err != nil {
				return nil, fmt.Errorf("stating original media: %w", err)
			}
			return nil, pgx.ErrNoRows
		}
		if ls.afterOriginalVODOpen != nil {
			ls.afterOriginalVODOpen()
		}
		current, err := ls.lookupOriginalVODTarget(ctx, recordingID)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if current.ID != target.ID || current.RelPath != target.RelPath {
			_ = file.Close()
			return nil, pgx.ErrNoRows
		}
		if offsetSeconds > 0 {
			duration, err := probeOriginalVODDuration(ctx, ls.cfg.FFprobe, file)
			if err != nil {
				_ = file.Close()
				return nil, fmt.Errorf("probing original VOD duration: %w", err)
			}
			// 映像が残らない offset を通すと ffmpeg は何も出力せず、playlist 待ちの
			// 15 秒後に 504 になる（実バイナリで測定）。
			if float64(offsetSeconds) >= duration-originalVODTailMargin {
				_ = file.Close()
				return nil, errOriginalVODOffsetUnavailable
			}
		}
		return file, nil
	}
}

// originalVODFFmpegInputPath は ffmpeg / ffprobe が原本を開く名前。
//
// Cmd.ExtraFiles[0] が子の fd 3 になるので /dev/fd/3 でその記述子を開く。
// ffmpeg の `fd:` プロトコルを使わないのは、`-ss` の入力側シークに必要な
// 「ファイルとして開き直せる入力」を avformat に渡したいため（`fd:` は
// 記述子を直接読むので、seekable かどうかの判定が OS 任せになる。未検証）。
// /dev/fd/3 は記述子を保持する inode を指すので、DB 確認の後に canonical path が
// unlink されても読める（TestOriginalVODRetainedSessionSurvivesOriginalDeletion）。
const originalVODFFmpegInputPath = "/dev/fd/3"

// originalVODProbeTimeout は probeOriginalVODDuration（ffprobe 起動）の上限。
const originalVODProbeTimeout = 5 * time.Second

// originalVODTailMargin は映像の終端からこの秒数以内の offset を範囲外にする。
// 終端ちょうどの offset は最後のフレームより後ろを指して出力が空になりうる
// （合成 TS で offset 40 / 映像終端 40.01 が 15 秒待って 504 になった）。
const originalVODTailMargin = 0.5

// probeOriginalVODDuration は原本の「映像が存在する長さ」（録画先頭からの秒）を
// 開いた記述子越しに測る。offset がこの値以上なら映像が 1 フレームも残らない。
//
// format の duration は音声など最長のストリームで決まり、映像の終端より長い
// （合成 660 秒 TS: format 660.010 / 映像 660.000）。そのため映像ストリームの
// 終端（start_time + duration - format の start_time）を使う。ストリームの値が
// 取れなければ format の duration に落とす。
//
// ffprobe は継承した記述子の読み位置を共有して動かすので、返す前に先頭へ戻す
// （ffmpeg の入力側 -ss は先頭からの位置で測る。外すと偽 ffmpeg が fd 3 から 0 バイトしか読めず
// TestOriginalVODOffsetIdleGCRemovesScratch が落ちる）。
func probeOriginalVODDuration(ctx context.Context, ffprobe string, file *os.File) (float64, error) {
	ffprobe = ffargs.FFprobePath(ffprobe)
	probeCtx, cancel := context.WithTimeout(ctx, originalVODProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, ffprobe, ffargs.OriginalVODDurationProbeArgs(originalVODFFmpegInputPath)...)
	cmd.ExtraFiles = []*os.File{file}
	out, err := cmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return 0, probeCtx.Err()
		}
		return 0, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewinding probed original media: %w", err)
	}
	var probed struct {
		Format struct {
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probed); err != nil {
		return 0, fmt.Errorf("parsing original VOD probe output: %w", err)
	}
	valid := func(raw string) (float64, bool) {
		v, err := strconv.ParseFloat(raw, 64)
		return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	duration, ok := valid(probed.Format.Duration)
	if len(probed.Streams) > 0 {
		vStart, ok1 := valid(probed.Streams[0].StartTime)
		vDur, ok2 := valid(probed.Streams[0].Duration)
		fStart, ok3 := valid(probed.Format.StartTime)
		if ok1 && ok2 && ok3 {
			duration, ok = vStart+vDur-fStart, true
		}
	}
	if !ok || duration <= 0 {
		return 0, fmt.Errorf("invalid original VOD duration in %q", strings.TrimSpace(string(out)))
	}
	return duration, nil
}

// originalVODRecordingRemoved reports whether a user action (trash, purge,
// supersede) has removed the recording. Segment requests use only this: the
// retained session reads a held descriptor and scratch files, so an asset that
// became deleted (for example until_encoded) must not stop a playback in
// progress. A DB error other than ErrNoRows keeps serving the files.
func (ls *LiveStreamer) originalVODRecordingRemoved(ctx context.Context, recordingID int64) bool {
	if ls.pool == nil {
		return false
	}
	rec, err := sqlcgen.New(ls.pool).GetRecordingByID(ctx, recordingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	if err != nil {
		slog.Warn("streamer: original VOD recording check failed; serving retained files",
			"recording_id", recordingID, "err", err)
		return false
	}
	return rec.DeletedAt != nil || rec.PurgedAt != nil || rec.SupersededAt != nil
}

func chaseSessionKeyFor(recordingID, offsetSeconds int64) sessionKey {
	return sessionKey{
		kind:          chaseSessionKind,
		id:            recordingID,
		offsetSeconds: offsetSeconds,
	}
}

// parseCanonicalChaseOffset accepts the decimal seconds used in the chase URL.
// Keeping the spelling canonical prevents one requested position from creating
// multiple sessions or segment directories.
func parseCanonicalChaseOffset(raw string) (int64, bool) {
	if raw == "" {
		return 0, true
	}
	if len(raw) > 1 && raw[0] == '0' {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 || strconv.FormatInt(v, 10) != raw {
		return 0, false
	}
	return v, true
}

func chaseOffsetFromRequest(r *http.Request) (int64, bool) {
	return parseCanonicalChaseOffset(chi.URLParam(r, "offset"))
}

// ChasePlaylist handles the site-local form of the chase route. Only the
// playlist lookup touches the database; segments use the in-memory session.
func (ls *LiveStreamer) ChasePlaylist(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	target, err := ls.LookupChaseTarget(r.Context(), recordingID)
	if err != nil {
		writeChaseTargetError(w, r, err)
		return
	}
	if target.Site != ls.site {
		http.NotFound(w, r)
		return
	}
	ls.ChasePlaylistForTarget(w, r, target)
}

// ChasePlaylistForTarget starts or joins the one shared ffmpeg session for a
// recording and serves an EVENT playlist. The omitted offset starts at the
// recording head; a non-zero offset starts from the requested recording-relative
// second. Viewers with the same recording and offset share one session.
func (ls *LiveStreamer) ChasePlaylistForTarget(w http.ResponseWriter, r *http.Request, target ChaseTarget) {
	if target.Site != ls.site {
		http.NotFound(w, r)
		return
	}
	offsetSeconds, ok := chaseOffsetFromRequest(r)
	if !ok {
		http.Error(w, "invalid chase offset", http.StatusBadRequest)
		return
	}
	profile, ok := ls.cfg.profile(r.URL.Query().Get("profile"))
	if !ok {
		http.Error(w, "unknown chase profile", http.StatusBadRequest)
		return
	}
	key := chaseSessionKeyFor(target.RecordingID, offsetSeconds)
	var s *liveSession
	if target.canStartChaseSession() {
		var err error
		s, err = ls.existingChaseSessionOrCooldown(r.Context(), key)
		if err != nil {
			if _, retryable := liveEvictionReason(err); !retryable {
				writeSessionError(w, err)
				return
			}
			// 起動に失敗した進行中のセッションは、共通の退避・再試行経路（下の
			// recoverSessionStartup。s.done の待ちもそこで行う）に通す。健全なセッションと
			// cooldown 応答は、offset のメタデータ要求より前に処理する。
			s, err = ls.recoverSessionStartup(r.Context(), key, s.source, s, err)
			if err != nil {
				writeSessionError(w, err)
				return
			}
		}
		if s == nil {
			committedSize := ls.committedOriginalSize(target.RecordingID)
			var source sessionSource
			if offsetSeconds == 0 {
				// 先頭からでも、追従配信が閉じた後は Range で続きを読む（followChaseRecord）。
				client, ok := ls.mirakc.(mirakcSeekRecordClient)
				if !ok {
					http.Error(w, "chase stream unavailable", http.StatusServiceUnavailable)
					return
				}
				source = func(ctx context.Context) (io.ReadCloser, error) {
					return followChaseRecord(ctx, client, target.RecordID, committedSize)
				}
			} else {
				client, ok := ls.mirakc.(mirakcSeekRecordClient)
				if !ok {
					http.Error(w, "chase offset stream unavailable", http.StatusServiceUnavailable)
					return
				}
				record, err := client.GetRecord(r.Context(), target.RecordID)
				if err != nil {
					slog.Error("streamer: getting chase record metadata", "record_id", target.RecordID, "err", err)
					http.Error(w, "chase offset stream unavailable", http.StatusServiceUnavailable)
					return
				}
				startByte, err := chaseStartByteOffset(record, offsetSeconds)
				if err != nil {
					if errors.Is(err, errChaseOffsetUnavailable) {
						http.Error(w, "chase offset is outside the available recording range", http.StatusRequestedRangeNotSatisfiable)
						return
					}
					http.Error(w, "chase offset stream is not ready", http.StatusServiceUnavailable)
					return
				}
				source = func(ctx context.Context) (io.ReadCloser, error) {
					return waitForChaseRecordAtOffset(ctx, client, target.RecordID, startByte, committedSize)
				}
			}
			s, err = ls.getOrCreateSessionFor(r.Context(), key, source)
			if err != nil {
				writeSessionError(w, err)
				return
			}
		}
	} else {
		// The recording finished after the session was created. Keep serving the
		// retained EVENT playlist, but never start a second mirakc follow session.
		ls.mu.Lock()
		var ok bool
		s, ok = ls.chaseSessions[key]
		ls.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err := waitReadyTouching(r.Context(), s, playlistStartupTimeout); err != nil {
			if errors.Is(err, errStartupTimeout) {
				http.Error(w, "chase stream did not start in time", http.StatusGatewayTimeout)
			}
			return
		}
		if s.startErr != nil {
			http.NotFound(w, r)
			return
		}
	}
	s.touch()

	// captions 無効時も音声レンディションを含む per-profile master を返す。
	playlistName := profile.Name + ".m3u8"
	readyMarker := "#EXT-X-STREAM-INF"
	if ls.cfg.Captions {
		playlistName = "playlist.m3u8"
	}
	playlistPath := filepath.Join(s.dir, playlistName)
	content, ok := waitForPlaylist(r.Context(), s, playlistPath, playlistStartupTimeout, readyMarker)
	if !ok {
		slog.Error("streamer: chase playlist did not appear in time",
			"recording_id", target.RecordingID, "profile", profile.Name, "dir", s.dir)
		http.Error(w, "chase stream did not start in time", http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(content)
}

// ChaseSegment serves segments and variant playlists from the recording-keyed
// session. The site in the URL selects the LiveStreamer, so the hot segment path
// does not query Postgres repeatedly.
func (ls *LiveStreamer) ChaseSegment(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := chi.URLParam(r, "name")
	if !ls.cfg.servesFile(name) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}

	offsetSeconds, ok := chaseOffsetFromRequest(r)
	if !ok {
		http.Error(w, "invalid chase offset", http.StatusBadRequest)
		return
	}
	ls.mu.Lock()
	s, ok := ls.chaseSessions[chaseSessionKeyFor(recordingID, offsetSeconds)]
	ls.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := waitReadyTouching(r.Context(), s, playlistStartupTimeout); err != nil {
		if errors.Is(err, errStartupTimeout) {
			http.Error(w, "chase stream did not start in time", http.StatusGatewayTimeout)
		}
		return
	}
	if s.startErr != nil {
		http.NotFound(w, r)
		return
	}
	s.touch()

	path := sessionFilePath(s.dir, name)
	if strings.HasSuffix(name, ".m3u8") {
		content, ok := waitForPlaylist(r.Context(), s, path, playlistStartupTimeout, "#EXTINF")
		if !ok {
			http.Error(w, "chase stream did not start in time", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(content)
		return
	}
	if filepath.Ext(name) == ".vtt" {
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

// ChaseLeave is a leave hint, not a stop command, with the same shared-session
// semantics as live Leave. A missing session is intentionally still 204.
func (ls *LiveStreamer) ChaseLeave(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	offsetSeconds, ok := chaseOffsetFromRequest(r)
	if !ok {
		http.Error(w, "invalid chase offset", http.StatusBadRequest)
		return
	}
	ls.mu.Lock()
	s, ok := ls.chaseSessions[chaseSessionKeyFor(recordingID, offsetSeconds)]
	ls.mu.Unlock()
	if !ok {
		metrics.LiveLeaveHints.WithLabelValues("no_session").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.hintLeave(time.Now(), ls.cfg.leaveGrace(), ls.cfg.IdleTimeout) {
		metrics.LiveLeaveHints.WithLabelValues("no_effect").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	metrics.LiveLeaveHints.WithLabelValues("deadline_shortened").Inc()
	slog.Info("streamer: chase leave hint received, shortening idle deadline",
		"recording_id", recordingID, "grace", ls.cfg.leaveGrace())
	w.WriteHeader(http.StatusNoContent)
}

// OriginalVODPlaylist starts or joins the single source-TS-to-HLS session for
// this recording. Every configured profile is emitted by the same ffmpeg; the
// profile query selects a playlist and is deliberately absent from the key.
func (ls *LiveStreamer) OriginalVODPlaylist(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	offsetSeconds, ok := chaseOffsetFromRequest(r)
	if !ok {
		http.Error(w, "invalid original VOD offset", http.StatusBadRequest)
		return
	}
	profile, ok := ls.cfg.profile(r.URL.Query().Get("profile"))
	if !ok {
		http.Error(w, "unknown original VOD profile", http.StatusBadRequest)
		return
	}
	target, err := ls.lookupOriginalVODTarget(r.Context(), recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			ls.invalidateOriginalVODSession(recordingID)
		}
		writeOriginalVODError(w, r, err)
		return
	}
	key := originalVODSessionKeyFor(recordingID, offsetSeconds)
	ls.mu.Lock()
	s, exists := ls.getSessionLocked(key)
	ls.mu.Unlock()
	if !exists {
		source := ls.originalVODSource(recordingID, offsetSeconds, target)
		// Reject out-of-range requests before getOrCreateSessionFor can fail on a
		// full pool or evict an unrelated idle session. The descriptor opened here
		// is closed at once; the session reopens the file itself, so a file unlinked
		// in between yields the usual 404.
		if offsetSeconds > 0 {
			probe, err := source(r.Context())
			if err != nil {
				if errors.Is(err, errOriginalVODOffsetUnavailable) {
					slog.Info("streamer: original VOD offset outside recording range",
						"recording_id", recordingID, "offset", offsetSeconds)
				}
				writeOriginalVODError(w, r, err)
				return
			}
			_ = probe.Close()
		}
		s, err = ls.getOrCreateSessionFor(
			r.Context(), key, source,
		)
		if err != nil {
			writeOriginalVODError(w, r, err)
			return
		}
	} else if err := waitReadyTouching(r.Context(), s, playlistStartupTimeout); err != nil {
		if errors.Is(err, errStartupTimeout) {
			http.Error(w, "original VOD stream did not start in time", http.StatusGatewayTimeout)
		}
		return
	} else if s.startErr != nil {
		writeOriginalVODError(w, r, s.startErr)
		return
	}
	s.touch()

	playlistName := profile.Name + ".m3u8"
	if ls.cfg.Captions {
		playlistName = "playlist.m3u8"
	}
	content, ok := waitForPlaylist(
		r.Context(), s, filepath.Join(s.dir, playlistName), playlistStartupTimeout, "#EXT-X-STREAM-INF",
	)
	if !ok {
		slog.Error("streamer: original VOD playlist did not appear in time",
			"recording_id", recordingID, "profile", profile.Name, "dir", s.dir)
		http.Error(w, "original VOD stream did not start in time", http.StatusGatewayTimeout)
		return
	}
	writeHLSPlaylist(w, content)
}

// OriginalVODSegment serves the (possibly still growing) EVENT variant playlist, subtitle
// playlist, or segment from the retained original VOD session.
func (ls *LiveStreamer) OriginalVODSegment(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	offsetSeconds, ok := chaseOffsetFromRequest(r)
	if !ok {
		http.Error(w, "invalid original VOD offset", http.StatusBadRequest)
		return
	}
	name := chi.URLParam(r, "name")
	if !ls.cfg.servesFile(name) {
		http.Error(w, "invalid segment name", http.StatusBadRequest)
		return
	}
	if ls.originalVODRecordingRemoved(r.Context(), recordingID) {
		ls.invalidateOriginalVODSession(recordingID)
		http.NotFound(w, r)
		return
	}
	key := originalVODSessionKeyFor(recordingID, offsetSeconds)
	ls.mu.Lock()
	s, ok := ls.getSessionLocked(key)
	ls.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := waitReadyTouching(r.Context(), s, playlistStartupTimeout); err != nil {
		if errors.Is(err, errStartupTimeout) {
			http.Error(w, "original VOD stream did not start in time", http.StatusGatewayTimeout)
		}
		return
	}
	if s.startErr != nil {
		writeOriginalVODError(w, r, s.startErr)
		return
	}
	s.touch()

	path := sessionFilePath(s.dir, name)
	if strings.HasSuffix(name, ".m3u8") {
		content, ok := waitForPlaylist(r.Context(), s, path, playlistStartupTimeout, "#EXTINF")
		if !ok {
			http.Error(w, "original VOD stream did not start in time", http.StatusGatewayTimeout)
			return
		}
		writeHLSPlaylist(w, content)
		return
	}
	if filepath.Ext(name) == ".vtt" {
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

// invalidateOriginalVODSession stops every offset session and removes its HLS
// output after the recording stops being an eligible VOD target (for example,
// trash or purge). Completed FFmpeg sessions are otherwise retained until idle GC.
func (ls *LiveStreamer) invalidateOriginalVODSession(recordingID int64) {
	ls.mu.Lock()
	var sessions []*liveSession
	for key, s := range ls.chaseSessions {
		if key.kind == originalVODSessionKind && key.id == recordingID {
			delete(ls.chaseSessions, key)
			sessions = append(sessions, s)
		}
	}
	ls.mu.Unlock()
	if len(sessions) == 0 {
		return
	}
	for _, s := range sessions {
		s.stop()
		cleanupSessionDir(s)
	}
	ls.setActiveSessionMetrics()
}

// OriginalVODLeave shortens the shared session's idle deadline. It is a hint;
// viewers of the same recording keep the session alive through their requests.
func (ls *LiveStreamer) OriginalVODLeave(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "site") != ls.site {
		http.NotFound(w, r)
		return
	}
	recordingID, ok := parseCanonicalRecordingID(chi.URLParam(r, "id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	offsetSeconds, ok := chaseOffsetFromRequest(r)
	if !ok {
		http.Error(w, "invalid original VOD offset", http.StatusBadRequest)
		return
	}
	key := originalVODSessionKeyFor(recordingID, offsetSeconds)
	ls.mu.Lock()
	s, ok := ls.getSessionLocked(key)
	ls.mu.Unlock()
	if !ok {
		metrics.LiveLeaveHints.WithLabelValues("no_session").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.hintLeave(time.Now(), ls.cfg.leaveGrace(), ls.cfg.IdleTimeout) {
		metrics.LiveLeaveHints.WithLabelValues("no_effect").Inc()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	metrics.LiveLeaveHints.WithLabelValues("deadline_shortened").Inc()
	slog.Info("streamer: original VOD leave hint received, shortening idle deadline",
		"recording_id", recordingID, "grace", ls.cfg.leaveGrace())
	w.WriteHeader(http.StatusNoContent)
}

func writeOriginalVODError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, errSessionLimit) {
		writeSessionError(w, err)
		return
	}
	if errors.Is(err, errOriginalVODOffsetUnavailable) {
		http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
		return
	}
	slog.Error("streamer: starting original VOD session", "err", err)
	http.Error(w, "original VOD stream unavailable", http.StatusServiceUnavailable)
}

func writeHLSPlaylist(w http.ResponseWriter, content []byte) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(content)
}

func writeChaseTargetError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	slog.Error("streamer: looking up chase target", "err", err)
	http.Error(w, "chase stream unavailable", http.StatusInternalServerError)
}

// parseCanonicalRecordingID accepts only the decimal spelling used by
// recordings.id in URLs. Aliases such as 00123 are rejected so one recording
// cannot have multiple cache or session identities.
func parseCanonicalRecordingID(raw string) (int64, bool) {
	if raw == "" || (len(raw) > 1 && raw[0] == '0') {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 || strconv.FormatInt(v, 10) != raw {
		return 0, false
	}
	return v, true
}

// followChaseRecord は先頭からの追っかけの入力を返す。mirakc の追従配信は無入力
// タイムアウトで録画中にも閉じうる（実際に閉じる頻度は未検証）。そのまま ffmpeg の EOF に
// すると、録画が続いているのに playlist に ENDLIST が付く。閉じた後（正常に閉じても、
// 途中で切れても）は読んだバイトの続きから chaseRangeFollowReader で追い、mirakc が録画の
// 終了を返すまで EOF にしない（TestFollowChaseRecordContinuesWithRangeAfterFollowCloses /
// TestChaseRangeFollowReaderResumesAfterUncleanBodyClose）。
func followChaseRecord(ctx context.Context, client mirakcSeekRecordClient, recordID string, committedSize chaseCommittedSize) (io.ReadCloser, error) {
	body, err := waitForChaseRecord(ctx, client, recordID)
	if err != nil {
		return nil, err
	}
	return newChaseRangeFollowReader(ctx, client, recordID, 0, body, committedSize), nil
}

func waitForChaseRecord(ctx context.Context, client mirakcRecordClient, recordID string) (io.ReadCloser, error) {
	deadline := time.Now().Add(playlistStartupTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errChaseRecordNotReadyTimeout
		}
		body, err := streamRecordFollowWithin(ctx, client, recordID, remaining)
		if err == nil {
			return body, nil
		}
		if errors.Is(err, errChaseRecordNotReadyTimeout) {
			return nil, err
		}
		if !errors.Is(err, mirakc.ErrRecordNotReady) {
			return nil, err
		}

		// 204 means “the recording file is still empty”, not a broken connection.
		// Retry until the same 15s startup budget used by playlist readiness expires;
		// this keeps 204 retries out of connection-failure accounting while still
		// guaranteeing that a request eventually returns 503.
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return nil, errChaseRecordNotReadyTimeout
		}
		wait := playlistPollInterval
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

const mpegTSPacketSize = 188

// chaseStartByteOffset maps the user-facing recording-file-relative second to a
// byte position in the current mirakc content snapshot. The file starts at the
// later of the tuner-open time and Program.StartAt: filter-program waits for
// the scheduled programme when mirakc opens the tuner early. MPEG-TS packet
// alignment avoids asking ffmpeg to begin halfway through a packet. The map is
// intentionally approximate: bitrate changes and encoder buffering mean that
// PTS is the final authority, so the UI exposes normal HLS seeking after the
// initial start.
func chaseStartByteOffset(record *mirakc.Record, offsetSeconds int64) (int64, error) {
	if offsetSeconds == 0 {
		return 0, nil
	}
	if record == nil || record.Content.Length == nil || *record.Content.Length == 0 {
		return 0, mirakc.ErrRecordNotReady
	}

	recordingStart := record.Recording.StartTime.Time()
	fileStart := recordingStart
	if record.Program.StartAt != nil && record.Program.StartAt.Time().After(fileStart) {
		fileStart = record.Program.StartAt.Time()
	}
	available := time.Since(fileStart)
	if record.Recording.Status != "recording" {
		switch {
		case record.Recording.Duration != nil:
			available = time.Duration(*record.Recording.Duration)*time.Millisecond - fileStart.Sub(recordingStart)
		case record.Recording.EndTime != nil:
			available = record.Recording.EndTime.Time().Sub(fileStart)
		}
	}
	availableSeconds := int64(available / time.Second)
	if availableSeconds <= offsetSeconds || availableSeconds <= 0 {
		return 0, errChaseOffsetUnavailable
	}

	length := *record.Content.Length
	if length < mpegTSPacketSize {
		return 0, mirakc.ErrRecordNotReady
	}
	// This form avoids overflowing length*offset for long recordings while
	// retaining integer arithmetic for the byte position.
	denominator := uint64(availableSeconds)
	requested := uint64(offsetSeconds)
	byteOffset := (length/denominator)*requested + (length%denominator)*requested/denominator
	byteOffset -= byteOffset % mpegTSPacketSize
	if byteOffset >= length {
		byteOffset = length - mpegTSPacketSize
		byteOffset -= byteOffset % mpegTSPacketSize
	}
	return int64(byteOffset), nil
}

// waitForChaseRecordAtOffset gets the first finite Range response within the
// normal startup budget. Once the first bytes are available, the returned
// reader follows the recording by requesting the next Range after each finite
// response reaches EOF.
func waitForChaseRecordAtOffset(ctx context.Context, client mirakcSeekRecordClient, recordID string, startByte int64, committedSize chaseCommittedSize) (io.ReadCloser, error) {
	deadline := time.Now().Add(playlistStartupTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errChaseRecordNotReadyTimeout
		}
		body, length, err := streamRecordRangeWithin(ctx, client, recordID, startByte, remaining)
		// length は 206 の Content-Length。分からない（-1）本文も読み、空かどうかは
		// 読んだ結果で reader が判断する。
		if err == nil && body != nil && length != 0 {
			return newChaseRangeFollowReader(ctx, client, recordID, startByte, body, committedSize), nil
		}
		if body != nil {
			_ = body.Close()
		}
		if errors.Is(err, errChaseRecordNotReadyTimeout) {
			return nil, err
		}
		if err != nil && !errors.Is(err, mirakc.ErrRecordNotReady) && !errors.Is(err, mirakc.ErrRangeNotSatisfiable) {
			return nil, err
		}
		if err := waitForChaseRecordPoll(ctx, deadline); err != nil {
			return nil, err
		}
	}
}

func waitForChaseRecordPoll(ctx context.Context, deadline time.Time) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errChaseRecordNotReadyTimeout
	}
	wait := playlistPollInterval
	if remaining < wait {
		wait = remaining
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// 追っかけの Range 追従の間隔と再試行の待ち。テストが実時間を待たずに済むよう var にする。
var (
	// chaseRangePollMin は Range 要求の最短間隔（要求の開始から数える）。待たずに次を取ると、
	// 追い付いた直後の空の応答と GetRecord で 1 セッションあたり毎秒 20 要求になっていた
	// （古い mirakc は Range ごとに dd を起こす）。データが続く間の変換の遅れはこの間隔まで。
	chaseRangePollMin = 500 * time.Millisecond
	// chaseRangePollMax は追い付いたまま録画中が続くときのバックオフの上限。録画が止まって
	// いる間だけ効き、再開後の最初の取得はこれだけ遅れうる。フロントは変換済みの端
	// （seekable の終端）より後ろへのシークを新しい offset で張り直すので、この遅れは
	// 張り直しの頻度を変えるだけで、見られない区間を作らない（docs/frontend/live.md）。
	chaseRangePollMax = time.Second
	// chaseRetryDelay は一過性の失敗の再試行の待ち。回数と分類は ingest と共有する
	// （mirakc.RetryDelay / mirakc.MaxConsecutiveRetries / mirakc.IsRetryable）。
	chaseRetryDelay = mirakc.RetryDelay
)

// errChaseRecordPurged は、追っかけが録画ファイルの終端まで読む前に mirakc の record が
// 消えた（404）ことを表す。
var errChaseRecordPurged = errors.New("mirakc record is gone before the chase read it to the end")

// chaseCommittedSize は録画のコミット済み原本のバイト数を返す（無ければ ok=false）。
type chaseCommittedSize func(ctx context.Context) (size int64, ok bool, err error)

// chaseRangeFollowReader turns mirakc's finite Range responses into one
// long-lived reader. It never reads and discards the recording head: every
// request begins at the byte position already consumed by ffmpeg.
//
// EOF を返すのは、次のどちらかで録画ファイルの終端まで渡し終えたときだけである。
//   - GetRecord が録画の終了（recording 以外）を返し、同じ offset への最後の Range も空だった
//     （状態遷移と最後の追記の競合を吸収する）
//   - record が 404 になり、かつコミット済み原本のバイト数が読んだ位置と一致した。ingest は
//     mirakc が録画の終了を返し、最後の Range が空になるまで読んでからコミットするので
//     （internal/worker/ingest.go の transferIngestRecord）、コミットされたバイト数は終了時点の
//     ファイル長である。一致すれば終端まで読んでいる
//
// 404 で原本と一致しない（ffmpeg が先端より遅れていて purge が先に来た、別経路の原本、
// mirakc が record を失った等）ならエラーを返す。終端が分からないまま EOF にすると、
// 欠けた終端に ENDLIST が付く。一過性の失敗は mirakc.MaxConsecutiveRetries 回まで再試行する。
//
// Close は別の goroutine から呼んでよい。進行中の Read と待ちを打ち切り、以後の要求を止める。
type chaseRangeFollowReader struct {
	ctx           context.Context
	cancel        context.CancelFunc
	client        mirakcSeekRecordClient
	recordID      string
	committedSize chaseCommittedSize

	mu     sync.Mutex
	body   io.ReadCloser // mu で守る
	closed bool          // mu で守る

	// 以下は Read を呼ぶ goroutine だけが触る。
	nextOffset int64
	// bodyBytes は今の本文から読んだバイト数。Content-Length の無い（-1）本文が何も返さずに
	// 終わったら、追い付いた（空の応答）として扱う。
	bodyBytes     int64
	finished      bool
	done          bool
	err           error
	nextRequestAt time.Time
	idleWait      time.Duration
	failures      int
}

// newChaseRangeFollowReader は offset から追う reader を作る。body は offset から始まる
// 読みかけの本文（追従配信か最初の Range 応答）で、nil なら最初の Read で Range を要求する。
// committedSize が nil なら、404 は常にエラーになる。
func newChaseRangeFollowReader(ctx context.Context, client mirakcSeekRecordClient, recordID string, offset int64, body io.ReadCloser, committedSize chaseCommittedSize) *chaseRangeFollowReader {
	ctx, cancel := context.WithCancel(ctx)
	return &chaseRangeFollowReader{
		ctx:           ctx,
		cancel:        cancel,
		client:        client,
		recordID:      recordID,
		committedSize: committedSize,
		body:          body,
		nextOffset:    offset,
	}
}

func (r *chaseRangeFollowReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		body, closed := r.current()
		switch {
		case closed:
			return 0, io.ErrClosedPipe
		case r.err != nil:
			return 0, r.err
		case r.done:
			return 0, io.EOF
		case body == nil:
			if err := r.requestNext(); err != nil {
				r.err = err
			}
			continue
		}

		n, err := body.Read(p)
		r.nextOffset += int64(n)
		r.bodyBytes += int64(n)
		if n > 0 {
			r.failures = 0
			r.idleWait = 0
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			empty := r.bodyBytes == 0
			r.closeBody(body)
			if empty {
				if caughtErr := r.caughtUp(); caughtErr != nil {
					r.err = caughtErr
				}
			}
		default:
			// 追従配信や Range の本文が途中で切れた（ErrUnexpectedEOF・接続リセット等）。
			// 読んだ位置は分かっているので、同じ続きを Range で取り直す。
			r.closeBody(body)
			if failErr := r.failure(fmt.Errorf("reading chase record body at offset %d: %w", r.nextOffset, err)); failErr != nil {
				r.err = failErr
			}
		}
		if n > 0 {
			return n, nil
		}
	}
}

// requestNext は nextOffset からの Range を 1 回要求する。本文があれば r.body に置き、
// 空なら caughtUp、失敗なら failure / gone に回す。
func (r *chaseRangeFollowReader) requestNext() error {
	if err := sleepUntil(r.ctx, r.nextRequestAt); err != nil {
		return err
	}
	r.nextRequestAt = time.Now().Add(chaseRangePollMin)
	body, length, err := r.client.StreamRecord(r.ctx, r.recordID, r.nextOffset)
	if err == nil && body != nil && length != 0 {
		r.bodyBytes = 0
		return r.setBody(body)
	}
	if body != nil {
		_ = body.Close()
	}
	switch {
	case err == nil, errors.Is(err, mirakc.ErrRecordNotReady), errors.Is(err, mirakc.ErrRangeNotSatisfiable):
		r.failures = 0
		return r.caughtUp()
	case chaseRecordNotFound(err):
		return r.gone(err)
	default:
		return r.failure(fmt.Errorf("requesting chase record range at offset %d: %w", r.nextOffset, err))
	}
}

// caughtUp は空の応答の後に呼ぶ。終了済みなら EOF、まだ録画中ならバックオフして待つ。
func (r *chaseRangeFollowReader) caughtUp() error {
	if r.finished {
		r.done = true
		return nil
	}
	finished, err := chaseRecordFinished(r.ctx, r.client, r.recordID)
	if err != nil {
		if chaseRecordNotFound(err) {
			return r.gone(err)
		}
		return r.failure(fmt.Errorf("checking chase record status: %w", err))
	}
	r.failures = 0
	if finished {
		// 状態遷移と最後の追記の競合を吸収するため、同じ offset をすぐにもう 1 回読む。
		r.finished = true
		r.nextRequestAt = time.Time{}
		return nil
	}
	if r.idleWait == 0 {
		r.idleWait = chaseRangePollMin
	} else {
		r.idleWait = min(r.idleWait*2, chaseRangePollMax)
	}
	r.nextRequestAt = time.Now().Add(r.idleWait)
	return nil
}

// gone は record が 404 になったときに呼ぶ。読んだ位置がコミット済み原本の終端なら EOF、
// それ以外はエラーにする（chaseRangeFollowReader の doc を参照）。
func (r *chaseRangeFollowReader) gone(cause error) error {
	if r.committedSize != nil {
		size, ok, err := r.committedSize(r.ctx)
		if err != nil {
			return fmt.Errorf("%w at offset %d (reading the committed original: %w): %w", errChaseRecordPurged, r.nextOffset, err, cause)
		}
		if ok && size == r.nextOffset {
			r.done = true
			return nil
		}
		if ok {
			return fmt.Errorf("%w at offset %d of %d committed bytes: %w", errChaseRecordPurged, r.nextOffset, size, cause)
		}
	}
	return fmt.Errorf("%w at offset %d (no committed original): %w", errChaseRecordPurged, r.nextOffset, cause)
}

// failure は一過性の失敗なら上限つきでバックオフを予約し、そうでなければエラーを返す。
func (r *chaseRangeFollowReader) failure(err error) error {
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if !mirakc.IsRetryable(err) {
		return err
	}
	r.failures++
	if r.failures > mirakc.MaxConsecutiveRetries {
		return fmt.Errorf("chase record %s failed %d consecutive times: %w", r.recordID, r.failures, err)
	}
	delay := chaseRetryDelay(r.failures - 1)
	slog.Warn("streamer: transient chase record failure, retrying",
		"record_id", r.recordID, "offset", r.nextOffset, "consecutive_failures", r.failures, "delay", delay, "err", err)
	r.nextRequestAt = time.Now().Add(delay)
	return nil
}

func (r *chaseRangeFollowReader) current() (io.ReadCloser, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body, r.closed
}

func (r *chaseRangeFollowReader) setBody(body io.ReadCloser) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = body.Close()
		return io.ErrClosedPipe
	}
	r.body = body
	r.mu.Unlock()
	return nil
}

// closeBody は body がまだ今の本文なら外して閉じる（Close が先に外していたら何もしない）。
func (r *chaseRangeFollowReader) closeBody(body io.ReadCloser) {
	r.mu.Lock()
	if r.body != body {
		r.mu.Unlock()
		return
	}
	r.body = nil
	r.mu.Unlock()
	_ = body.Close()
}

// Close は進行中の Read と待ちを打ち切り、以後の要求を止める（TestChaseRangeFollowReaderCloseStopsConcurrentRead）。
func (r *chaseRangeFollowReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	body := r.body
	r.body = nil
	r.mu.Unlock()
	r.cancel()
	if body == nil {
		return nil
	}
	return body.Close()
}

// chaseRecordNotFound は mirakc の record が無い（404）か。意味は chaseRangeFollowReader を参照。
func chaseRecordNotFound(err error) bool {
	var apiErr *mirakc.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// sleepUntil は at まで待つ（過去なら待たない）。
func sleepUntil(ctx context.Context, at time.Time) error {
	wait := time.Until(at)
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func chaseRecordFinished(ctx context.Context, client mirakcSeekRecordClient, recordID string) (bool, error) {
	record, err := client.GetRecord(ctx, recordID)
	if err != nil {
		return false, err
	}
	return record.Recording.Status != "recording", nil
}

type rangedRecordResult struct {
	body   io.ReadCloser
	length int64
	err    error
}

// streamRecordRangeWithin bounds only the response-header wait. The returned
// body remains valid beyond timeout because its request context is cancelled
// only when the body is closed or the session ends.
func streamRecordRangeWithin(ctx context.Context, client mirakcSeekRecordClient, recordID string, offset int64, timeout time.Duration) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	attemptCtx, cancel := context.WithCancel(ctx)
	resultCh := make(chan rangedRecordResult)
	go func() {
		body, length, err := client.StreamRecord(attemptCtx, recordID, offset)
		select {
		case resultCh <- rangedRecordResult{body: body, length: length, err: err}:
		case <-attemptCtx.Done():
			if body != nil {
				_ = body.Close()
			}
		}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-resultCh:
		if result.err != nil {
			cancel()
			return nil, 0, result.err
		}
		if result.body == nil {
			cancel()
			return nil, 0, mirakc.ErrRecordNotReady
		}
		return &cancelOnCloseReadCloser{ReadCloser: result.body, cancel: cancel}, result.length, nil
	case <-timer.C:
		cancel()
		return nil, 0, errChaseRecordNotReadyTimeout
	case <-ctx.Done():
		cancel()
		return nil, 0, ctx.Err()
	}
}

// streamRecordFollowWithin bounds the response-header wait without imposing the
// same deadline on the returned long-lived body. A context deadline passed
// directly to StreamRecordFollow would also cancel a successful body after the
// chase startup budget, which would truncate the ffmpeg input. On timeout we
// cancel the request context; on success the request context remains attached to
// the body and is released with the session context.
func streamRecordFollowWithin(ctx context.Context, client mirakcRecordClient, recordID string, timeout time.Duration) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	attemptCtx, cancel := context.WithCancel(ctx)
	type result struct {
		body io.ReadCloser
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		body, err := client.StreamRecordFollow(attemptCtx, recordID)
		resultCh <- result{body: body, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-resultCh:
		if result.err != nil {
			cancel()
			return nil, result.err
		}
		return &cancelOnCloseReadCloser{ReadCloser: result.body, cancel: cancel}, nil
	case <-timer.C:
		cancel()
		return nil, errChaseRecordNotReadyTimeout
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

type cancelOnCloseReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *cancelOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.cancel()
	return err
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
	case errors.Is(err, errChaseInputCoolingDown):
		var cooldown *chaseInputCooldownError
		seconds := 1
		if errors.As(err, &cooldown) && cooldown.retryAfter > 0 {
			seconds = int((cooldown.retryAfter + time.Second - 1) / time.Second)
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		http.Error(w, chaseInputCooldownMessage, http.StatusBadGateway)
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

const chaseInputCooldownMessage = "追っかけ入力のエラーが続いているため、再作成を一時停止しています。しばらく待ってから再読み込みしてください。"

type chaseInputCooldownError struct {
	retryAfter time.Duration
}

func (e *chaseInputCooldownError) Error() string { return errChaseInputCoolingDown.Error() }

func (e *chaseInputCooldownError) Unwrap() error { return errChaseInputCoolingDown }

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

type sessionKind string

const (
	liveSessionKind        sessionKind = "live"
	chaseSessionKind       sessionKind = "chase"
	originalVODSessionKind sessionKind = "original_vod"
)

type sessionKey struct {
	kind          sessionKind
	id            int64
	offsetSeconds int64
}

type sessionSource func(context.Context) (io.ReadCloser, error)

// chaseSessionDir gives every recording-relative start position its own leaf
// directory. In particular, offset 0 must not use the recording directory as
// the parent of other offsets: cleanup of one session is allowed to remove only
// that session's HLS files.
func chaseSessionDir(segmentDir, site string, recordingID, offsetSeconds int64) string {
	return filepath.Join(
		segmentDir,
		site,
		"chase",
		strconv.FormatInt(recordingID, 10),
		"offset",
		strconv.FormatInt(offsetSeconds, 10),
	)
}

func originalVODSessionDir(segmentDir, site string, recordingID, offsetSeconds int64) string {
	return filepath.Join(
		segmentDir,
		site,
		"original-vod",
		strconv.FormatInt(recordingID, 10),
		"offset",
		strconv.FormatInt(offsetSeconds, 10),
	)
}

func originalVODSessionKeyFor(recordingID, offsetSeconds int64) sessionKey {
	return sessionKey{kind: originalVODSessionKind, id: recordingID, offsetSeconds: offsetSeconds}
}

// liveSession はライブまたは追っかけ再生の 1 セッション（1 mirakc 接続 +
// 1 ffmpeg プロセス）。種別だけが資源の入口と出力ディレクトリを変え、上限・
// startup wait・idle GC・leave は同じ状態機械を通る。
//
// crash-only の唯一の例外（使い捨てのインメモリ状態）。DB には一切書かない。
type liveSession struct {
	serviceID int64
	key       sessionKey
	source    sessionSource
	dir       string // SegmentDir/site/{serviceID|chase/recordingID/offset/seconds}

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
	return len(ls.sessions) + len(ls.chaseSessions)
}

func sessionKindOf(s *liveSession) sessionKind {
	switch s.key.kind {
	case chaseSessionKind, originalVODSessionKind:
		return s.key.kind
	default:
		return liveSessionKind
	}
}

func sessionIDOf(s *liveSession) int64 {
	if sessionKindOf(s) != liveSessionKind {
		return s.key.id
	}
	return s.serviceID
}

// getSessionLocked returns the session for a complete key. The caller must
// hold ls.mu. Chase offsets are part of the key so two initial positions never
// share an HLS timeline accidentally.
func (ls *LiveStreamer) getSessionLocked(key sessionKey) (*liveSession, bool) {
	if key.kind == chaseSessionKind || key.kind == originalVODSessionKind {
		if ls.chaseSessions == nil {
			return nil, false
		}
		s, ok := ls.chaseSessions[key]
		return s, ok
	}
	if ls.sessions == nil {
		return nil, false
	}
	s, ok := ls.sessions[key.id]
	return s, ok
}

func (ls *LiveStreamer) putSessionLocked(s *liveSession) {
	if sessionKindOf(s) == chaseSessionKind || sessionKindOf(s) == originalVODSessionKind {
		if ls.chaseSessions == nil {
			ls.chaseSessions = make(map[sessionKey]*liveSession)
		}
		ls.chaseSessions[s.key] = s
		return
	}
	if ls.sessions == nil {
		ls.sessions = make(map[int64]*liveSession)
	}
	ls.sessions[s.key.id] = s
}

func (ls *LiveStreamer) deleteSessionLocked(s *liveSession) {
	if sessionKindOf(s) == chaseSessionKind || sessionKindOf(s) == originalVODSessionKind {
		delete(ls.chaseSessions, s.key)
		return
	}
	delete(ls.sessions, s.key.id)
}

func (ls *LiveStreamer) setActiveSessionMetrics() {
	ls.mu.Lock()
	live := len(ls.sessions)
	chase := len(ls.chaseSessions)
	vod := 0
	for key := range ls.chaseSessions {
		if key.kind == originalVODSessionKind {
			vod++
			chase--
		}
	}
	ls.mu.Unlock()
	metrics.LiveActiveSessions.WithLabelValues(string(liveSessionKind)).Set(float64(live))
	metrics.LiveActiveSessions.WithLabelValues(string(chaseSessionKind)).Set(float64(chase))
	metrics.LiveActiveSessions.WithLabelValues(string(originalVODSessionKind)).Set(float64(vod))
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
//
// **退避（takeIdleSessionForRetry → stop → 解放待ち）は evictMu で直列化し、
// LiveStreamer 全体で 1 本しか走らない（issue #677 のレビュー指摘）。** 同じ
// serviceID を待っている同時要求は全員が同じ起動失敗を受け取るので、ロックが
// 無いと全員が個別に退避を試み、圧力イベント 1 つに対して要求数ぶんの idle
// セッションを殺してしまう。evictMu を取った直後に `ls.sessions[serviceID]`
// を見て、**既に別の要求が退避と再試行を終えていれば**（先着の再試行が成功して
// 新しいセッションが map に入っていれば）自分は退避せずその成果に相乗りする。
// **evictMu の保持区間に getOrCreateSessionOnce（最大 playlistStartupTimeout の
// 起動待ち）を含めない** --- 含めると、無関係な別サービスへの要求まで他サービスの
// 起動待ちで足止めされる。実際の起動 I/O はロックの外で行う。
func (ls *LiveStreamer) getOrCreateSession(ctx context.Context, serviceID int64) (*liveSession, error) {
	return ls.getOrCreateSessionFor(ctx, sessionKey{kind: liveSessionKind, id: serviceID}, func(ctx context.Context) (io.ReadCloser, error) {
		return ls.mirakc.StreamService(ctx, serviceID, ls.cfg.TunerPriority)
	})
}

func (ls *LiveStreamer) getOrCreateSessionFor(ctx context.Context, key sessionKey, source sessionSource) (*liveSession, error) {
	s, err := ls.getOrCreateSessionOnceFor(ctx, key, source)
	return ls.recoverSessionStartup(ctx, key, source, s, err)
}

func (ls *LiveStreamer) recoverSessionStartup(ctx context.Context, key sessionKey, source sessionSource, s *liveSession, err error) (*liveSession, error) {
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

	// 呼び出し元（HTTP リクエスト）が既に切れているなら、退避してまで再試行する
	// 相手がいない。退避は 5 秒強 evictMu を占有するので、無意味な退避で他の
	// 同時要求を待たせない。
	if ctx.Err() != nil {
		return nil, err
	}

	ls.evictMu.Lock()

	// 別の同時要求が既に退避と再試行を終えていたら、退避せずその成果に相乗りする
	// （相乗り経路では eviction counter を計上しない --- 実際に退避したのは
	// 先着の 1 本だけである）。
	ls.mu.Lock()
	_, alreadyRecovered := ls.getSessionLocked(key)
	ls.mu.Unlock()
	if alreadyRecovered {
		ls.evictMu.Unlock()
		return ls.getOrCreateSessionOnceFor(ctx, key, source)
	}

	victim := ls.takeIdleSessionForRetry(time.Now())
	if victim == nil {
		ls.evictMu.Unlock()
		return nil, err
	}

	slog.Info("streamer: evicting idle session before retry",
		"kind", string(sessionKindOf(victim)), "session_id", sessionIDOf(victim), "reason", reason)
	victim.stop()
	// live の runSession は自分で掃除するが、完了済み chase は EVENT playlist
	// を idle GC まで保持するため defer が掃除を意図的に省略する。退去経路では
	// map から既に外れており通常の GC / shutdown が到達できないため、ここで明示的
	// にディレクトリを解放する。live 側に対しても冪等なので共通化する。
	cleanupSessionDir(victim)
	// mirakc は HTTP body の Close と tuner プロセスの解放を同期していない。
	// stop が done まで待っても、直後の要求が容量エラーになる窓が実物で観測された。
	releaseWait := liveMirakcReleaseWait
	if sessionKindOf(victim) == originalVODSessionKind {
		// 原本 VOD はチューナーを持たないので、解放待ちは無意味（シークのたびに
		// 5 秒止まる。実バイナリで測定）。
		releaseWait = 0
	}
	select {
	case <-ctx.Done():
		// **退避は既に起きている**（victim.stop() は完了済み）。ここで諦めるのは
		// このリクエストの再試行だけ --- mirakc の失敗ではないので retry_failed
		// には混ぜず、専用の result で区別する。
		ls.evictMu.Unlock()
		metrics.LiveSessionEvictions.WithLabelValues(reason, "retry_abandoned").Inc()
		return nil, ctx.Err()
	case <-time.After(releaseWait):
	}
	// 再試行セッションは evictMu を放す前に map へ登録する。放した後に登録すると、
	// evictMu を待っていた同時要求がその間に alreadyRecovered を確認して空振りし、
	// victim も既に無いので元のエラーを返してしまう。ready 待ちは evictMu の外で行う。
	retry, retryErr := ls.startSessionOnceFor(key, source)
	ls.evictMu.Unlock()
	if ls.afterEvictRelease != nil {
		ls.afterEvictRelease()
	}
	retry, retryErr = ls.awaitSessionReady(ctx, retry, retryErr)
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

// existingChaseSessionOrCooldown は、mirakc のメタデータを引く前に既存 offset のセッションへ
// 相乗りさせ、録画単位の入力失敗 cooldown 中は新規作成を拒む。この事前確認の後に別セッションが
// 失敗する競合は、getOrCreateSessionOnceFor 内の最終確認が塞ぐ。
func (ls *LiveStreamer) existingChaseSessionOrCooldown(ctx context.Context, key sessionKey) (*liveSession, error) {
	ls.mu.Lock()
	s, exists := ls.getSessionLocked(key)
	if !exists && key.kind == chaseSessionKind {
		if err := ls.chaseInputCooldownLocked(key.id); err != nil {
			ls.mu.Unlock()
			return nil, err
		}
	}
	ls.mu.Unlock()
	if !exists {
		return nil, nil
	}
	if err := waitReadyTouching(ctx, s, playlistStartupTimeout); err != nil {
		return nil, err
	}
	if s.startErr != nil {
		return s, s.startErr
	}
	return s, nil
}

// chaseInputCooldownLocked は録画単位の cooldown エラーを返す。呼び出し側は ls.mu を保持すること。
// 失敗の記録・map からの削除・作成の可否判定を 1 つのロック区間で行うためである。
func (ls *LiveStreamer) chaseInputCooldownLocked(recordingID int64) error {
	if retryAt, failed := ls.failedChaseInputs[recordingID]; failed {
		remaining := time.Until(retryAt)
		if remaining > 0 {
			return &chaseInputCooldownError{retryAfter: remaining}
		}
		delete(ls.failedChaseInputs, recordingID)
	}
	return nil
}

func (ls *LiveStreamer) getOrCreateSessionOnceFor(ctx context.Context, key sessionKey, source sessionSource) (*liveSession, error) {
	s, err := ls.startSessionOnceFor(key, source)
	return ls.awaitSessionReady(ctx, s, err)
}

// awaitSessionReady は startSessionOnceFor が返したセッションの ready を待つ。
// startSessionOnceFor がエラーを返していたら、そのまま返す。
func (ls *LiveStreamer) awaitSessionReady(ctx context.Context, s *liveSession, err error) (*liveSession, error) {
	if err != nil {
		return nil, err
	}
	if err := waitReadyTouching(ctx, s, playlistStartupTimeout); err != nil {
		return nil, err
	}
	if s.startErr != nil {
		return s, s.startErr
	}
	ls.setActiveSessionMetrics()
	return s, nil
}

// startSessionOnceFor は key のセッションが無ければ作って map に登録し、ready は待たずに返す。
// 既にあればそれを返す。登録までを 1 回のロックで行うので、evictMu を持ったまま呼べば
// 「退避した本人の登録」が evictMu を待つ同時要求より先に見える。
func (ls *LiveStreamer) startSessionOnceFor(key sessionKey, source sessionSource) (*liveSession, error) {
	ls.mu.Lock()
	if s, ok := ls.getSessionLocked(key); ok {
		ls.mu.Unlock()
		return s, nil
	}
	if key.kind == chaseSessionKind {
		if err := ls.chaseInputCooldownLocked(key.id); err != nil {
			ls.mu.Unlock()
			return nil, err
		}
	}
	if ls.closed {
		ls.mu.Unlock()
		return nil, errShuttingDown
	}
	if len(ls.sessions)+len(ls.chaseSessions) >= ls.cfg.MaxSessions {
		ls.mu.Unlock()
		metrics.LiveSessionStartFailures.WithLabelValues("session_limit").Inc()
		return nil, errSessionLimit
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	s := &liveSession{
		serviceID:  key.id,
		key:        key,
		source:     source,
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
		lastAccess: time.Now(),
		cancel:     cancel,
	}
	if key.kind == chaseSessionKind || key.kind == originalVODSessionKind {
		// Recording sessions use recordings.id (and chase also includes its offset).
		s.serviceID = 0
	}
	ls.putSessionLocked(s)
	ls.mu.Unlock()
	ls.setActiveSessionMetrics()

	go ls.runSession(sessionCtx, s)
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
// 中で最も古いものを選ぶ。
//
// **離脱ヒントを受けたセッションがこの規則で最古の候補になるのは
// `idle_timeout > 5 × segment_seconds + 2s` のときに限る（無条件ではない）。**
// ヒント直後の idle 時間は `idle_timeout - leaveGrace` で、これが候補の閾値
// （`2 × segment_seconds`）を上回るには `idle_timeout - (3×segment_seconds+2s) >
// 2×segment_seconds`、すなわち上記の条件が要る（`leaveGrace` の定義そのもの。
// 展開すると `idle_timeout > 5×segment_seconds + 2s`）。既定値（`idle_timeout: 30s` /
// `segment_seconds: 2s`）はこれを満たす（ヒント後 idle 22s > 閾値 4s）。満たさない
// 設定（例: `idle_timeout: 10s` / `segment_seconds: 2s` --- ヒント後 idle 2s < 閾値 4s）
// では、ヒントは退避の候補化には効かない。ただしその設定では idle GC 自体の刻みが
// 短いので（gcInterval が `idle_timeout` にも連動する）、露出は限定される ---
// `TestLiveStreamer_EvictionCandidate_PrefersLeaveHint`（成立域）と
// `TestLiveStreamer_EvictionCandidate_HintDoesNotQualifyBelowThreshold`（不成立域）が
// 両側を固定する。
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
	for _, s := range ls.chaseSessions {
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
		ls.deleteSessionLocked(victim)
	}
	ls.mu.Unlock()

	if victim != nil {
		ls.setActiveSessionMetrics()
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
	kind := sessionKindOf(s)
	keepCompletedRecordingSession := false
	inputFailed := false
	// close(s.done) は必ず最後（他の全ての後片付けの後）に行う。stop() は
	// `<-s.done` が閉じたら「片付け完了」とみなして戻るので、途中の状態
	// （map から消す前・ディレクトリを消す前）で閉じると、呼び出し側が
	// 「もう消えている」つもりで見に行った os.Stat がまだ古いディレクトリを
	// 見つけてしまう（実際にテストで踏んだ競合）。
	defer close(s.done)
	defer func() {
		ls.mu.Lock()
		if inputFailed {
			ls.recordFailedChaseInputLocked(s.key.id)
		}
		// idle GC が先にこの id を削除して新しいセッションに入れ替えていたら、
		// 新しいセッションを消さない（cur == s のときだけ削除）。
		if !keepCompletedRecordingSession {
			if cur, ok := ls.getSessionLocked(s.key); ok && cur == s {
				ls.deleteSessionLocked(s)
			}
		}
		ls.mu.Unlock()
		if !keepCompletedRecordingSession && s.dir != "" {
			cleanupSessionDir(s)
		}
		ls.setActiveSessionMetrics()
	}()

	dir := filepath.Join(ls.cfg.SegmentDir, ls.site, strconv.FormatInt(sessionIDOf(s), 10))
	switch kind {
	case chaseSessionKind:
		dir = chaseSessionDir(ls.cfg.SegmentDir, ls.site, sessionIDOf(s), s.key.offsetSeconds)
	case originalVODSessionKind:
		dir = originalVODSessionDir(ls.cfg.SegmentDir, ls.site, sessionIDOf(s), s.key.offsetSeconds)
	}
	if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
		s.startErr = fmt.Errorf("creating live segment dir: %w", err)
		metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
		close(s.ready)
		return
	}
	s.dir = dir

	body, err := s.source(ctx)
	if err != nil {
		if kind == originalVODSessionKind {
			s.startErr = err
			metrics.LiveSessionStartFailures.WithLabelValues("original_vod_error").Inc()
		} else if errors.Is(err, errChaseRecordNotReadyTimeout) {
			s.startErr = err
			metrics.LiveSessionStartFailures.WithLabelValues("record_not_ready_timeout").Inc()
		} else {
			s.startErr = &liveUpstreamStartError{err: err}
			metrics.LiveSessionStartFailures.WithLabelValues("upstream_error").Inc()
		}
		// 範囲外の offset は利用者入力の結果で、サーバーの障害ではない（416 になる）。
		level := slog.LevelError
		if errors.Is(err, errOriginalVODOffsetUnavailable) {
			level = slog.LevelInfo
		}
		slog.Log(ctx, level, "streamer: requesting session upstream",
			"kind", string(kind), "session_id", sessionIDOf(s), "err", err)
		close(s.ready)
		return
	}
	defer func() { _ = body.Close() }()

	var originalFile *os.File
	if kind == originalVODSessionKind {
		var ok bool
		originalFile, ok = body.(*os.File)
		if !ok {
			s.startErr = errors.New("original VOD source is not a seekable file")
			metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
			close(s.ready)
			return
		}
	}

	input, streamInfo, err := probeLiveSessionInput(ctx, ls.cfg.FFprobe, kind, sessionIDOf(s), body, originalFile)
	if err != nil {
		s.startErr = err
		close(s.ready)
		return
	}
	defer func() { _ = input.Close() }()
	captionInput := ls.cfg.Captions && streamInfo.hasSubtitles
	audioStreamCount := streamInfo.audioStreams

	playlistType := hlsLivePlaylist
	inputPath := "pipe:0"
	var offsetSeconds int64
	switch kind {
	case chaseSessionKind:
		playlistType = hlsEventPlaylist
	case originalVODSessionKind:
		playlistType = hlsOriginalEventPlaylist
		inputPath = originalVODFFmpegInputPath
		offsetSeconds = s.key.offsetSeconds
	}
	args := buildHLSFFmpegArgsForPlaylistType(
		ls.cfg, dir, captionInput, playlistType, inputPath, offsetSeconds, audioStreamCount,
	)
	cmd := exec.CommandContext(ctx, ls.cfg.FFmpeg, args...)
	if originalFile != nil {
		// Go maps ExtraFiles[0] to child fd 3. Passing the already-open original
		// keeps ffmpeg's seekable input alive if until_encoded unlinks its name.
		cmd.ExtraFiles = []*os.File{originalFile}
	}
	var chaseInput *chaseInputCopy
	switch {
	case originalFile != nil:
	case kind == chaseSessionKind:
		if chaseInput, s.startErr = attachChaseInput(cmd); s.startErr != nil {
			metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
			close(s.ready)
			return
		}
	default:
		cmd.Stdin = input
	}
	stderr := newCappedWriter(stderrCap)
	cmd.Stderr = stderr
	// ctx がキャンセルされてプロセスを kill した後、I/O をコピーするゴルーチン
	// （cmd.Stdin 用の内部パイプ）が終わるまで Wait は最大この時間だけ待つ。
	// ffmpeg が孫プロセスを fork していて標準入出力の fd を握ったまま残ると
	// （通常は起きないが）、Wait が無期限にブロックしうる。stop() は
	// idle GC / shutdown から呼ばれるので、ここが詰まるとチューナー解放も
	// 詰まる --- 上限を設けて必ず前に進めるようにする。
	cmd.WaitDelay = 5 * time.Second

	startErr := cmd.Start()
	chaseInput.started(startErr)
	if err := startErr; err != nil {
		s.startErr = fmt.Errorf("starting live ffmpeg: %w", err)
		metrics.LiveSessionStartFailures.WithLabelValues("ffmpeg_error").Inc()
		close(s.ready)
		return
	}

	slog.Info("streamer: session started", "kind", string(kind), "session_id", sessionIDOf(s), "dir", dir,
		"profiles", len(ls.cfg.Profiles))
	close(s.ready)

	chaseInput.copy(input, cmd.Process.Kill)

	waitErr := cmd.Wait()
	inputErr := chaseInput.finish(body)
	inputFailed = kind == chaseSessionKind && inputErr != nil && ctx.Err() == nil
	ffmpegCompleted := ffmpegSessionCompleted(ctx, cmd, waitErr, inputErr, kind, sessionIDOf(s), stderr)
	if (kind == chaseSessionKind || kind == originalVODSessionKind) && ctx.Err() == nil && ffmpegCompleted {
		// Keep completed recording playlists and all segments until the shared idle
		// GC reclaims the session, so clients can fetch ENDLIST and seek the full VOD.
		keepCompletedRecordingSession = true
	}
}

// probeLiveSessionInput reads a finite input prefix, probes its audio/subtitle streams, and
// returns a reader that replays the prefix before the remaining live input. For an opened
// original VOD file it uses ReadAt so ffmpeg retains the seekable input at offset zero.
func probeLiveSessionInput(
	ctx context.Context,
	ffprobe string,
	kind sessionKind,
	sessionID int64,
	body io.ReadCloser,
	originalFile *os.File,
) (io.ReadCloser, liveStreamInfo, error) {
	input := body
	var prefix []byte
	var readErr error
	if originalFile != nil {
		prefix = make([]byte, liveStreamProbeBytes)
		n, err := originalFile.ReadAt(prefix, 0)
		prefix = prefix[:n]
		readErr = err
	} else {
		input, prefix, readErr = readLiveStreamPrefix(ctx, body, liveStreamProbeBytes, liveStreamProbeWait)
	}
	if ctx.Err() != nil {
		_ = input.Close()
		return nil, liveStreamInfo{}, ctx.Err()
	}
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		slog.Warn("streamer: reading probe prefix failed; using single audio ES fallback",
			"kind", string(kind), "session_id", sessionID, "err", readErr)
	}
	streamInfo, err := probeLiveStreamInfo(ctx, ffprobe, prefix)
	if err != nil {
		if ctx.Err() != nil {
			_ = input.Close()
			return nil, liveStreamInfo{}, ctx.Err()
		}
		slog.Warn("streamer: probing live stream failed; using single audio ES fallback",
			"kind", string(kind), "session_id", sessionID, "err", err)
		streamInfo.audioStreams = 1
	}
	return input, streamInfo, nil
}

// recordFailedChaseInputLocked は ls.mu を保持した状態で追っかけ入力の cooldown を記録する。
func (ls *LiveStreamer) recordFailedChaseInputLocked(recordingID int64) {
	if ls.failedChaseInputs == nil {
		ls.failedChaseInputs = make(map[int64]time.Time)
	}
	ls.failedChaseInputs[recordingID] = time.Now().Add(chaseInputFailureCooldown)
}

// ffmpegSessionCompleted は ffmpeg が完走したか（ENDLIST を書いて正常終了したか）を判定し、
// 終わり方をログに残す。inputErr は追っかけの入力のエラー（chaseInputCopy.finish）。
func ffmpegSessionCompleted(ctx context.Context, cmd *exec.Cmd, waitErr, inputErr error, kind sessionKind, sessionID int64, stderr *cappedWriter) bool {
	// 入力のエラーで kill した（idle GC / shutdown の ctx キャンセルは除く）。ffmpeg の
	// 異常終了ではないので、その旨を 1 回だけ記録する。
	inputFailed := inputErr != nil && ctx.Err() == nil
	if inputFailed {
		slog.Error("streamer: chase input failed; killed ffmpeg so the playlist does not get ENDLIST",
			"session_id", sessionID, "err", inputErr)
		// 入力の失敗と同じころに ffmpeg が自分で落ちていたら（kill より先に終わっていて、
		// 終わり方が SIGKILL でない）、その落ち方も stderr ごと残す
		// （TestFFmpegSessionCompletedKeepsCrashBesideInputFailure）。
		if ffmpegExitedOnItsOwn(waitErr) {
			slog.Error("streamer: ffmpeg exited unexpectedly",
				"kind", string(kind), "session_id", sessionID, "err", waitErr, "stderr", strings.TrimSpace(stderr.String()))
		}
	}
	ffmpegCompleted := waitErr == nil && !inputFailed
	if waitErr != nil && ctx.Err() == nil && !inputFailed {
		if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			ffmpegCompleted = true
			// ffmpeg 自体は exit 0 で完走したが、孫プロセスが stdin/stderr の
			// fd を握ったままで WaitDelay が先に切れた（internal/worker の
			// runEncode / commandOutput と同型のハングの exit 0 版）。正常な
			// セッション終了なので運用者向けの Error にはしない。
			slog.Warn("streamer: ffmpeg exited successfully but WaitDelay expired before I/O completed",
				"kind", string(kind), "session_id", sessionID, "wait_delay", cmd.WaitDelay)
		} else {
			// ctx.Err() == nil ということは idle GC / shutdown による意図した kill ではない
			// ---ffmpeg 自身が落ちた（mirakc 側の切断、コーデックエラー等）。
			slog.Error("streamer: ffmpeg exited unexpectedly",
				"kind", string(kind), "session_id", sessionID, "err", waitErr, "stderr", strings.TrimSpace(stderr.String()))
		}
	}
	return ffmpegCompleted
}

// committedOriginalSize は録画のコミット済み原本（active）のバイト数を返す関数を作る。
// DB が無い構成（テスト）では nil で、404 は常にエラーになる。
func (ls *LiveStreamer) committedOriginalSize(recordingID int64) chaseCommittedSize {
	if ls.pool == nil {
		return nil
	}
	return func(ctx context.Context) (int64, bool, error) {
		row, err := sqlcgen.New(ls.pool).GetActiveOriginalMediaAsset(ctx, recordingID)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		return row.SizeBytes, true, nil
	}
}

// ffmpegExitedOnItsOwn は Wait のエラーが、こちらの kill（SIGKILL）以外での ffmpeg の
// 異常終了（0 以外の終了コードか、SIGKILL 以外のシグナル）か。
func ffmpegExitedOnItsOwn(waitErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL
}

// chaseInputCopy は追っかけの ffmpeg の stdin のパイプと、そこへの写し（copyChaseInput）を持つ。
// 入力のエラーで stdin を閉じる前に ffmpeg を kill するためで、os/exec の Stdin（io.Reader）に
// 任せると入力のエラーでもパイプが閉じられ、StdinPipe では Wait も書き込み側を閉じる。
// nil のメソッドは何もしない（追っかけ以外のセッション）。
type chaseInputCopy struct {
	read, write *os.File
	done        chan struct{}
	err         error
	// finishing は finish が入力と stdin を閉じ始めたこと。その後の入力のエラーは finish が
	// 起こしたもので、入力の失敗ではない（ffmpeg が自分で終わった）。
	finishing atomic.Bool
}

// attachChaseInput は cmd の stdin にパイプの読み側を付ける。Start の前に呼ぶ。
func attachChaseInput(cmd *exec.Cmd) (*chaseInputCopy, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("opening chase ffmpeg stdin: %w", err)
	}
	cmd.Stdin = pr
	return &chaseInputCopy{read: pr, write: pw}, nil
}

// started は Start の直後に呼ぶ。読み側は子に渡したので親の分を閉じる（持ち続けると、
// 書き側を閉じても子に EOF が届かない）。Start が失敗したら書き側も閉じる。
func (c *chaseInputCopy) started(startErr error) {
	if c == nil {
		return
	}
	_ = c.read.Close()
	if startErr != nil {
		_ = c.write.Close()
	}
}

// copy は写しを goroutine で始める。
func (c *chaseInputCopy) copy(input io.Reader, kill func() error) {
	if c == nil {
		return
	}
	c.done = make(chan struct{})
	go func() {
		defer close(c.done)
		c.err = copyChaseInput(c.write, input, kill, &c.finishing)
	}()
}

// finish は Wait の後に呼び、写しが入力のエラーで ffmpeg を kill したならそのエラーを返す
// （それ以外は nil）。ffmpeg が自分で終わったとき、写しは入力の Read か stdin への Write で
// 待っているかもしれない。入力と stdin の書き側を閉じて抜けさせてから待つ。stdin を閉じるのは、
// ffmpeg の孫が読み側を握ったまま読まないと、パイプが埋まった Write が終わらないためである
// （TestChaseInputCopyFinishUnblocksStuckWrite）。
func (c *chaseInputCopy) finish(input io.Closer) error {
	if c == nil || c.done == nil {
		return nil
	}
	c.finishing.Store(true)
	_ = input.Close()
	_ = c.write.Close()
	<-c.done
	return c.err
}

// copyChaseInput は追っかけの入力を ffmpeg の stdin へ写す。ffmpeg は stdin の EOF で
// ENDLIST を書くので、stdin を閉じるのは入力が正常な EOF で終わったときだけにする。入力が
// エラーで終わったら、先に kill してから stdin を閉じる（kill の後の ffmpeg は何も実行しない）。
// そうしないと途中までの入力に ENDLIST が付く（TestChaseInputErrorDoesNotWriteEndlist）。
// finishing が立った後の入力のエラーは finish が閉じたためなので kill せず nil を返す
// （TestChaseFFmpegCrashIsNotReportedAsInputFailure）。戻り値は kill した入力のエラー。
func copyChaseInput(stdin io.WriteCloser, input io.Reader, kill func() error, finishing *atomic.Bool) error {
	buf := make([]byte, 64<<10)
	for {
		n, readErr := input.Read(buf)
		if n > 0 {
			if _, writeErr := stdin.Write(buf[:n]); writeErr != nil {
				_ = stdin.Close()
				//nolint:nilerr // ffmpeg が先に終わったか finish が閉じた。終わり方は Wait が扱う。
				return nil
			}
		}
		switch {
		case readErr == nil:
		case errors.Is(readErr, io.EOF):
			_ = stdin.Close()
			return nil
		case finishing.Load():
			_ = stdin.Close()
			return nil
		default:
			_ = kill()
			_ = stdin.Close()
			return readErr
		}
	}
}

func cleanupSessionDir(s *liveSession) {
	if s.dir == "" {
		return
	}
	if err := os.RemoveAll(s.dir); err != nil {
		slog.Warn("streamer: segment cleanup failed", "kind", string(sessionKindOf(s)),
			"session_id", sessionIDOf(s), "dir", s.dir, "err", err)
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
	for recordingID, retryAt := range ls.failedChaseInputs {
		if !now.Before(retryAt) {
			delete(ls.failedChaseInputs, recordingID)
		}
	}
	var idle []*liveSession
	for id, s := range ls.sessions {
		if s.idleSince(now) >= ls.cfg.IdleTimeout {
			idle = append(idle, s)
			// 即座にマップから外す。新しい要求が stop() の完了を待たずに
			// 別のセッションを起こせるようにする。
			delete(ls.sessions, id)
		}
	}
	for key, s := range ls.chaseSessions {
		if s.idleSince(now) >= ls.cfg.IdleTimeout {
			idle = append(idle, s)
			// 即座にマップから外す。新しい要求が stop() の完了を待たずに
			// 別のセッションを起こせるようにする。
			delete(ls.chaseSessions, key)
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
			slog.Info("streamer: session idle, stopping", "kind", string(sessionKindOf(s)), "session_id", sessionIDOf(s))
			s.stop()
			// A chase session whose ffmpeg already reached EOF keeps its EVENT
			// files until this common idle-GC path. Live cleanup is idempotent.
			cleanupSessionDir(s)
			metrics.LiveIdleGCReclaimed.Inc()
		}(s)
	}
	wg.Wait()

	ls.setActiveSessionMetrics()
}

// shutdown はプロセス停止時に呼ぶ。新規セッションの受付を止め、既存の全セッションを
// 止めて mirakc の接続を閉じる（チューナー解放）。
//
// reapIdle と同じ理由で並行に stop() する（1 本が詰まっても他のチューナー解放を
// 遅らせない。SIGTERM の drain 猶予は有限）。
func (ls *LiveStreamer) shutdown() {
	ls.mu.Lock()
	ls.closed = true
	sessions := make([]*liveSession, 0, len(ls.sessions)+len(ls.chaseSessions))
	for _, s := range ls.sessions {
		sessions = append(sessions, s)
	}
	for _, s := range ls.chaseSessions {
		sessions = append(sessions, s)
	}
	ls.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *liveSession) {
			defer wg.Done()
			s.stop()
			cleanupSessionDir(s)
		}(s)
	}
	wg.Wait()
	ls.mu.Lock()
	for _, s := range sessions {
		if cur, ok := ls.getSessionLocked(s.key); ok && cur == s {
			ls.deleteSessionLocked(s)
		}
	}
	ls.mu.Unlock()
	ls.setActiveSessionMetrics()
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
// ランタイムでは ffprobe が数えた音声 ES 数を使い、複数なら 2 本目も map する。
// この公開 builder は dual mono 用の 1 ES として引数を組み立てる。字幕は通常 map
// しない。Captions=true の専用経路だけ optional に ARIB caption を map し、
// libaribcaption で WebVTT にする。既定経路は Debian 系 ffmpeg でも従来どおり動く。
//
// **音声はプロファイルごとに 3 本の代替音声レンディション（標準 / 主 / 副）で出す。**
// 標準はフィルタ無し（`DEFAULT=YES`）。単一 ES は主 / 副を pan し、複数 ES は
// 2 本目を副へ割り当てる。選ぶのはプレイヤーで、サーバーは選択を知らない
// （docs/api/media.md §音声）。
//
// argv の順序（issue #321 決定コメント §3）:
//
//	-hide_banner -nostats -loglevel error
//	[cfg.HWAccel ブロック]                          # 入力 1 本ぶん、1 回だけ
//	-probesize 5M -analyzeduration 3M
//	[cfg.InputExtraArgs…]
//	-f mpegts -i pipe:0
//	  ── プロファイルごとに繰り返し ──
//	  -map 0:v:0 -map 0:a:0 ×2 -map 0:a:0|0:a:1  -c:v  -c:a [single ES の pan]
//	  [-vf <deinterlace[, scaler が決めた scale]>]  [-crf|-qp]  [-preset]
//	  （captions 経路では `-c:a:N` / `-filter:v:N` / `-filter:a:N` を使う）
//	  -force_key_frames expr:…
//	  [profile.extra_args…]                         # ユーザー（出力側）
//	  -var_stream_map … -master_pl_name NAME.m3u8 -f hls ... NAME.%v.m3u8  # アプリ所有の末尾
//
// **既定経路はプロファイルごとの出力のまま、各出力が自分の master（`NAME.m3u8`）を
// 持つ。** 1 つの master にまとめると `hls_time` が 1 つになり、プロファイルごとの
// `segment_seconds` が表現できなくなる（captions 経路はそのため検証で揃えさせている）。
// variant のファイル名は `NAME.<n>.m3u8` で、プロファイル名に `.` は使えないので
// 別プロファイルの master と衝突しない。
//
// **Captions=true のときは 1 つの master playlist（%v 展開）を出す形に分岐する。**
// withSubtitles は Captions=true のときだけ効き、起動前の ffprobe 判定結果を渡す
// （false なら字幕 map / rendition を完全に省き、字幕の無い番組でも映像・音声の
// HLS を継続できる）。Captions=false のときは無視される。
func BuildLiveFFmpegArgs(cfg LiveConfig, dir string, withSubtitles bool) []string {
	return buildHLSFFmpegArgsForPlaylistType(cfg, dir, withSubtitles, hlsLivePlaylist, "pipe:0", 0, 1)
}

// BuildChaseFFmpegArgs は live と同じ画質・音声 rendition を出し、EVENT playlist にする。
// EVENT は録画履歴全体を残すため delete_segments を使わない。
//
// **追っかけもライブ・原本 HLS と同じ音声選択（標準 / 主 / 副）を提供する。** EVENT は
// segment を保持するためライブ窓のずれは起きない。実ブラウザの切替・シーク・再開の
// 判定は web/e2e/chase-audio.mjs が担う。
func BuildChaseFFmpegArgs(cfg LiveConfig, dir string, withSubtitles bool) []string {
	return buildHLSFFmpegArgsForPlaylistType(cfg, dir, withSubtitles, hlsEventPlaylist, "pipe:0", 0, 1)
}

// BuildOriginalVODFFmpegArgs converts the original MPEG-2 TS into an EVENT HLS
// playlist that grows while ffmpeg converts and gains ENDLIST at EOF. It is not
// `-hls_playlist_type vod`: ffmpeg 9.0.2 writes no .m3u8 until it exits in that
// mode (measured), so a recording longer than playlistStartupTimeout would never
// become playable. Every segment is kept until shared idle GC, and the output
// has the same profile, audio rendition, and optional subtitle graph as live.
// offsetSeconds is mapped down to the 30000/1001 fps input frame grid before
// accurate input-side -ss, and the video encoder gets -bf 0. Measured only on
// the synthetic MPEG-2 fixture of web/e2e/recording-playback-timeline.mjs with
// libx264 (Chrome shows hls.js frames 66.73 ms late without -bf 0; WebKit's
// native HLS showed no difference). Unverified: the captions path's effect in a
// browser, hardware encoders, and recordings whose audio lead has a phase other
// than the fixture's (audio start 10.4067 s, video start 11.1007 s; an integer
// -ss showed frames 33.37 ms early there). fd 3 is the original opened by the Go
// process and passed via Cmd.ExtraFiles, so unlinking its canonical path cannot
// break the session.
func BuildOriginalVODFFmpegArgs(cfg LiveConfig, dir string, withSubtitles bool, offsetSeconds int64) []string {
	return buildHLSFFmpegArgsForPlaylistType(
		cfg, dir, withSubtitles, hlsOriginalEventPlaylist, originalVODFFmpegInputPath, offsetSeconds, 1,
	)
}

// appendMPEGTSInput は MPEG-TS 入力（`-f mpegts [-ss N] -i path`）を args に足す。
// offsetSeconds > 0 のときだけ入力側シークを付ける。
func appendMPEGTSInput(args []string, inputPath string, offsetSeconds int64) []string {
	return appendMPEGTSInputWithSeek(args, inputPath, offsetSeconds, strconv.FormatInt(offsetSeconds, 10))
}

func appendOriginalVODMPEGTSInput(args []string, inputPath string, offsetSeconds int64) []string {
	if offsetSeconds <= 0 {
		return appendMPEGTSInput(args, inputPath, 0)
	}
	frame := offsetSeconds * chapters.FrameNumerator / chapters.FrameDenominator
	seekSeconds := float64(frame) * float64(chapters.FrameDenominator) / float64(chapters.FrameNumerator)
	return appendMPEGTSInputWithSeek(args, inputPath, offsetSeconds, fmt.Sprintf("%.9f", seekSeconds))
}

func appendMPEGTSInputWithSeek(args []string, inputPath string, offsetSeconds int64, seek string) []string {
	args = append(args, "-f", "mpegts")
	if offsetSeconds > 0 {
		args = append(args, "-ss", seek)
	}
	return append(args, "-i", inputPath)
}

type hlsPlaylistType uint8

const (
	hlsLivePlaylist hlsPlaylistType = iota
	hlsEventPlaylist
	// hlsOriginalEventPlaylist is an EVENT playlist like chase, but reads the original
	// file with a frame-aligned -ss and encodes with -bf 0 so the HLS timeline matches
	// the original MP4 timeline. Its hls_flags are the same as chase's.
	hlsOriginalEventPlaylist
)

func buildHLSFFmpegArgsForPlaylistType(
	cfg LiveConfig,
	dir string,
	withSubtitles bool,
	playlistType hlsPlaylistType,
	inputPath string,
	offsetSeconds int64,
	audioStreamCount int,
) []string {
	originalVOD := playlistType == hlsOriginalEventPlaylist
	if cfg.Captions {
		return buildLiveCaptionFFmpegArgsForPlaylistType(
			cfg, dir, withSubtitles, playlistType, inputPath, offsetSeconds, audioStreamCount,
		)
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
	if originalVOD {
		args = appendOriginalVODMPEGTSInput(args, inputPath, offsetSeconds)
	} else {
		args = appendMPEGTSInput(args, inputPath, offsetSeconds)
	}
	for _, p := range cfg.Profiles {
		// 映像・音声だけ。字幕 / データ放送は捨てる（上記 arib_caption）。
		// -map は output 単位のオプションなので、ループの前に 1 組だけ置くと
		// 最初の .m3u8 にしか適用されず、2 本目以降は自動ストリーム選択に戻る。
		args = append(args, "-map", "0:v:0")
		args = appendAudioRenditionMaps(args, audioStreamCount)
		args = append(args, "-c:v", p.VideoCodec, "-c:a", p.AudioCodec)
		args = appendAudioRenditionFilters(args, 0, audioStreamCount)
		if originalVOD {
			// With B frames, Chrome/hls.js showed frames 2 frames (66.73 ms) behind the
			// original MP4 timeline on the synthetic fixture (e2e
			// recording-playback-timeline, libx264). Unverified for hardware encoders.
			args = append(args, "-bf", "0")
		}
		if filter, ok := ffargs.VideoFilterArgs(p.Scaler, p.Height, p.Deinterlace); ok {
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
		playlistSize := strconv.Itoa(p.PlaylistSize)
		playlistOptions := []string{}
		if playlistType != hlsLivePlaylist {
			// EVENT playlists grow from the head until ffmpeg sees EOF. list_size 0
			// and the absence of delete_segments retain every segment for seeking.
			playlistSize = "0"
			playlistOptions = []string{"-hls_playlist_type", "event"}
		}
		// 出力ファイル名は master（NAME.m3u8）と variant（NAME.<n>.m3u8）。
		// 字幕付きは playlist.m3u8 と playlist_<n>.m3u8。
		variants := append([]string{"v:0,agroup:aud"}, audioRenditionEntries(0, "aud")...)
		args = append(args, "-var_stream_map", strings.Join(variants, " "), "-master_pl_name", p.Name+".m3u8")
		segmentFile, playlistFile := p.Name+".%v_seg%05d.ts", p.Name+".%v.m3u8"
		args = append(args,
			"-f", "hls",
			"-hls_time", strconv.Itoa(p.SegmentSeconds),
			"-hls_list_size", playlistSize,
		)
		args = append(args, playlistOptions...)
		args = append(args,
			// delete_segments: プレイリスト長を超えた古いセグメントを削除する
			//（プロセスが落ちても残骸を溜め続けない。正常系の掃除）。
			// temp_file: 一時ファイルに書いてから rename するので、配信側が
			// 書き込み途中のファイルを読むことがない。追っかけ再生は
			// delete_segments を使わない（BuildChaseFFmpegArgs）。
			"-hls_flags", hlsFlagsForPlaylistType(playlistType),
			"-hls_segment_filename", filepath.Join(dir, "segments", segmentFile),
			// hls_base_url: プレイリストの各セグメント行に付ける接頭辞。
			// **これが無いと ffmpeg は basename だけを書く**（実機で確認済み）。
			// HLS クライアントはプレイリスト自身の URL 基準で相対解決するため、
			// basename のままだと `.../live/h264_seg00001.ts` を要求してしまい、
			// このサーバーが実際に配信するルート（`.../live/segments/{name}`）と
			// 食い違って 404 になる。`-hls_segment_filename` が書き込む物理パス
			// （`segments/` サブディレクトリ）と、プレイリストが指す論理 URI を
			// 一致させるための必須フラグ（issue #91 のレビューで発見）。
			"-hls_base_url", "segments/",
			filepath.Join(dir, playlistFile),
		)
	}
	return args
}

// appendAudioRenditionMaps appends input maps for the standard, main, and sub
// renditions. A single audio ES carries both channels of dual-mono audio; with
// separate ESs, the first is standard/main and the second is sub.
func appendAudioRenditionMaps(args []string, audioStreamCount int) []string {
	inputs := [3]string{"0:a:0", "0:a:0", "0:a:0"}
	if audioStreamCount >= 2 {
		inputs[2] = "0:a:1"
	}
	for _, input := range inputs {
		args = append(args, "-map", input)
	}
	return args
}

// appendAudioRenditionFilters adds the pan filters used when main/sub are the
// left and right channels of one dual-mono audio ES. Separate audio ESs already
// contain the selected language, so those renditions keep the source channels.
func appendAudioRenditionFilters(args []string, firstAudioOutput, audioStreamCount int) []string {
	if audioStreamCount >= 2 {
		return args
	}
	return append(args,
		"-filter:a:"+strconv.Itoa(firstAudioOutput+1), dualMonoPans[0],
		"-filter:a:"+strconv.Itoa(firstAudioOutput+2), dualMonoPans[1],
	)
}

// dualMonoPans は二重音声の主（L）/ 副（R）を両耳へ写す出力側のフィルタ。
//
// 既定のデコード（`-dual_mono_mode` 無し）は二重音声を L = 主 / R = 副のステレオで
// 出すので、出力側で片側を両耳へ写せば主 / 副になる。**`-dual_mono_mode` は入力
// （デコーダ）側のオプションなので 1 回の起動で両方を出せないが、これなら出せる。**
// 実測（ffmpeg 9.0.2）: モノラル AAC 2 本の SCE を 1 フレームに継いだ二重音声で、
// この pan の出力は `-dual_mono_mode main|sub` の出力とバイト一致した。二重音声で
// ない通常のステレオに当てると片側のチャンネルだけになる（利用者が選んだときだけ）。
var dualMonoPans = [2]string{"pan=stereo|c0=c0|c1=c0", "pan=stereo|c0=c1|c1=c1"}

// audioRenditionEntries は 1 プロファイルぶんの音声レンディション（標準 / 主 / 副）の
// `-var_stream_map` 項目を返す。first はその最初の音声出力ストリームの index。
//
// **並び順が UI との契約である。** master の `NAME` は ffmpeg が `audio_<n>` で固定し、
// n はプロファイル数でずれる（`name:` で変わるのは URI だけ。実測）。フロントは
// グループ内の順序（0 = 標準 / 1 = 主 / 2 = 副）で選ぶ（web/src/lib/live.ts
// liveAudioTrackIndex）。
func audioRenditionEntries(first int, group string) []string {
	return []string{
		fmt.Sprintf("a:%d,agroup:%s,default:yes", first, group),
		fmt.Sprintf("a:%d,agroup:%s", first+1, group),
		fmt.Sprintf("a:%d,agroup:%s", first+2, group),
	}
}

// hlsFlags は `-hls_flags` の値を返す。
//
// **ライブは program_date_time が要る。** 無いと hls.js（1.7.1 / 1.7.3 / canary）は、
// 前に聴いた音声レンディションへ**ライブの窓（list_size × hls_time）より後で**戻ると
// 再生が止まる（バッファが空になり、音声は前のトラックのまま。手元に残った古い
// playlist が今の窓と重ならず、PDT 無しでは揃えられない）。窓の内側ですぐ戻る分には
// 止まらない。判定は `web/e2e/live-audio.mjs` の ①（各トラックを 15 秒聴いてから
// 戻る。PDT を外すと標準へ戻る所で落ち、付けると通る。WebAudio で左右の周波数を
// 測る）。追っかけは EVENT playlist で segment を消さず窓がスライドしないので、PDT が
// 無くても各トラックを 15 秒聴いて戻っても止まらない（`web/e2e/chase-audio.mjs`。
// hls.js が何で位置を揃えているかは測っていない）。
func hlsFlags(eventPlaylist bool) string {
	if eventPlaylist {
		return "temp_file"
	}
	return "delete_segments+temp_file+program_date_time"
}

func hlsFlagsForPlaylistType(playlistType hlsPlaylistType) string {
	return hlsFlags(playlistType != hlsLivePlaylist)
}

// buildLiveCaptionFFmpegArgsForPlaylistType は HLS を 1 つの master playlist として出力する。
// %v はプロファイルごとの video/audio variant を表す。withSubtitles は起動前の
// ffprobe 判定結果で、false の場合は字幕 map / rendition を完全に省き、字幕なし
// 番組でも映像・音声の HLS を継続できる。
//
// **音声レンディションはプロファイルごとのグループ（`agroup:a<N>`）に入れる。**
// プロファイルごとに `audio_codec` / `extra_args` が違いうるので、1 グループに
// まとめるとそれが表現できない。プロファイル N の音声は a:3N（標準）/ a:3N+1（主）/
// a:3N+2（副）。video variant が先に並ぶので字幕 playlist は `subtitles_0.m3u8` のまま。
//
// **per-stream 指定子は必ず型付き（`:v:N` / `:a:N`）にする。** この経路の出力
// ストリーム順は v0, a0..a2, [s0,] v1, a3..a5 で、`-preset:N` / `-vf:N`（型無しの
// グローバル出力ストリーム index）は 2 本目以降のプロファイルでは音声側を指して
// しまい、preset もフィルタも掛からない（実 ffmpeg で測定・固定: レビュー指摘）。
//
// **フィルタは `-filter:v:N`（`-vf` の完全形）にする。**`-vf:v:N`（`-vf` に型
// 付き specifier を重ねる書き方）は ffmpeg 9.0.1 で単一出力・複数 video map の
// 構成において機能しない（specifier が意図通り分離されず、最後に指定した
// フィルタが両方の video ストリームに適用されて警告が出ることを実測で確認。
// `-c:v:N` や `-preset:v:N` のような型を伴わない他オプションでの `:v:N` 付与は
// 問題なく機能する --- `-vf`/`-filter:v` だけの挙動）。
func buildLiveCaptionFFmpegArgsForPlaylistType(
	cfg LiveConfig,
	dir string,
	withSubtitles bool,
	playlistType hlsPlaylistType,
	inputPath string,
	offsetSeconds int64,
	audioStreamCount int,
) []string {
	originalVOD := playlistType == hlsOriginalEventPlaylist
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
	if originalVOD {
		args = appendOriginalVODMPEGTSInput(args, inputPath, offsetSeconds)
	} else {
		args = appendMPEGTSInput(args, inputPath, offsetSeconds)
	}

	var variants, audioVariants []string
	for i, p := range cfg.Profiles {
		args = append(args, "-map", "0:v:0")
		args = appendAudioRenditionMaps(args, audioStreamCount)
		if i == 0 && withSubtitles {
			args = append(args, "-map", "0:s:0?")
		}
		a := 3 * i
		args = append(args, "-c:v:"+strconv.Itoa(i), p.VideoCodec)
		if originalVOD {
			args = append(args, "-bf:v:"+strconv.Itoa(i), "0")
		}
		for output := 0; output < 3; output++ {
			stream := a + output
			args = append(args, "-c:a:"+strconv.Itoa(stream), p.AudioCodec)
			if audioStreamCount < 2 && output > 0 {
				args = append(args, "-filter:a:"+strconv.Itoa(stream), dualMonoPans[output-1])
			}
		}
		if filter, ok := ffargs.VideoFilterArgs(p.Scaler, p.Height, p.Deinterlace); ok {
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
		group := "a" + strconv.Itoa(i)
		mapping := fmt.Sprintf("v:%d,agroup:%s", i, group)
		audioVariants = append(audioVariants, audioRenditionEntries(a, group)...)
		if i == 0 && withSubtitles {
			mapping += ",s:0,sgroup:subs"
		}
		variants = append(variants, mapping)
	}
	playlistSize := strconv.Itoa(cfg.Profiles[0].PlaylistSize)
	playlistOptions := []string{}
	if playlistType != hlsLivePlaylist {
		playlistSize = "0"
		playlistOptions = []string{"-hls_playlist_type", "event"}
	}
	args = append(args,
		"-var_stream_map", strings.Join(append(variants, audioVariants...), " "),
		"-master_pl_name", "playlist.m3u8",
		"-f", "hls",
		"-hls_time", strconv.Itoa(cfg.Profiles[0].SegmentSeconds),
		"-hls_list_size", playlistSize,
	)
	args = append(args, playlistOptions...)
	args = append(args,
		"-hls_flags", hlsFlagsForPlaylistType(playlistType),
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

type liveStreamPrefixPump struct {
	mu          sync.Mutex
	limit       int
	prefix      []byte
	probing     bool
	prefixReady chan struct{}
	readErr     error
}

func (p *liveStreamPrefixPump) finishProbeLocked(err error) {
	if !p.probing {
		return
	}
	p.probing = false
	p.readErr = err
	close(p.prefixReady)
}

func (p *liveStreamPrefixPump) snapshotAndStop() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishProbeLocked(nil)
	return append([]byte(nil), p.prefix...), p.readErr
}

func (p *liveStreamPrefixPump) copy(body io.Reader, writer *io.PipeWriter) {
	buf := make([]byte, 64*1024)
	for {
		n, readErr := body.Read(buf)
		remainder := buf[:n]
		p.mu.Lock()
		if p.probing && n > 0 {
			take := min(n, p.limit-len(p.prefix))
			p.prefix = append(p.prefix, buf[:take]...)
			remainder = buf[take:n]
			if len(p.prefix) == p.limit {
				p.finishProbeLocked(nil)
			}
		}
		if readErr != nil {
			p.finishProbeLocked(readErr)
		}
		p.mu.Unlock()

		if len(remainder) > 0 {
			if _, err := writer.Write(remainder); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				_ = writer.Close()
			} else {
				_ = writer.CloseWithError(readErr)
			}
			return
		}
	}
}

type liveStreamPrefixReplay struct {
	reader io.Reader
	pipe   *io.PipeReader
}

func (r *liveStreamPrefixReplay) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *liveStreamPrefixReplay) Close() error {
	return r.pipe.Close()
}

// readLiveStreamPrefix asynchronously buffers up to limit bytes for ffprobe, then
// returns a reader that replays that prefix before the rest of body. It stops
// waiting after wait even when an upstream Read is blocked, so a chase playlist
// can start before a growing recording produces more data. The pump owns body
// reads until it reaches EOF or body is closed.
func readLiveStreamPrefix(
	ctx context.Context,
	body io.ReadCloser,
	limit int,
	wait time.Duration,
) (input io.ReadCloser, prefix []byte, err error) {
	pipeReader, pipeWriter := io.Pipe()
	pump := &liveStreamPrefixPump{
		limit:       limit,
		prefix:      make([]byte, 0, limit),
		probing:     true,
		prefixReady: make(chan struct{}),
	}
	go pump.copy(body, pipeWriter)

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-pump.prefixReady:
	case <-timer.C:
	case <-ctx.Done():
	}
	prefix, err = pump.snapshotAndStop()
	input = &liveStreamPrefixReplay{
		reader: io.MultiReader(bytes.NewReader(prefix), pipeReader),
		pipe:   pipeReader,
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return input, prefix, err
}

type liveStreamInfo struct {
	audioStreams int
	hasSubtitles bool
}

// probeLiveStreamInfo は ffprobe に MPEG-TS の有限な先頭部分だけを渡し、音声 ES 数と
// 字幕の有無を調べる。アプリケーション自身は TS/PES や放送記述子を解釈しない。
func probeLiveStreamInfo(ctx context.Context, ffprobe string, prefix []byte) (liveStreamInfo, error) {
	ffprobe = ffargs.FFprobePath(ffprobe)
	probeCtx, cancel := context.WithTimeout(ctx, liveStreamProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, ffprobe,
		"-v", "error", "-probesize", "5M", "-analyzeduration", "3M",
		"-show_entries", "stream=codec_type", "-of", "json", "-i", "pipe:0",
	)
	cmd.Stdin = bytes.NewReader(prefix)
	out, err := cmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return liveStreamInfo{}, probeCtx.Err()
		}
		return liveStreamInfo{}, fmt.Errorf("running ffprobe: %w", err)
	}
	var result struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return liveStreamInfo{}, fmt.Errorf("decoding ffprobe stream list: %w", err)
	}
	info := liveStreamInfo{}
	for _, stream := range result.Streams {
		switch stream.CodecType {
		case "audio":
			info.audioStreams++
		case "subtitle":
			info.hasSubtitles = true
		}
	}
	if info.audioStreams == 0 {
		return liveStreamInfo{}, errors.New("ffprobe found no audio stream")
	}
	return info, nil
}
