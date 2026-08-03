package worker

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/webhook"
)

const (
	defaultIngestConcurrency = 2
	defaultEpgSyncInterval   = 10 * time.Minute

	// defaultTunerSyncInterval はチューナー射影の既定間隔。
	//
	// チューナー構成の変更には mirakc の再起動が要るので更新頻度は低くてよく、
	// EPG 全量同期と同じ 10 分で足りる（issue #21「EPG 全量同期（既定 10 分）と
	// 同じジョブで投影すれば十分」）。専用の設定キーは設けていない --- 運用者が
	// 触る理由がないので、設定面を広げない。
	defaultTunerSyncInterval = 10 * time.Minute
)

// pendingJobStates は「まだ終わっていない」ジョブの状態。
//
// UniqueOpts.ByState に渡して、一意化の対象を実行前・実行中に限定する。
// River の既定（rivertype.UniqueOptsByStateDefault）は completed と discarded を
// 含むため、既定のままだと「一度成功した引数のジョブは二度と投入できない」に
// なってしまう。定期ジョブは実質ワンショットになり、失敗して破棄されたジョブも
// 再投入できない。
//
// 同時実行を防ぐのが目的なので、終わったジョブは一意性の判定から外す。
// 「もう処理済みか」は River のジョブ履歴ではなく DB の状態（media_assets 行の
// 有無など）が真実である。
var pendingJobStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// verifySite は、mirakc かファイルシステムに触るワーカーが処理しようとしている
// ジョブの site（jobSite）が、そのワーカープロセス自身の site（workerSite、
// config.mirakc.site）と一致するかを検査する fail-fast ガード（issue #139）。
//
// mirakc の record id / programId はインスタンススコープであり、DB の site 列は
// そのスコープの境界を表現するために存在する（issue #31、docs/schema.md
// §1-5）。River のキュー名は site で修飾されていない（根治は #138 が決める）ため、
// 多サイト構成では site A の worker が site B のジョブを掴みうる。掴んだまま
// mirakc/FS に触れると、A の mirakc に B の id を投げて A の別番組を B の
// recording としてコミットしうる（不変条件 3「コミット = DB 行」への違反。
// 詳細は issue #139 本文）。
//
// workerSite が空文字列（Deps.Site 未注入。単体テストの部分構成を含む）の場合は
// db.DefaultSite に解決してから比較する — DeleteReconcileWorker.Site と同じ
// 解決規則で、単一サイト構成でこのガードを追加してもテスト・運用のどちらも
// 壊れないようにする。
//
// 呼び出し側は mirakc/FS に触れる**前**（最初の HTTP 呼び出し・os.Create 等より
// 前）に呼ぶこと。合わないジョブは即座に失敗させ、再試行は River に委ねる
// （同じジョブが必ず自サイトの worker に回る保証はないが、他サイトの worker が
// いずれ拾う。うるさいが安全 --- issue #139 本文の「id が自サイトに存在しない」
// ケースと同じ扱いに揃う）。
func verifySite(workerSite, jobSite, queue string) error {
	site := workerSite
	if site == "" {
		site = db.DefaultSite
	}
	if jobSite != site {
		return fmt.Errorf(
			"%s: job site %q does not match this worker's site %q, refusing before touching mirakc/FS (issue #139)",
			queue, jobSite, site)
	}
	return nil
}

