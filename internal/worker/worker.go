package worker

import (
	"fmt"
	"log/slog"
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

// site 単位のキュー（ingest/epg/reconciler/watcher）と cleanup（delete_reconcile /
// catalog_export）の各 InsertOpts は UniqueOpts.ByQueue: true を立てる
// （issue #185 M4-13 のレビューで判明。river@v0.40.0/insert_opts.go:171-175 の
// ByQueue の doc コメント参照）。
//
// # なぜ必要か
//
// River の UniqueOpts は既定で ByQueue: false であり、そのとき一意キーは
// kind + args（ByArgs 有効時）**だけ**で組み立てられ、Queue を含まない
// （internal/dbunique/db_unique.go の鍵組み立て）。このリポジトリの site 単位
// ジョブは元々すべて UniqueOpts{ByArgs: true, ByState: pendingJobStates} だった
// ため、ByQueue を立てない限り、**キュー名を変えても一意キーは変わらない**。
//
// この PR がキュー名を `ingest` → `ingest_<site>` 等に修飾した結果、デプロイ後に
// 旧キューへ挿入された行（同じ kind + args）が unique_key を占有し続け、新しい
// 修飾済みキューへの Insert が `UniqueSkippedAsDuplicate` として**エラーを返さず
// 黙って**合流してしまう。個々のジョブが retryable のまま残ると、その args
// （site + record_id 等）は二度と新キューへ入れず、レベルトリガーの再投入
// （record_sweep 等）も同じ args に合流するだけで前進しない。
//
// ByQueue: true にすると一意キーに Queue が加わるため、キュー名が変われば
// 別のジョブとして扱われる。今回のような**将来のキュー名変更（リネーム・
// 修飾規則の変更）に対する一般的な保険**として、site 単位のキュー全部と
// cleanup（delete_reconcile / catalog_export、river.QueueDefault → cleanup の
// 移設が同じ問題を踏む）に適用する。
//
// **これは今回の移行そのものも救う。** 旧キューの行の unique_key は
// 変わらない（kind + args から作られたまま）が、ByQueue: true の下で作られる
// 新しい鍵は kind + args + queue から作られるので**別ハッシュになり衝突しない**。
// レビューで 6 ジョブ種すべてについて実測した --- 旧形式の残骸がある状態でも
// 新キューへの Insert は skipped=false で通る（対照として、旧形式の再 Insert は
// skipped=true で dedup されるので、残骸が旧鍵を占有していること自体は確認済み）。
//
// したがって旧キューの残骸の掃除は**滞留メトリクスの衛生のためであって、
// 再投入のブロック解除のためではない**。手順は docs/runbook/troubleshooting.md
// 「デプロイ直後、旧キューの残骸が `river_job` に残っている」を参照。
//
// ruler / encode / thumbnail は今回のキュー名変更の対象外（ruler は
// site 非依存のまま "ruler" 固定、encode/thumbnail も変更していない）ので、
// ByQueue を追加する必要はない --- 追加しても害はないが、対象外のキューまで
// 変更すると「なぜここは変えたのか」の説明が必要になる範囲が広がるだけで
// 何の保険にもならない。
const uniqueByQueue = true

// verifySite は、mirakc かファイルシステムに触るワーカーが処理しようとしている
// ジョブの site（jobSite）が、そのワーカープロセス自身の site（workerSite、
// config.mirakc.site）と一致するかを検査する fail-fast ガード（issue #139）。
//
// mirakc の record id / programId はインスタンススコープであり、DB の site 列は
// そのスコープの境界を表現するために存在する（issue #31、docs/schema.md
// §1-5）。**一次防御はキュー選択そのもの**: site 単位のキュー（ingest/epg/
// reconciler/watcher）は物理名を site で修飾する（qualifyQueueName、issue #185
// M4-13）ため、site A の worker は site B 向けのジョブを購読すら
// しない（cmd/rokuban.validateSiteBinding が worker.queues とサイト束縛の
// 組み合わせも検査する）。verifySite はその後ろに立つ**二次防御**で、キュー選択が
// 正しくてもジョブの Args.Site 自体が壊れている経路（手で INSERT した / 将来の
// ヒント投入のバグ / River の一意性判定が旧キューの残骸と合流して args だけが
// 別サイトのまま挿入された、等）を捕まえる。掴んだまま mirakc/FS に触れると、
// A の mirakc に B の id を投げて A の別番組を B の recording としてコミットし
// うる（不変条件 3「コミット = DB 行」への違反。詳細は issue #139 本文）。
//
// workerSite が空文字列（Deps.Site 未注入。単体テストの部分構成を含む）の場合は
// db.DefaultSite に解決してから比較する。単一サイト構成でこのガードを追加しても
// テスト・運用のどちらも壊れないようにするための規約。
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
	river.AddWorker(workers, &EncodeReconcileWorker{
		Pool: deps.Pool,
		// desired の絞り込みにプロファイル名だけを使う（EncodeWorker と同じ
		// 設定を読む。EncodeReconcileArgs.InsertOpts のコメント参照）。
		Profiles: deps.Encode,
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
		TrashRetention:    deps.Cleanup.TrashRetention,
		OrphanMTimeGrace:  deps.Cleanup.OrphanMTimeGrace,
		OrphanAge:         deps.Cleanup.OrphanAge,
		MaxDeletesPerPass: deps.Cleanup.MaxDeletesPerPass,
		Webhook:           deps.Webhook,
	})
	river.AddWorker(workers, &StorageSyncWorker{
		Pool:       deps.Pool,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
	})
	return workers
}

