package riveradapter

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/tsscan"
)

const (
	defaultIngestConcurrency          = 3
	defaultEpgSyncInterval            = 10 * time.Minute
	defaultTunerSyncInterval          = 10 * time.Minute
	defaultTSScanReconcileInterval    = 15 * time.Minute
	defaultRulerPassInterval          = 10 * time.Minute
	defaultReconcilePassInterval      = 30 * time.Second
	defaultRecordSweepInterval        = 5 * time.Minute
	defaultCatalogExportInterval      = 24 * time.Hour
	defaultDeleteReconcileInterval    = 15 * time.Minute
	defaultLabelRuleReconcileInterval = 15 * time.Minute
	defaultEncodeReconcileInterval    = 15 * time.Minute
	defaultThumbnailReconcileInterval = 15 * time.Minute
	defaultCMDetectReconcileInterval  = 15 * time.Minute
	defaultStorageSyncInterval        = 5 * time.Minute
)

// DefaultSoftStopTimeout は Config.SoftStopTimeout の既定値。
const DefaultSoftStopTimeout = 5 * time.Second

const (
	defaultEncodeConcurrency    = 1
	defaultThumbnailConcurrency = 1
	// defaultCleanupConcurrency は cleanup キューの MaxWorkers。delete_reconcile と
	// catalog_export はそれぞれ UniqueOpts{ByArgs} で自分自身の重複実行を防ぐので、
	// 「両方が同時に 1 本ずつ走れる」ぶんだけ確保すれば足り、それ以上に増やす理由がない。
	defaultCleanupConcurrency = 2
)

// allQueues はこのプロセスが知っている全キューの**論理**（unqualified）名とその
// 設定。worker.queues（Config.Queues）で絞り込む際の許可リストになり、
// jobs.AllQueueNames / jobs.RequiresEncodeTools / jobs.RequiresSiteBinding もこの論理名の
// 集合に対して判定する --- worker.queues の設定値・エラーメッセージのいずれも
// 論理名のままにするため（issue #185 の「罠」「エラーメッセージも論理名で出す」）。
//
// site 単位のキュー（internal/jobs の siteBoundQueueNames）を実際に Insert/購読
// するときの物理名（`<base>_<site>`）への展開は jobs.PhysicalQueueName が担う。BuildConfig が
// river.Config.Queues を組み立てる直前にこの展開を行う。
func allQueues(ingestConcurrency, encodeConcurrency, thumbnailConcurrency int) map[string]river.QueueConfig {
	return map[string]river.QueueConfig{
		jobs.DefaultQueue: {MaxWorkers: 100},
		jobs.IngestQueue:  {MaxWorkers: ingestConcurrency},
		// 全量同期が重ならないよう 1 本に絞る。tuner_sync も同じキューを使う
		// （どちらも使い捨てプロジェクションの全量同期。TunerSyncArgs.InsertOpts 参照）。
		jobs.EpgQueue: {MaxWorkers: 1},
		// サイト単位の排他は UniqueOpts が担うので、こちらも複数ワーカーは要らない。
		jobs.RulerQueue: {MaxWorkers: 1},
		// reconciler も ruler と同じ理由（UniqueOpts がサイト単位の排他を担う）で 1 本に絞る。
		jobs.ReconcilerQueue: {MaxWorkers: 1},
		// record_sweep も同じ理由（UniqueOpts がサイト単位の排他を担う）で 1 本に絞る。
		jobs.RecordSweepQueue: {MaxWorkers: 1},
		// encode / thumbnail は CPU 拘束なので ingest とは独立に絞る（EPGStation#531、
		// issue #64）。thumbnail ワーカーは M3-4、encode ワーカーは M3-3。
		jobs.EncodeQueue:    {MaxWorkers: encodeConcurrency},
		jobs.ThumbnailQueue: {MaxWorkers: thumbnailConcurrency},
		jobs.CMDetectQueue:  {MaxWorkers: 1},
		// delete_reconcile / catalog_export / label_rule_reconcile 用（issue #185 M4-13。internal/jobs/queue.go の
		// CleanupQueue のコメント参照）。
		jobs.CleanupQueue: {MaxWorkers: defaultCleanupConcurrency},
		// storage_sync 用（issue #238 M7-5）。UniqueOpts{ByArgs} が重複実行を防ぐので
		// tuner_sync/ruler/reconciler/record_sweep と同じく 1 本で足りる。
		jobs.StorageQueue: {MaxWorkers: 1},
		// ts_scan は原本全体を読み直すため、同時 I/O を 1 件に抑える。
		jobs.TSScanQueue: {MaxWorkers: 1},
	}
}