// Deps は各ワーカーに注入する依存。
type Deps struct {
	MirakcClient *mirakc.Client
	Pool         *pgxpool.Pool
	MediaDir     string
	ScratchDir   string

	// Site は config.mirakc.site（issue #31）。用途は 2 つ:
	//  1. mirakc/FS に触るワーカー（Ingest / EpgSync / TunerSync / ReconcilePass /
	//     RecordSweep）が、自分が処理するジョブの args.Site と自分自身の site を
	//     照合する fail-fast ガード（issue #139、verifySite）。
	//  2. DeleteReconcileWorker のように site 単位のジョブ引数を持たないが
	//     site をキーにする資源（サーキットブレーカー）を持つワーカーへの注入。
	// 空なら db.DefaultSite を使う（テストの部分構成を許す。verifySite・
	// DeleteReconcileWorker.Work のどちらも同じ解決規則）。
	Site string

	// Encode は構造化エンコードプロファイルと ffmpeg パス（issue #64 / #65）。
	// worker ロール起動時に ValidateTools 済み（不変条件 4）。
	Encode config.EncodeConfig

	// EpgRetentionGrace は放送済み番組を刈り取るまでの猶予。0 なら既定値。
	EpgRetentionGrace time.Duration

	// RulerRetentionGrace は ruler が reservations / program_intents を GC するまでの
	// 猶予。0 なら ruler 側の既定値を使う。epg.retention_grace と同じ値を流用する
	// 運用を想定している（docs/recording.md §3.2「番組終了後の GC」）。
	RulerRetentionGrace time.Duration

	// RulerMaxDeletesPerPass は大量削除サーキットブレーカーの閾値
	// （ruler.max_deletes_per_pass）。0 なら ruler 側の既定値を使う
	// （docs/recording.md §3.2「大量削除サーキットブレーカー」）。
	RulerMaxDeletesPerPass int

	// ReconcileStartDelayGrace は開始遅延検出器の猶予
	// （reconciler.start_delay_grace）。0 なら reconciler 側の既定値を使う
	// （docs/recording.md §3.3「開始遅延検出器」）。
	ReconcileStartDelayGrace time.Duration

	// IngestStallTimeout は ingest の無進捗検知タイムアウト
	// （ingest.stall_timeout）。0 なら IngestWorker 側の既定値（30 秒）を使う。
	// River の総時間タイムアウトは無効化しているため、これが ingest の唯一の
	// タイムアウトである（docs/recording.md §5.3「層 1」）。
	IngestStallTimeout time.Duration

	// Webhook は録画ライフサイクル通知用クライアント（M3-11）。nil 可。
	// recording.finished/failed は record_sweep 経由の processRecord から、
	// encode.finished/failed は EncodeWorker から、recording.deleted は
	// DeleteReconcileWorker から発火する（issue #73）。
	Webhook *webhook.Client

	// Cleanup は削除 reconcile（M3-8）の猶予・閾値設定。
	Cleanup config.CleanupConfig
}

// NewWorkers は全ワーカーを登録した river.Workers を返す。
func NewWorkers(deps *Deps) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &IngestWorker{
		MirakcClient: deps.MirakcClient,
		Pool:         deps.Pool,
		MediaDir:     deps.MediaDir,
		StallTimeout: deps.IngestStallTimeout,
		Site:         deps.Site,
	})
	river.AddWorker(workers, &EncodeWorker{
		Pool:       deps.Pool,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
		FFmpeg:     deps.Encode.FFmpeg,
		Profiles:   deps.Encode,
		Webhook:    deps.Webhook,
	})
	river.AddWorker(workers, &EncodeEnqueueHintWorker{
		Pool: deps.Pool,
	})
	river.AddWorker(workers, &EpgSyncWorker{
		MirakcClient:   deps.MirakcClient,
		Pool:           deps.Pool,
		RetentionGrace: deps.EpgRetentionGrace,
		Site:           deps.Site,
	})
	river.AddWorker(workers, &TunerSyncWorker{
		MirakcClient: deps.MirakcClient,
		Pool:         deps.Pool,
		Site:         deps.Site,
	})
	river.AddWorker(workers, &RulerPassWorker{
		Pool:              deps.Pool,
		RetentionGrace:    deps.RulerRetentionGrace,
		MaxDeletesPerPass: deps.RulerMaxDeletesPerPass,
	})
	river.AddWorker(workers, &ReconcilePassWorker{
		MirakcClient:    deps.MirakcClient,
		Pool:            deps.Pool,
		StartDelayGrace: deps.ReconcileStartDelayGrace,
		Site:            deps.Site,
	})
	river.AddWorker(workers, &RecordSweepWorker{
		MirakcClient: deps.MirakcClient,
		Pool:         deps.Pool,
		Webhook:      deps.Webhook,
		Site:         deps.Site,
	})
	river.AddWorker(workers, &CatalogExportWorker{
		Pool:     deps.Pool,
		MediaDir: deps.MediaDir,
	})
	river.AddWorker(workers, &ThumbnailWorker{
		Pool:       deps.Pool,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
		FFmpeg:     deps.Encode.FFmpeg,
		FFprobe:    deps.Encode.FFprobe,
	})
	river.AddWorker(workers, &DeleteReconcileWorker{
		Pool:              deps.Pool,
		MediaDir:          deps.MediaDir,
		Site:              deps.Site,
		TrashRetention:    deps.Cleanup.TrashRetention,
		OrphanMTimeGrace:  deps.Cleanup.OrphanMTimeGrace,
		OrphanAge:         deps.Cleanup.OrphanAge,
		MaxDeletesPerPass: deps.Cleanup.MaxDeletesPerPass,
		Webhook:           deps.Webhook,
	})
	return workers
}