const (
	// encodeQueue はエンコードジョブ専用のキュー名（M3-3 でワーカーを登録する）。
	encodeQueue = "encode"
	// thumbnailQueue はサムネイルジョブ専用のキュー名。
	thumbnailQueue = "thumbnail"
	// cleanupQueue は物理削除系ジョブ（delete_reconcile / catalog_export）専用の
	// キュー名（issue #185 M4-13）。両方ともアーカイブ（単一の MediaDir）にしか
	// 触れず site には依存しないため、修飾しない（siteBoundQueueNames に入れない。
	// allQueues のコメント参照）。以前は river.QueueDefault に同居していた。
	// docs/overview.md のキュー配置表が M3 の時点で既にこの名前を約束していた。
	//
	// **`worker.queues` を明示的に絞れば cleanup だけを除外できるようになった
	// という意味であり、既定（`worker.queues` 未指定 = 全キュー購読）のサイト側
	// worker が cleanup を掴まなくなったわけではない。** `worker.queues` が空の
	// 場合、allQueues の全論理名（cleanup を含む）が対象になり、site 束縛の
	// 有無に関わらずすべて購読される --- 実際、site 束縛 worker が
	// `worker.queues` を書かずに起動すると、自分の site 単位のキューに加えて
	// `cleanup` も `default` も `ruler` も購読する（実バイナリ検証で確認済み。
	// issue #185 の PR コメント参照）。「既定で cleanup を掴まないようにする」
	// には site 束縛 worker の既定購読集合から cleanup を外す変更が要るが、
	// それは単一サイト構成（唯一の worker が束縛 worker）で delete_reconcile が
	// 誰にも走らなくなるので単純にはできない。設計判断が必要なため、実装せず
	// issue #185 のコメントに提起した。
	cleanupQueue = "cleanup"

	// storageQueue はストレージ観測ジョブ（storage_sync）専用のキュー名
	// （issue #238 M7-5）。cleanupQueue に混ぜない理由は StorageSyncArgs.InsertOpts
	// のコメント参照（cleanupQueue は「物理削除系ジョブ専用」と明記されており、
	// 削除を一切しない観測ジョブを混ぜるとその名付けの前提が崩れる）。
	// site には依存しない（アーカイブもスクラッチも単一。siteBoundQueueNames には
	// 入れない）。
	storageQueue = "storage"

	defaultEncodeConcurrency    = 1
	defaultThumbnailConcurrency = 1
	// defaultCleanupConcurrency は cleanup キューの MaxWorkers。delete_reconcile と
	// catalog_export はそれぞれ UniqueOpts{ByArgs} で自分自身の重複実行を防ぐので、
	// 「両方が同時に 1 本ずつ走れる」ぶんだけ確保すれば足り、それ以上に増やす理由がない。
	defaultCleanupConcurrency = 2

	// riverQueueNameMaxLen は River のキュー名の最大長
	// （river@v0.40.0/client.go:2335,2337-2348、validateQueueName）。
	// 修飾後のキュー名（qualifyQueueName）はこの上限を超えられない。
	riverQueueNameMaxLen = 64
)

