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
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

func writeHLSPlaylist(w http.ResponseWriter, content []byte) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(content)
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

const (
	liveSessionKind        sessionKind = "live"
	chaseSessionKind       sessionKind = "chase"
	originalVODSessionKind sessionKind = "original_vod"
)

const (
	hlsLivePlaylist hlsPlaylistType = iota
	hlsEventPlaylist
	// hlsOriginalEventPlaylist is an EVENT playlist like chase, but reads the original
	// file with a frame-aligned -ss and encodes with -bf 0 so the HLS timeline matches
	// the original MP4 timeline. Its hls_flags are the same as chase's.
	hlsOriginalEventPlaylist
)

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