// Config は River クライアントの設定。
type Config struct {
	// JobTimeout は River の既定ジョブタイムアウト。Spec.Timeout がある worker はその値を使う。
	JobTimeout time.Duration

	// TestOnly は River のテスト用設定を有効にする。
	TestOnly bool

	// BoundSites はこのプロセスが束縛されている mirakc サイト名の集合
	// （cmd/rokuban が --sites から解決する。空スライスは 0 サイト束縛
	// （中央プロセス）を意味する。issue #183 M4-11 / #185 M4-13 / #532 で
	// 単一の BoundSite string から N site の集合に一般化した）。
	//
	// 2 つの役目を持つ（issue #532 の「含むもの」2・3。同じ値を 2 箇所で使うのは
	// 「この site 集合をどこまで面倒みるか」が両方とも束縛そのものだから ---
	// server.go は常にここへ同じ値を渡していた）:
	//
	//  1. internal/jobs の siteBoundQueueNames に含まれる論理キュー（ingest/epg/reconciler/
	//     watcher）を実際に Insert/購読する際の物理名（`<base>_<site>`）を、
	//     この集合の**要素ごとに**組み立てる（jobs.PhysicalQueueName、
	//     BuildConfig。insert 側は 1 ジョブの args.Site から 1 つに決まる
	//     ので N 化するのは subscribe 側だけ）
	//  2. site 単位の定期ジョブ（epg_sync/tuner_sync/ruler_pass/
	//     reconcile_pass/record_sweep。旧 EpgSyncSite 等の 5 フィールド）を、
	//     この集合の要素ごとに 1 本ずつ登録する
	//
	// 空スライスは jobs.PhysicalQueueName が db.DefaultSite に解決する 1 要素
	// （[]string{""}）として扱う --- 0 サイト束縛の worker が site-bound キューを
	// 要求しないことは cmd/rokuban.validateSiteBinding が起動時に強制するので、
	// ここで実際にこの解決が使われるのはテストの部分構成（BoundSites 未設定）
	// だけである。
	BoundSites []string

	// IngestConcurrency は ingest キューの同時実行数（サイト単位のキャップ）。0 なら既定値。
	IngestConcurrency int

	// EncodeConcurrency は encode キューの MaxWorkers。0 なら既定値 1。
	// encode.concurrency から注入する（issue #64）。
	EncodeConcurrency int

	// ThumbnailConcurrency は thumbnail キューの MaxWorkers。0 なら既定値 1。
	// encode.thumbnail_concurrency から注入する（issue #64）。
	ThumbnailConcurrency int

	// EpgSyncInterval は EPG 全量同期の間隔。0 なら既定値。BoundSites の要素ごとに
	// 1 本ずつ登録する（PeriodicJobs が true のときのみ。BoundSites のコメント参照）。
	EpgSyncInterval time.Duration

	// TunerSyncInterval はチューナー射影の間隔。0 なら既定値（10 分）。BoundSites の
	// 要素ごとに 1 本ずつ登録する（PeriodicJobs が true のときのみ）。
	TunerSyncInterval time.Duration

	// RulerPassInterval は ruler 定期パスの間隔。0 なら既定値。BoundSites の要素
	// ごとに 1 本ずつ登録する（PeriodicJobs が true のときのみ。ruler は mirakc に
	// 触れないが、args.Site が DB 行の絞り込みなので site ごとに 1 本で正しい
	// --- internal/jobs/queue.go の siteBoundQueueNames のコメント参照）。
	RulerPassInterval time.Duration

	// ReconcilePassInterval は reconciler 定期パスの間隔。0 なら既定値（30 秒）。
	// BoundSites の要素ごとに 1 本ずつ登録する（PeriodicJobs が true のときのみ）。
	ReconcilePassInterval time.Duration

	// RecordSweepInterval は record_sweep 定期パス（watcher 全量突き合わせ (c)、
	// docs/recording.md §3.3）の間隔。0 なら既定値（5 分）。BoundSites の要素
	// ごとに 1 本ずつ登録する（PeriodicJobs が true のときのみ）。
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

	// LabelRuleReconcile が true なら分類ルールの全件再評価を定期ジョブとして
	// 登録する（PeriodicJobs が true のときのみ）。サイト非依存の全件評価なので、
	// BoundSites の要素数には影響されない。
	LabelRuleReconcile bool

	// LabelRuleReconcileInterval は分類ルール再評価の間隔。0 なら既定値（15 分）。
	LabelRuleReconcileInterval time.Duration

	// EncodeReconcile が true なら encode の desired−observed 定期パス
	// （issue #163）を定期ジョブとして登録する（PeriodicJobs が true のときのみ）。
	// CatalogExport / DeleteReconcile と同じくサイト非依存
	// （エンコードは site の属性を持たない。EncodeReconcileArgs のコメント参照）。
	EncodeReconcile bool

	// EncodeReconcileInterval は encode reconcile の間隔。0 なら既定値（15 分）。
	EncodeReconcileInterval time.Duration

	// ThumbnailReconcile が true なら thumbnail の desired−observed 定期パスを
	// 定期ジョブとして登録する（PeriodicJobs が true のときのみ）。EncodeReconcile
	// と同じくサイト非依存で、thumbnail キューを実ジョブと共有する。
	ThumbnailReconcile bool

	// ThumbnailReconcileInterval は thumbnail reconcile の間隔。0 なら既定値（15 分）。
	ThumbnailReconcileInterval time.Duration

	// TSScanReconcile は active original の TS scan を定期的に再投入する。
	TSScanReconcile bool

	// CMDetectReconcile registers the CM detection desired-state periodic pass.
	CMDetectReconcile bool

	// StorageSync が true ならストレージ観測（issue #238 M7-5）を定期ジョブとして
	// 登録する（PeriodicJobs が true のときのみ）。CatalogExport / DeleteReconcile と
	// 同じくサイト非依存（観測対象は単一の MediaDir / ScratchDir）。
	StorageSync bool

	// StorageSyncInterval はストレージ観測の間隔。0 なら既定値（5 分）。
	StorageSyncInterval time.Duration

	// PeriodicJobs が false なら、BoundSites / CatalogExport / DeleteReconcile /
	// EncodeReconcile / ThumbnailReconcile / TSScanReconcile / CMDetectReconcile / StorageSync が設定されていても River の PeriodicJobs を
	// 一切登録しない。
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

	// SoftStopTimeout は停止の合図を受けてから実行中のジョブを打ち切るまでの猶予。
	// 0 なら既定値（DefaultSoftStopTimeout）。
	//
	// **これを設定しないと SIGTERM が drain にならない。** River は
	// SoftStopTimeout が未設定（0）のとき work ctx を start ctx から継ぐので、
	// `signal.NotifyContext` の ctx を `Start` に渡している構成では **SIGTERM が
	// そのまま StopAndCancel 相当のハードストップになる**（river@v0.47.0 client.go の
	// workParentCtx）。実行中のジョブは即座に ctx を切られ、試行回数を 1 つ
	// 潰して `available` に戻る。0 を既定値に読み替えるのはこのためで、
	// 「設定し忘れ」が最も危険な側に倒れないようにする。
	//
	// 効くのは停止の合図の種類を問わない（Start の ctx の cancel / Stop /
	// StopAndCancel のいずれでも同じ）。したがって 1 件消化モードの正常終了
	// （cmd/rokuban の stopOnceProcess が撃つ graceful stop）にも上限が付く ---
	// 1 件消化した後に取りこぼしのジョブを掴んでいた場合、その完走を待つのは
	// この猶予までである。
	SoftStopTimeout time.Duration

	// RescueStuckJobsAfter は river.Config.RescueStuckJobsAfter にそのまま渡す。
	// 0 なら River の既定（1h）。設定キーは worker.rescue_stuck_jobs_after。
	RescueStuckJobsAfter time.Duration

	// Once が非 nil なら 1 件消化モード（`rokuban server --once`）になる。
	// KEDA ScaledJob が起こした k8s Job が自分で終了できるようにするための
	// 起動形態で、ジョブ 1 件の Work を抜けたら（成功・失敗を問わず）
	// プロセスを畳む（OnceGate、docs/operations.md §5）。
	//
	// このモードでは BuildConfig が次の 3 つを強制する:
	//
	//   - 引くキューはちょうど 1 つ（ScaledJob はキュー単位に作るため）
	//   - そのキューの MaxWorkers は 1（同時 claim を 1 件に抑える。
	//     KEDA 側は「1 Job = 1 アイテム」で数を合わせている）
	//   - PeriodicJobs は false（定期投入を Job の起動回数に委ねない）
	//
	// **ジョブの失敗をプロセスの終了コードに出さない。** リトライは River が
	// 持っており、k8s 側は `backoffLimit: 0` / `restartPolicy: Never` で
	// 起こし直さない前提なので、両方が再試行して二重にリトライする形を作らない
	// （終了コードの決定は cmd/rokuban 側。OnceOutcome のコメント参照）。
	Once *OnceGate
}