// siteBoundQueueNames は mirakc への到達を必要とする、site 単位のキューの
// **論理**（unqualified）名。ingest（watcher が発見した record の取り込み）・
// epg（EPG 全量同期。tuner_sync も同じキューを使う）・reconciler（宣言的同期
// パス）・watcher（record_sweep。recordSweepQueue の実体はこの名前）の 4 つ。
//
// この 1 つの変数が 2 つの役目を持つ（意図的に単一の変数に統一している ---
// 分けると M4-11 が M4-13 に申し送った罠を再現する。issue #185 のコメント参照）:
//  1. qualifyQueueName で物理キュー名（`<base>_<site>`）に展開する対象の判定
//     （physicalQueueName）
//  2. worker ロールを 0 サイト束縛（中央プロセス）で起動してよいかの判定
//     （RequiresSiteBinding、cmd/rokuban.validateSiteBinding が使う）
//
// **ruler はここに入らない。** ruler は mirakc に一切触れない（不変条件 1）
// DB のみの仕事で、キュー名も修飾しない（#138 の決定、issue #185 の「含むもの」1
// の表）。0 サイト束縛の中央 worker が ruler キューを購読しても、届く
// ruler_pass ジョブは args.Site が指す DB 行だけを触るので安全に処理できる
// （RulerPassWorker のコメント「site 照合ガードは不要と判断」）。M4-11 時点では
// 保守的に ruler も site-bound 扱いにしていたが、#138 の決定表（site 軸:
// 非依存）に合わせてここで外す。
//
// 対照的に site 非依存のキューは river.QueueDefault・ruler・encode・thumbnail・
// cleanup・storage の 6 つ（アーカイブとエンコードプロファイルは単一で、site の
// 属性を持たない。ruler は DB のみ。storage は issue #238 M7-5 で追加、アーカイブ/
// スクラッチが単一なのは cleanup と同じ理由）。
var siteBoundQueueNames = []string{ingestQueue, epgQueue, reconcilerQueue, recordSweepQueue}

// qualifyQueueName は site 単位のキューの物理名を組み立てる下請け
// （issue #185 M4-13）。区切り文字は `_`。site が空文字列なら db.DefaultSite に
// 解決する --- verifySite と同じ規約で、単体テストの部分構成（Site 未設定の
// JobArgs や ClientConfig.BoundSite 未設定）でも決定的な名前になる。
//
// **直接呼ばない。** base が siteBoundQueueNames に含まれるかどうかの判定を
// 呼び出し側に委ねると、判定を書き忘れた場所だけ「site で修飾しない」という
// 別の意味論になり、insert 側と subscribe 側の 2 か所で判定がズレる余地が生まれる
// （issue #185 のレビューで発見: 当初 InsertOpts はこの関数を直接呼び、
// allQueues/buildRiverConfig の購読側だけが physicalQueueName で門番していた。
// その状態で siteBoundQueueNames から要素を 1 つ外すと、insert 側は今まで通り
// 修飾済みキュー名を書き込み、subscribe 側だけが非修飾名に切り替わるという
// 「挿入先と購読先が食い違う」壊れ方をした）。**必ず physicalQueueName を経由
// する**（InsertOpts も buildRiverConfig もこの 1 系統だけを通る）。
func qualifyQueueName(base, site string) string {
	if site == "" {
		site = db.DefaultSite
	}
	return base + "_" + site
}

// physicalQueueName は論理キュー名（worker.queues の設定値、AllQueueNames が
// 返す名前、各 JobArgs.InsertOpts が使うキュー定数）から、このプロセスが実際に
// River へ Insert/購読する物理キュー名を返す。siteBoundQueueNames に含まれる
// 論理名だけ qualifyQueueName で site 修飾し、それ以外
// （ruler/encode/thumbnail/cleanup/default）はそのまま返す。
//
// **insert 側（各 JobArgs.InsertOpts）と subscribe 側（buildRiverConfig）の
// 両方がこの関数を経由する。** 単一の関数に統一しているのは、site 単位の
// キュー集合（siteBoundQueueNames）の定義を 1 か所にし、insert 側と subscribe
// 側で別々に判定してズレる経路を構造的に無くすため（qualifyQueueName のコメント
// 参照）。
func physicalQueueName(logical, boundSite string) string {
	if !slices.Contains(siteBoundQueueNames, logical) {
		return logical
	}
	return qualifyQueueName(logical, boundSite)
}