const (
	// encodeQueue はエンコードジョブ専用のキュー名（M3-3 でワーカーを登録する）。
	encodeQueue = "encode"
	// thumbnailQueue はサムネイルジョブ専用のキュー名。
	thumbnailQueue = "thumbnail"

	defaultEncodeConcurrency    = 1
	defaultThumbnailConcurrency = 1
)

// allQueues はこのプロセスが知っている全キューとその設定。worker.queues
// （ClientConfig.Queues）で絞り込む際の許可リストにもなる。
func allQueues(ingestConcurrency, encodeConcurrency, thumbnailConcurrency int) map[string]river.QueueConfig {
	return map[string]river.QueueConfig{
		river.QueueDefault: {MaxWorkers: 100},
		ingestQueue:        {MaxWorkers: ingestConcurrency},
		// 全量同期が重ならないよう 1 本に絞る。tuner_sync も同じキューを使う
		// （どちらも使い捨てプロジェクションの全量同期。TunerSyncArgs.InsertOpts 参照）。
		epgQueue: {MaxWorkers: 1},
		// サイト単位の排他は UniqueOpts が担うので、こちらも複数ワーカーは要らない。
		rulerQueue: {MaxWorkers: 1},
		// reconciler も ruler と同じ理由（UniqueOpts がサイト単位の排他を担う）で 1 本に絞る。
		reconcilerQueue: {MaxWorkers: 1},
		// record_sweep も同じ理由（UniqueOpts がサイト単位の排他を担う）で 1 本に絞る。
		recordSweepQueue: {MaxWorkers: 1},
		// encode / thumbnail は CPU 拘束なので ingest とは独立に絞る（EPGStation#531、
		// issue #64）。thumbnail ワーカーは M3-4、encode ワーカーは M3-3。
		encodeQueue:    {MaxWorkers: encodeConcurrency},
		thumbnailQueue: {MaxWorkers: thumbnailConcurrency},
	}
}

// ClientConfig は River クライアントの設定。
type ClientConfig struct {
	// IngestConcurrency は ingest キューの同時実行数（サイト単位のキャップ）。0 なら既定値。
	IngestConcurrency int

	// EncodeConcurrency は encode キューの MaxWorkers。0 なら既定値 1。
	// encode.concurrency から注入する（issue #64）。
	EncodeConcurrency int

	// ThumbnailConcurrency は thumbnail キューの MaxWorkers。0 なら既定値 1。
	// encode.thumbnail_concurrency から注入する（issue #64）。
	ThumbnailConcurrency int

	// EpgSyncSite が空でない場合、そのサイトの EPG 全量同期を定期ジョブとして登録する
	// （PeriodicJobs が true のときのみ）。
	EpgSyncSite string

	// EpgSyncInterval は EPG 全量同期の間隔。0 なら既定値。
	EpgSyncInterval time.Duration

	// TunerSyncSite が空でない場合、そのサイトのチューナー射影を定期ジョブとして
	// 登録する（PeriodicJobs が true のときのみ）。
	TunerSyncSite string

	// TunerSyncInterval はチューナー射影の間隔。0 なら既定値（10 分）。
	TunerSyncInterval time.Duration

	// RulerPassSite が空でない場合、そのサイトの ruler 1 パス評価を定期ジョブとして
	// 登録する（PeriodicJobs が true のときのみ）。
	RulerPassSite string

	// RulerPassInterval は ruler 定期パスの間隔。0 なら既定値。
	RulerPassInterval time.Duration

	// ReconcilePassSite が空でない場合、そのサイトの reconciler 1 パス突き合わせを
	// 定期ジョブとして登録する（PeriodicJobs が true のときのみ）。
	ReconcilePassSite string

	// ReconcilePassInterval は reconciler 定期パスの間隔。0 なら既定値（30 秒）。
	ReconcilePassInterval time.Duration

	// RecordSweepSite が空でない場合、そのサイトの watcher 全量突き合わせ（(c)、
	// docs/recording.md §3.3）を定期ジョブとして登録する（PeriodicJobs が true のときのみ）。
	RecordSweepSite string

	// RecordSweepInterval は record_sweep 定期パスの間隔。0 なら既定値（5 分）。
	RecordSweepInterval time.Duration

	// CatalogExport が true なら catalog_export を定期ジョブとして登録する
	// （PeriodicJobs が true のときのみ）。サイト非依存の災害復旧バックアップなので
	// サイト引数は不要（docs/storage.md §8）。
	CatalogExport bool

	// CatalogExportInterval は catalog エクスポートの間隔。0 なら既定値（24 時間）。
	CatalogExportInterval time.Duration

	// DeleteReconcile が true なら削除 reconcile（M3-8）を定期ジョブとして登録する
	// （PeriodicJobs が true のときのみ）。CatalogExport と同じくサイト非依存
	// （物理ストレージは単一の media_dir）。
	DeleteReconcile bool

	// DeleteReconcileInterval は削除 reconcile の間隔。0 なら既定値（15 分）。
	DeleteReconcileInterval time.Duration

	// PeriodicJobs が false なら、EpgSyncSite / TunerSyncSite / RulerPassSite /
	// ReconcilePassSite / RecordSweepSite / CatalogExport / DeleteReconcile が
	// 設定されていても River の PeriodicJobs を一切登録しない。
	// k8s では false にして、CronJob が
	// `rokuban enqueue` を叩く形に委ねる（docs/data.md §2「定期実行の契機は
	// デプロイ形態に委ねる」。設定キーは worker.periodic_jobs、既定 true）。
	PeriodicJobs bool

	// Queues は実際に SKIP LOCKED で引くキューを絞り込む。空なら全キューを引く。
	// ロールを増やさずに「ruler / reconciler だけ別 Pod」のような構成を実現するための knob
	// （docs/configuration.md の worker.queues。docs/overview.md「ロールは
	// 『プロセスの形』を表し、『どの仕事をするか』は表さない」）。
	// 未知のキュー名が含まれる場合は起動時エラーにする（typo で静かに何も
	// 引かなくなる事故を防ぐ）。
	Queues []string
}