// BuildConfig は Config から river.Config を組み立てる。
//
// river.NewClient を呼ぶ NewClient から分離してあるのは、DB 接続なしで
// queues 検証や PeriodicJobs の有無を単体テストできるようにするため。
func BuildConfig(workers *river.Workers, cfg Config) (*river.Config, error) {
	physicalQueues, err := resolveQueues(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateOncePhysicalQueues(cfg, physicalQueues); err != nil {
		return nil, err
	}

	// 実際に SKIP LOCKED で引く物理キュー名の集合をログに出す（issue #185 の
	// 「罠」: 全キュー購読（既定）は「全部購読しているつもりで実は site 束縛
	// キューを引いていない」を起動時に伝える手段が要る。KEDA のスケーラ定義を
	// 合わせる運用者と、「ジョブが available のまま動かない」を調べる人が
	// 最初に見る情報になる）。
	slog.Info("worker: subscribing to queues",
		"queues", sortedQueueNames(physicalQueues), "bound_sites", cfg.BoundSites)

	softStopTimeout := cfg.SoftStopTimeout
	if softStopTimeout <= 0 {
		softStopTimeout = DefaultSoftStopTimeout
	}
	riverCfg := &river.Config{
		Queues:          physicalQueues,
		Workers:         workers,
		SoftStopTimeout: softStopTimeout,
		JobTimeout:      cfg.JobTimeout,
		TestOnly:        cfg.TestOnly,

		RescueStuckJobsAfter: cfg.RescueStuckJobsAfter,
	}
	configureOnceRiver(riverCfg, cfg.Once, physicalQueues)
	configurePeriodicJobs(riverCfg, cfg)
	return riverCfg, nil
}

// resolveQueues はこの設定が実際に引く**物理**キュー（`<base>_<site>`）を、
// 並列度の解決と once モードの上書きまで済ませた形で返す
// （選択 → once の上書き → site 別物理キューへの展開）。
func resolveQueues(cfg Config) (map[string]river.QueueConfig, error) {
	queues, err := selectRiverQueues(cfg)
	if err != nil {
		return nil, err
	}
	if err := configureOnceQueues(cfg, queues); err != nil {
		return nil, err
	}
	return qualifyRiverQueues(queues, cfg.BoundSites), nil
}

// selectRiverQueues は設定された並列度を解決し、論理キューの集合を返す。
func selectRiverQueues(cfg Config) (map[string]river.QueueConfig, error) {
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
	// all / queues は論理（unqualified）名で組み立てる --- worker.queues の設定値・
	// 未知キューのエラーメッセージのいずれも論理名のままにするため
	// （issue #185 の「罠」「エラーメッセージも論理名で出す」。allQueues のコメント参照）。
	all := allQueues(ingestConcurrency, encodeConcurrency, thumbnailConcurrency)
	if len(cfg.Queues) == 0 {
		return all, nil
	}
	queues := make(map[string]river.QueueConfig, len(cfg.Queues))
	for _, name := range cfg.Queues {
		qc, ok := all[name]
		if !ok {
			return nil, fmt.Errorf("worker.queues: unknown queue %q (valid: %s)",
				name, strings.Join(sortedQueueNames(all), ", "))
		}
		queues[name] = qc
	}
	return queues, nil
}

// configureOnceQueues は once モードの論理キュー検証と同時実行数の制限を行う。
func configureOnceQueues(cfg Config, queues map[string]river.QueueConfig) error {
	if cfg.Once == nil {
		return nil
	}
	// **1 キューだけ**を要求する。ScaledJob はキュー単位に作る（寿命 ×
	// site 束縛で切る。docs/operations.md §5）ので、1 件消化モードの Job が
	// 複数キューを引く形は KEDA 側に対応する定義が無い。加えて、複数キューを
	// 許すと MaxWorkers=1 でもキューごとに 1 件ずつ掴めるため「同時 claim は
	// 1 件」が成立しない（River の MaxWorkers はキュー単位）。
	if len(queues) != 1 {
		return fmt.Errorf("once mode requires exactly one queue, got %d (%s): "+
			"a KEDA ScaledJob is created per queue, so pass exactly one queue "+
			"(--queues <name> or worker.queues); an empty setting means all queues",
			len(queues), strings.Join(sortedQueueNames(queues), ", "))
	}
	// 定期投入を「Job が何回起きたか」に委ねない。River の PeriodicJobs は
	// リーダーに選出されたクライアントだけが投入するので、1 件で終わる Job が
	// リーダーになると投入の間隔が KEDA のスケール挙動で決まってしまう
	// （RunOnStart のぶんが起動ごとに入る）。docs/data.md §2 が定期実行の
	// 契機をデプロイ形態に委ねたのは CronJob に委ねるためであって、
	// Job の起動回数に委ねるためではない。
	if cfg.PeriodicJobs {
		return fmt.Errorf("once mode requires periodic jobs to be disabled " +
			"(set worker.periodic_jobs: false and insert periodic jobs with the " +
			"`rokuban enqueue` CronJobs instead)")
	}
	// 同時 claim を 1 件に抑える（config の *Concurrency より優先する）。
	for name, qc := range queues {
		qc.MaxWorkers = 1
		queues[name] = qc
	}
	return nil
}

// qualifyRiverQueues は論理キューを BoundSites ごとの物理キューへ展開する。
//
// river.Config.Queues は実際に SKIP LOCKED で引く物理キュー名でなければならない。
// ここで初めて論理名から物理名（`<base>_<site>`）に展開する（jobs.PhysicalQueueName）。
// site 単位のキューは BoundSites の要素ごとに 1 つずつ展開する ---
// **subscribe 側だけを N 回呼ぶ**（issue #532 の「罠」。insert 側
// （各 JobArgs.InsertOpts）は 1 ジョブの args.Site から物理名が 1 つに決まるので
// N 化しない）。
func qualifyRiverQueues(queues map[string]river.QueueConfig, boundSites []string) map[string]river.QueueConfig {
	if len(boundSites) == 0 {
		// 0 サイト束縛（中央プロセス）の後方互換フォールバック（BoundSites の
		// コメント、jobs.PhysicalQueueName と同じ規約）。本番は validateSiteBinding が
		// この状態で site-bound キューを要求すること自体を防ぐので、ここに来るのは
		// テストの部分構成（BoundSites 未設定）だけである。
		boundSites = []string{""}
	}
	// jobs.PhysicalQueueName(name, site) は site-bound でない name を site 引数に
	// 関わらずそのまま返すので、site-bound かどうかの判定をここで重複させる
	// 必要はない（jobs.PhysicalQueueName のコメントが警告する「判定が 2 か所に
	// 分かれてズレる」経路を作らないため。site-bound でないキューは
	// boundSites の要素数ぶん同じキーへ書くだけの無害な重複になる）。
	physicalQueues := make(map[string]river.QueueConfig, len(queues))
	for name, qc := range queues {
		for _, site := range boundSites {
			physicalQueues[jobs.PhysicalQueueName(name, site)] = qc
		}
	}
	return physicalQueues
}

// validateOncePhysicalQueues は site 展開後も once モードが 1 キューになることを検証する。
//
// once モードは KEDA ScaledJob がキュー単位（かつ site 単位。
// docs/operations.md §5「キュー × 置き場所 × site 軸」）に作る前提なので、
// 展開後もちょうど 1 つの物理キューでなければならない。configureOnceQueues の
// 「論理キューはちょうど 1 つ」の検査だけでは、その 1 つが site-bound キューで
// BoundSites が 2 つ以上のとき、ここで初めて 2 物理キューに割れてしまう
// （issue #532 で site を N 化した副作用。この検査が無いと once モードの
// 「同時 claim は 1 件」の前提が黙って崩れる）。
func validateOncePhysicalQueues(cfg Config, physicalQueues map[string]river.QueueConfig) error {
	if cfg.Once == nil || len(physicalQueues) == 1 {
		return nil
	}
	return fmt.Errorf(
		"once mode requires exactly one physical queue after site qualification, got %d (%s): "+
			"a KEDA ScaledJob binds to exactly one site, so pass exactly one bound site "+
			"when the once queue is site-bound (ingest/epg/reconciler/watcher)",
		len(physicalQueues), strings.Join(sortedQueueNames(physicalQueues), ", "))
}

// configureOnceRiver は once モードの middleware と起動ログを設定する。
func configureOnceRiver(riverCfg *river.Config, once *OnceGate, physicalQueues map[string]river.QueueConfig) {
	if once == nil {
		return
	}
	// ジョブの開始と終了を観測する唯一の手段として WorkerMiddleware を使う
	// （River には「ジョブが claim された」イベントが無く、Subscribe で拾える
	// のは完了後のイベントだけなので、「まだ 1 件も掴んでいない」と
	// 「掴んで実行中」を区別できない）。
	riverCfg.Middleware = []rivertype.Middleware{once.Middleware()}
	// `worker: subscribing to queues` とは別の行にする。あちらは KEDA の
	// スケーラ定義と購読集合を突き合わせる運用者が見る行なので、形を変えない
	// （issue #421 の「罠」）。
	// キューはちょうど 1 つ（configureOnceQueues / validateOncePhysicalQueues の
	// 検査）なので単数で出す。
	slog.Info("worker: once mode enabled (exits after one job)",
		"queue", sortedQueueNames(physicalQueues)[0])
}

// configurePeriodicJobs は設定された定期ジョブを River 設定へ登録する。
func configurePeriodicJobs(riverCfg *river.Config, cfg Config) {
	if !cfg.PeriodicJobs {
		return
	}
	configureSitePeriodicJobs(riverCfg, cfg)
	configureGlobalPeriodicJobs(riverCfg, cfg)
}

// configureSitePeriodicJobs は BoundSites ごとのサイト単位の定期ジョブを登録する。
//
// site 単位の 5 種（epg_sync/tuner_sync/ruler_pass/reconcile_pass/
// record_sweep）は cfg.BoundSites の要素ごとに 1 本ずつ登録する
// （issue #532 の「含むもの」3。旧 EpgSyncSite 等 5 つの独立フィールドを
// BoundSites 1 つに畳んだ --- server.go は常に同じ site 集合をこの 5 つに
// 渡していたので、独立フィールドは冗長でしかなかった）。0 サイト束縛
// （BoundSites が空）なら何も登録しない --- 中央プロセスが site 単位の
// 定期パスを持つ理由がない。
func configureSitePeriodicJobs(riverCfg *river.Config, cfg Config) {
	if len(cfg.BoundSites) == 0 {
		return
	}
	epgSyncInterval := cfg.EpgSyncInterval
	if epgSyncInterval <= 0 {
		epgSyncInterval = defaultEpgSyncInterval
	}
	tunerSyncInterval := cfg.TunerSyncInterval
	if tunerSyncInterval <= 0 {
		tunerSyncInterval = defaultTunerSyncInterval
	}
	rulerPassInterval := cfg.RulerPassInterval
	if rulerPassInterval <= 0 {
		rulerPassInterval = defaultRulerPassInterval
	}
	reconcilePassInterval := cfg.ReconcilePassInterval
	if reconcilePassInterval <= 0 {
		reconcilePassInterval = defaultReconcilePassInterval
	}
	recordSweepInterval := cfg.RecordSweepInterval
	if recordSweepInterval <= 0 {
		recordSweepInterval = defaultRecordSweepInterval
	}
	for _, site := range cfg.BoundSites {
		site := site
		riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs,
			river.NewPeriodicJob(river.PeriodicInterval(epgSyncInterval), func() (river.JobArgs, *river.InsertOpts) {
				return jobs.EpgSyncArgs{Site: site}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(tunerSyncInterval), func() (river.JobArgs, *river.InsertOpts) {
				return jobs.TunerSyncArgs{Site: site}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(rulerPassInterval), func() (river.JobArgs, *river.InsertOpts) {
				return jobs.RulerPassArgs{Site: site}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(reconcilePassInterval), func() (river.JobArgs, *river.InsertOpts) {
				return jobs.ReconcilePassArgs{Site: site}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(recordSweepInterval), func() (river.JobArgs, *river.InsertOpts) {
				return jobs.RecordSweepArgs{Site: site}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
		)
	}
}

// configureGlobalPeriodicJobs はサイトに依存しない定期ジョブを登録する。
func configureGlobalPeriodicJobs(riverCfg *river.Config, cfg Config) {
	appendPeriodic := func(enabled bool, interval, defaultInterval time.Duration, args river.JobArgs) {
		if !enabled {
			return
		}
		if interval <= 0 {
			interval = defaultInterval
		}
		// 起動直後にも 1 回書いておく。日次なので RunOnStart で初回を確保する
		// （元は CatalogExport 単独の判断。EncodeReconcile / DeleteReconcile /
		// StorageSync も同じ RunOnStart: true を共有しているが、それぞれの間隔での
		// 「起動直後の 1 回」が同程度に有用かは個別に検証していない）。
		riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
			river.PeriodicInterval(interval),
			func() (river.JobArgs, *river.InsertOpts) { return args, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}
	appendPeriodic(cfg.CatalogExport, cfg.CatalogExportInterval, defaultCatalogExportInterval, jobs.CatalogExportArgs{})
	appendPeriodic(cfg.DeleteReconcile, cfg.DeleteReconcileInterval, defaultDeleteReconcileInterval, jobs.DeleteReconcileArgs{})
	appendPeriodic(cfg.LabelRuleReconcile, cfg.LabelRuleReconcileInterval, defaultLabelRuleReconcileInterval, jobs.LabelRuleReconcileArgs{})
	appendPeriodic(cfg.EncodeReconcile, cfg.EncodeReconcileInterval, defaultEncodeReconcileInterval, jobs.EncodeReconcileArgs{})
	appendPeriodic(cfg.ThumbnailReconcile, cfg.ThumbnailReconcileInterval, defaultThumbnailReconcileInterval, jobs.ThumbnailReconcileArgs{})
	appendPeriodic(cfg.TSScanReconcile, 0, defaultTSScanReconcileInterval, tsscan.ReconcileArgs{})
	appendPeriodic(cfg.CMDetectReconcile, 0, defaultCMDetectReconcileInterval, jobs.CMDetectReconcileArgs{})
	appendPeriodic(cfg.StorageSync, cfg.StorageSyncInterval, defaultStorageSyncInterval, jobs.StorageSyncArgs{})
}

func sortedQueueNames(m map[string]river.QueueConfig) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NewLegacyClient creates a River client from the existing River worker registry.
// New application code should use NewAdapter with a jobq.Registry.
func NewLegacyClient(pool *pgxpool.Pool, workers *river.Workers, cfg Config) (*river.Client[pgx5.Tx], error) {
	riverCfg, err := BuildConfig(workers, cfg)
	if err != nil {
		return nil, err
	}
	client, err := river.NewClient(riverpgxv5.New(pool), riverCfg)
	if err != nil {
		return nil, fmt.Errorf("creating River client: %w", err)
	}
	return client, nil
}

// NewInsertOnlyClient creates a River client that can enqueue and inspect jobs but cannot run them.
func NewInsertOnlyClient(pool *pgxpool.Pool) (*river.Client[pgx5.Tx], error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("creating insert-only River client: %w", err)
	}
	return client, nil
}