// ValidateSiteForQueueNames は、site を site 単位のキューに修飾したときに
// River のキュー名の上限（riverQueueNameMaxLen、64 文字）を超えないことを検査する。
//
// internal/config はレジストリに書かれた site 名をロード時にこの上限の範囲で
// 検査している（config.MirakcSiteNameMaxLen。TestSiteBoundQueueNames_
// FitWithinMirakcSiteNameMaxLen が、config 側の上限が ValidateSiteForQueueNames の
// 上位集合であることを固定している）。ValidateSiteForQueueNames はそれとは独立に、
// site 名が config 以外の経路（コマンドライン引数・環境変数等）から来る場合に
// 備えた最後の砦として残す。
//
// cmd/rokuban が --sites で束縛した各サイト、および `rokuban enqueue --site` で
// 指定されたサイトについて、起動時 / 投入前に呼ぶ。
func ValidateSiteForQueueNames(site string) error {
	for _, base := range siteBoundQueueNames {
		name := qualifyQueueName(base, site)
		if len(name) > riverQueueNameMaxLen {
			return fmt.Errorf(
				"site %q: queue name %q (%d chars) would exceed River's %d character limit "+
					"once site-qualified; shorten the site name",
				site, name, len(name), riverQueueNameMaxLen)
		}
	}
	return nil
}

// allQueues はこのプロセスが知っている全キューの**論理**（unqualified）名とその
// 設定。worker.queues（ClientConfig.Queues）で絞り込む際の許可リストになり、
// AllQueueNames / RequiresEncodeTools / RequiresSiteBinding もこの論理名の
// 集合に対して判定する --- worker.queues の設定値・エラーメッセージのいずれも
// 論理名のままにするため（issue #185 の「罠」「エラーメッセージも論理名で出す」）。
//
// site 単位のキュー（siteBoundQueueNames）を実際に Insert/購読するときの物理名
// （`<base>_<site>`）への展開は physicalQueueName が担う。buildRiverConfig が
// river.Config.Queues を組み立てる直前にこの展開を行う。
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
		// delete_reconcile / catalog_export 用（issue #185 M4-13。cleanupQueue のコメント参照）。
		cleanupQueue: {MaxWorkers: defaultCleanupConcurrency},
		// storage_sync 用（issue #238 M7-5）。UniqueOpts{ByArgs} が重複実行を防ぐので
		// tuner_sync/ruler/reconciler/record_sweep と同じく 1 本で足りる。
		storageQueue: {MaxWorkers: 1},
	}
}