// buildRiverConfig は ClientConfig から river.Config を組み立てる。
//
// river.NewClient を呼ぶ NewClient から分離してあるのは、DB 接続なしで
// queues 検証や PeriodicJobs の有無を単体テストできるようにするため。
func buildRiverConfig(workers *river.Workers, cfg ClientConfig) (*river.Config, error) {
	ingestConcurrency := cfg.IngestConcurrency
	if ingestConcurrency <= 0 {
		ingestConcurrency = defaultIngestConcurrency
	}
	encodeConcurrency := cfg.EncodeConcurrency
	if encodeConcurrency <= 0 {
		encodeConcurrency = defaultEncodeConcurrency
	}
	thumbnailConcurrency := cfg.ThumbnailConcurrency
	if thumbnailConcurrency <= 0 {
		thumbnailConcurrency = defaultThumbnailConcurrency
	}

	all := allQueues(ingestConcurrency, encodeConcurrency, thumbnailConcurrency)

	queues := all
	if len(cfg.Queues) > 0 {
		queues = make(map[string]river.QueueConfig, len(cfg.Queues))
		for _, name := range cfg.Queues {
			qc, ok := all[name]
			if !ok {
				return nil, fmt.Errorf("worker.queues: unknown queue %q (valid: %s)",
					name, strings.Join(sortedQueueNames(all), ", "))
			}
			queues[name] = qc
		}
	}

	riverCfg := &river.Config{
		Queues:  queues,
		Workers: workers,
	}

	if cfg.PeriodicJobs {
		if cfg.EpgSyncSite != "" {
			interval := cfg.EpgSyncInterval
			if interval <= 0 {
				interval = defaultEpgSyncInterval
			}
			site := cfg.EpgSyncSite
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return EpgSyncArgs{Site: site}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			))
		}
		if cfg.TunerSyncSite != "" {
			interval := cfg.TunerSyncInterval
			if interval <= 0 {
				interval = defaultTunerSyncInterval
			}
			site := cfg.TunerSyncSite
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return TunerSyncArgs{Site: site}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			))
		}
		if cfg.RulerPassSite != "" {
			interval := cfg.RulerPassInterval
			if interval <= 0 {
				interval = defaultRulerPassInterval
			}
			site := cfg.RulerPassSite
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return RulerPassArgs{Site: site}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			))
		}
		if cfg.ReconcilePassSite != "" {
			interval := cfg.ReconcilePassInterval
			if interval <= 0 {
				interval = defaultReconcilePassInterval
			}
			site := cfg.ReconcilePassSite
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return ReconcilePassArgs{Site: site}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			))
		}
		if cfg.RecordSweepSite != "" {
			interval := cfg.RecordSweepInterval
			if interval <= 0 {
				interval = defaultRecordSweepInterval
			}
			site := cfg.RecordSweepSite
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return RecordSweepArgs{Site: site}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			))
		}
		if cfg.CatalogExport {
			interval := cfg.CatalogExportInterval
			if interval <= 0 {
				interval = defaultCatalogExportInterval
			}
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return CatalogExportArgs{}, nil
				},
				// 起動直後にも 1 回書いておく。日次なので RunOnStart で初回を確保する。
				&river.PeriodicJobOpts{RunOnStart: true},
			))
		}
		if cfg.DeleteReconcile {
			interval := cfg.DeleteReconcileInterval
			if interval <= 0 {
				interval = defaultDeleteReconcileInterval
			}
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return DeleteReconcileArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			))
		}
	}

	return riverCfg, nil
}

// AllQueueNames はこのプロセスが知っている全キュー名をソートして返す。
// worker.queues の設定ミスを報告するためのほか、ロール名との衝突検査
// （cmd/rokuban の TestAllRoles_ExcludesJobNames）でも使う --- キューを追加したときに
// 「キュー駆動の仕事をロールに昇格させていないか」の検査対象へ自動的に入るようにするため。
func AllQueueNames() []string {
	return sortedQueueNames(allQueues(0, 0, 0))
}

// RequiresEncodeTools は、worker.queues の設定でこのプロセスが encode または
// thumbnail キューを購読するかを返す。空スライスは「全キュー購読」を意味する
// ので true になる。
//
// worker ロール起動時の ffmpeg/ffprobe 存在検査（不変条件 4）をこの購読の
// 有無に揃えるための判定（issue #113 決定 C）。worker.queues で encode /
// thumbnail を明示的に外した worker Pod（例: ingest 専用 Pod）まで ffmpeg の
// 存在を要求しないようにする一方、既定（全キュー購読）や明示的に含めた場合は
// 引き続き起動時に検査して、ffmpeg 不在環境でジョブの再試行を焼く前に落ちる。
func RequiresEncodeTools(queues []string) bool {
	if len(queues) == 0 {
		return true
	}
	return slices.Contains(queues, encodeQueue) || slices.Contains(queues, thumbnailQueue)
}

func sortedQueueNames(m map[string]river.QueueConfig) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NewClient は River クライアントを生成する。
func NewClient(pool *pgxpool.Pool, workers *river.Workers, cfg ClientConfig) (*river.Client[pgx5.Tx], error) {
	riverCfg, err := buildRiverConfig(workers, cfg)
	if err != nil {
		return nil, err
	}

	client, err := river.NewClient(riverpgxv5.New(pool), riverCfg)
	if err != nil {
		return nil, fmt.Errorf("creating river client: %w", err)
	}
	return client, nil
}

// NewInsertOnlyClient は投入専用の River クライアントを返す。
//
// Queues を省略し Workers も登録しないため、Start は呼べない（呼ばないことが
// 前提。river@v0.40.0 の client.go:90 のコメントの通り、Queues を省略すれば
// insert-only なクライアントになる）。
//
// api ロールと `rokuban enqueue` サブコマンドはジョブを実行しない
// （不変条件: api は mirakc に問い合わせず、ffmpeg も実行しない）。
// NewWorkers が返すフルのワーカー群を登録すると ingest/encode 等まで
// そのプロセスに紐付いてしまうため、ヒント投入・CronJob 投入に必要な
// InsertTx / Insert だけができる最小構成にする。
//
// watcher ロール単独のプロセスも同じ理由でこれを使う（issue #113）。watcher が
// River を使うのは ingest ジョブの投入（InsertTx）だけで、worker ロールが無い
// 限り EncodeWorker/ThumbnailWorker のような ffmpeg/ffprobe に依存するワーカーを
// 登録・実行してはならない（不変条件 4）。
func NewInsertOnlyClient(pool *pgxpool.Pool) (*river.Client[pgx5.Tx], error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("creating insert-only river client: %w", err)
	}
	return client, nil
}