// ClientConfig は River クライアントの設定。
type ClientConfig struct {
	// BoundSite はこのプロセスが束縛されている単一の mirakc サイト名
	// （cmd/rokuban が --sites から解決する。空文字列は 0 サイト束縛（中央プロセス）
	// を意味する。issue #183 M4-11 / #185 M4-13）。
	//
	// siteBoundQueueNames に含まれる論理キュー（ingest/epg/reconciler/watcher）を
	// 実際に Insert/購読する際の物理名（`<base>_<site>`）を組み立てるのに使う
	// （physicalQueueName、buildRiverConfig）。空文字列は qualifyQueueName が
	// db.DefaultSite に解決する --- 0 サイト束縛の worker がこれらのキューを
	// 要求しないことは cmd/rokuban.validateSiteBinding が起動時に強制するので、
	// ここで実際に "" が site-bound キューの qualify に使われるのは
	// テストの部分構成（BoundSite 未設定）だけである。
	BoundSite string

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

	// EncodeReconcile が true なら encode の desired−observed 定期パス
	// （issue #163）を定期ジョブとして登録する（PeriodicJobs が true のときのみ）。
	// CatalogExport / DeleteReconcile と同じくサイト非依存
	// （エンコードは site の属性を持たない。EncodeReconcileArgs のコメント参照）。
	EncodeReconcile bool

	// EncodeReconcileInterval は encode reconcile の間隔。0 なら既定値（15 分）。
	EncodeReconcileInterval time.Duration

	// StorageSync が true ならストレージ観測（issue #238 M7-5）を定期ジョブとして
	// 登録する（PeriodicJobs が true のときのみ）。CatalogExport / DeleteReconcile と
	// 同じくサイト非依存（観測対象は単一の MediaDir / ScratchDir）。
	StorageSync bool

	// StorageSyncInterval はストレージ観測の間隔。0 なら既定値（5 分）。
	StorageSyncInterval time.Duration

	// PeriodicJobs が false なら、EpgSyncSite / TunerSyncSite / RulerPassSite /
	// ReconcilePassSite / RecordSweepSite / CatalogExport / DeleteReconcile /
	// EncodeReconcile / StorageSync が設定されていても River の PeriodicJobs を
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

	// all / queues は論理（unqualified）名で組み立てる --- worker.queues の設定値・
	// 未知キューのエラーメッセージのいずれも論理名のままにするため
	// （issue #185 の「罠」「エラーメッセージも論理名で出す」。allQueues のコメント参照）。
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

	// river.Config.Queues は実際に SKIP LOCKED で引く物理キュー名でなければならない。
	// ここで初めて論理名から物理名（`<base>_<site>`）に展開する（physicalQueueName）。
	physicalQueues := make(map[string]river.QueueConfig, len(queues))
	for name, qc := range queues {
		physicalQueues[physicalQueueName(name, cfg.BoundSite)] = qc
	}

	// 実際に SKIP LOCKED で引く物理キュー名の集合をログに出す（issue #185 の
	// 「罠」: 全キュー購読（既定）は「全部購読しているつもりで実は site 束縛
	// キューを引いていない」を起動時に伝える手段が要る。KEDA のスケーラ定義を
	// 合わせる運用者と、「ジョブが available のまま動かない」を調べる人が
	// 最初に見る情報になる）。
	slog.Info("worker: subscribing to queues",
		"queues", sortedQueueNames(physicalQueues), "bound_site", cfg.BoundSite)

	riverCfg := &river.Config{
		Queues:  physicalQueues,
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
		if cfg.EncodeReconcile {
			interval := cfg.EncodeReconcileInterval
			if interval <= 0 {
				interval = defaultEncodeReconcileInterval
			}
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return EncodeReconcileArgs{}, nil
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			))
		}
		if cfg.StorageSync {
			interval := cfg.StorageSyncInterval
			if interval <= 0 {
				interval = defaultStorageSyncInterval
			}
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, river.NewPeriodicJob(
				river.PeriodicInterval(interval),
				func() (river.JobArgs, *river.InsertOpts) {
					return StorageSyncArgs{}, nil
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

// RequiresSiteBinding は、worker.queues の設定でこのプロセスが site 単位のキュー
// （ingest/epg/reconciler/watcher。siteBoundQueueNames 参照）のいずれかを購読するかを
// 返す。空スライスは「全キュー購読」を意味するので true になる。
//
// RequiresEncodeTools と対になる判定で、worker ロールを 0 サイト束縛（中央プロセス、
// issue #183 M4-11 の `--sites=`）で起動できるかを決める。site 単位のキューを購読する
// worker は mirakc へのアクセス（Deps.MirakcClient）と site 単位のジョブ照合
// （internal/worker/worker.go の verifySite）を必要とし、束縛サイトが無いと
// 空文字列 site として処理され、届いたジョブの site と一致しないため全滅して
// 再試行し続ける。ruler/encode/thumbnail/cleanup/default だけに絞った worker
// （worker.queues でそれ以外を明示的に外した構成）は site 非依存なので
// 0 サイト束縛でも安全に動く（ruler が site 非依存である理由は
// siteBoundQueueNames のコメント参照）。
func RequiresSiteBinding(queues []string) bool {
	if len(queues) == 0 {
		return true
	}
	for _, q := range siteBoundQueueNames {
		if slices.Contains(queues, q) {
			return true
		}
	}
	return false
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
