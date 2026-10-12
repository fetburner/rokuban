package riveradapter

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/tsscan"
)

const (
	// defaultIngestConcurrency / defaultEpgSyncInterval は config.Ingest.Concurrency
	// / config.Epg.SyncInterval と同値の二重既定（config.defaults() 参照）だが、
	// 消さずに残す。Config は cmd/rokuban 経由の本番配線だけでなく、この
	// パッケージの多数のテストが `Config{...}` を直接組み立てて渡す
	// ライブラリ境界でもある（config.Load を経由しない）。
	//
	// **既定値の権威は config 側にあり、ingest.concurrency を 3 にした根拠も
	// そちら（config.IngestConfig.Concurrency の doc コメント）に書いてある。**
	//
	// IngestConcurrency=0 を渡すと allQueues の
	// `river.QueueConfig{MaxWorkers: 0}` になり、river.NewClient がこれを
	// 明示的な起動時エラーで拒否する（river@v0.47.0 client.go の
	// `c.MaxWorkers < 1` チェック。読んで確認済み）。
	// EpgSyncInterval=0 は `river.PeriodicInterval(0)` になり、こちらは panic
	// しない（PeriodicInterval.Next は単に `t.Add(0) = t` を返すだけ）が、
	// 定期ジョブの nextRunAt が前に進まなくなるため、river 内部の
	// PeriodicJobEnqueuer がタイマーを毎回ほぼ 0 でリセットするビジーループに
	// 陥り、ループ 1 周ごとに insertParamsFromConstructor + insertBatch で
	// Postgres への insert を試み続ける。river@v0.47.0 internal/maintenance/
	// periodic_job_enqueuer.go を読んで確認した --- タイマー側は
	// timeUntilNextRun / timerUntilNextRun.Reset、insert ループ側は
	// insertParamsFromConstructor / insertBatch。実際に client を起動してこのループを観測してはいない。
	//
	// 責務を config に移すのは config.Config の話であって、config を経由しない
	// 呼び出し元（このパッケージのテスト）まで持つ Config の 0-fallback を
	// 消す話ではない（issue #445 のファイル列挙が ingest.go/epg.go/storage.go に
	// 留めていたのもこの境界の違いから）。
	defaultIngestConcurrency = 3
	defaultEpgSyncInterval   = 10 * time.Minute

	// defaultTunerSyncInterval はチューナー射影の既定間隔。
	//
	// チューナー構成の変更には mirakc の再起動が要るので更新頻度は低くてよく、
	// EPG 全量同期と同じ 10 分で足りる（issue #21「EPG 全量同期（既定 10 分）と
	// 同じジョブで投影すれば十分」）。専用の設定キーは設けていない --- 運用者が
	// 触る理由がないので、設定面を広げない。
	defaultTunerSyncInterval       = 10 * time.Minute
	defaultTSScanReconcileInterval = 15 * time.Minute

	// defaultRulerPassInterval は定期パスの既定間隔。docs/recording.md §3.1 の
	// 起動契機の表で「定期（既定 10 分）」としている値と揃える。
	defaultRulerPassInterval = 10 * time.Minute

	// defaultReconcilePassInterval は定期パスの既定間隔。従来の reconciler の
	// 自前タイマー（reconciler.defaultConfig の ReconcileInterval）が使っていた
	// 30 秒をそのまま引き継ぐ（docs/recording.md §3.2「定期（既定 30 秒）」）。
	defaultReconcilePassInterval = 30 * time.Second

	// defaultRecordSweepInterval は定期パスの既定間隔。従来の watcher の自前タイマー
	// （旧 watcher.defaultConfig の ReconcileInterval）が使っていた 5 分をそのまま
	// 引き継ぐ（docs/recording.md §3.3）。
	defaultRecordSweepInterval = 5 * time.Minute

	// defaultCatalogExportInterval は catalog エクスポートの既定間隔（日次相当）。
	// docs/storage.md §8「日次 + 世代保持」。
	defaultCatalogExportInterval = 24 * time.Hour

	// defaultDeleteReconcileInterval は削除 reconcile 定期パスの既定間隔。
	defaultDeleteReconcileInterval = 15 * time.Minute

	// defaultLabelRuleReconcileInterval は定期の全件再評価の既定間隔。
	//
	// これは必須の窓閉じである。分類ルールの編集の前後でコミットした録画は、
	// 旧ルールで評価された当たりを持ったままになる（トリガーは自分の行しか
	// 見ない）。encode 系の定期パスと同じ 15 分に揃える。
	defaultLabelRuleReconcileInterval = 15 * time.Minute

	// defaultEncodeReconcileInterval は encode の desired−observed 定期パスの
	// 既定間隔。
	//
	// このパスが**新しく投入する**のは、ヒント経路（ingest 完了時のベストエフォート
	// 投入と POST /api/recordings/{id}/encode-profiles のヒントジョブ）が落ちた
	// ぶんだけである（候補には実行中・待機中のエンコードも含まれるが、それらへの
	// 投入は、args（締切を含む）が同じ間だけ River の UniqueOpts が合流させる）。
	// 落としたヒントの回復が数十分遅れても失うものは無い一方、何時間も放置は
	// したくないので、同じく「普段は新しい仕事を作らないバックストップ」である
	// delete_reconcile と同じ 15 分に揃える。
	defaultEncodeReconcileInterval = 15 * time.Minute

	// defaultThumbnailReconcileInterval は thumbnail の desired−observed 定期パスの
	// 既定間隔。ingest 完了時のヒント投入を補うバックストップとして encode
	// reconcile と揃える。
	defaultThumbnailReconcileInterval = 15 * time.Minute

	defaultCMDetectReconcileInterval = 15 * time.Minute

	// defaultStorageSyncInterval はストレージ観測の既定間隔。
	//
	// tuner_sync（10 分）より短くしているが、この値そのものに測定的根拠はない
	// （未検証。record_sweep の 5 分に合わせただけの初期値）。実際のアラート整備
	// （time() 差分での閾値検討）は本 PR のスコープ外 --- rokuban_storage_root_*
	// メトリクスを export するようになった今は組めるようになったが、アラート自体は
	// 未着手。専用の設定キーは設けていない --- tuner_sync / catalog_export /
	// delete_reconcile / record_sweep の既定間隔と同じく、運用者が調整する理由が
	// 今のところ無い（docs/runbook/setup.md の record_sweep の前例。issue #238）。
	defaultStorageSyncInterval = 5 * time.Minute
)

// DefaultSoftStopTimeout は Config.SoftStopTimeout の既定値。
//
// River v0.47.0 の公開契約では、Stop または Start に渡した ctx のキャンセルで
// graceful stop を始めると、この猶予の後に実行中ジョブの ctx が cancel される。
// StopAndCancel は猶予を待たずに cancel する。中断されたジョブは attempt が
// 開始前の値に戻り、errors を増やさず `available` に戻る（River v0.47.0 API:
// Client.Stop / StopAndCancel, Config.SoftStopTimeout; CHANGELOG.md §0.44.0）。
//
// **5 秒という値は、既定の停止猶予の内側に収めるために選んでいる。** Docker の
// 既定猶予は 10 秒、k8s は 30 秒である（実測: 猶予を使い切る停止でも 5.1 秒）。
// **どのジョブが 5 秒で終わるかは測っていない。**
//
// 既定を長く（例えば 30 秒に）すると、猶予を書いていないデプロイで**プロセスが
// 畳み終える前に SIGKILL が来る**。実測: 既定 30 秒のプロセスは停止に 30.09 秒
// 必要で、k8s の既定猶予 30 秒に 0.09 秒負けた。負けると River の行は一時的に
// `running` のまま残る。ingest は有限の Timeout と `JobRescuer`（既定 6 分）、
// encode は保存した締切後の `JobRescuer`、cm_detect は `cm_detect_reconcile` が回収する。
// その他のジョブも `JobRescuer` に委ねられる --- **設定を間違えた人ではなく、何も書かなかった人に
// 当たる**壊れ方を増やさないためにも猶予を正しく設定する。
//
// 数時間かかる encode はこの既定では完走できない。ingest は短い区切りで
// checkpoint と snooze を行う。長い drain を要するデプロイは `--soft-stop-timeout` と
// k8s の `terminationGracePeriodSeconds` を **対で**引き上げる
// （docs/operations.md §5「Deployment 併用時」）。
//
// **0 を「無制限」の意味に使えない。** River 自身の SoftStopTimeout の既定は無期限
// 待ちだが、この adapter は 0 をこの 5 秒の既定値に読み替える。cmd/rokuban 側も
// `--soft-stop-timeout` の 0 以下を起動エラーにする。
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
// するときの物理名（`<base>_<site>`）への展開は jobs.PhysicalQueueName が担う。buildConfig が
// river.Config.Queues を組み立てる直前にこの展開を行う。
func allQueues(ingestConcurrency, encodeConcurrency, thumbnailConcurrency int) map[string]river.QueueConfig {
	return map[string]river.QueueConfig{
		jobs.DefaultQueue: {MaxWorkers: 100},
		jobs.IngestQueue:  {MaxWorkers: ingestConcurrency},
		// 全量同期が重ならないよう 1 本に絞る。tuner_sync も同じキューを使う
		// （どちらも使い捨てプロジェクションの全量同期。TunerSyncArgs.Spec 参照）。
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
	// JobTimeout は River の既定ジョブタイムアウト（1 分）。Spec.Timeout がある worker はその値を使う。
	// 共有契約テストが RescueStuckJobsAfter を短くするために 0 でない値を渡す。River は
	// RescueStuckJobsAfter < JobTimeout を設定エラーにする（river@v0.47.0 client.go の検証）ので、
	// この値はその検証を通すためにある。
	JobTimeout time.Duration

	// TestOnly は River の TestOnly を有効にする。メンテナンスサービスの起動遅延を省いて
	// Start / Stop を速くする契約テスト用で、本番では使わない（river@v0.47.0 の doc も
	// テスト以外での使用を禁じている）。
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
	// River v0.47.0 の公開契約では、SoftStopTimeout が設定されている場合、Stop または
	// Start ctx の cancel は graceful stop となり、期限後にジョブ ctx を cancel する。
	// StopAndCancel はこの猶予を待たずに cancel する（River API: Client.Start / Stop /
	// StopAndCancel, Config.SoftStopTimeout）。中断されたジョブは attempt が開始前の値に
	// 戻り、errors を増やさず `available` に戻る（River v0.47.0 CHANGELOG.md §0.44.0）。
	//
	// 1 件消化モードの正常終了では `stopOnceProcess` が graceful stop を使うため、
	// すでに掴んでいる追加ジョブの完走待ちにもこの猶予が適用される。
	SoftStopTimeout time.Duration

	// RescueStuckJobsAfter は river.Config.RescueStuckJobsAfter にそのまま渡す。
	// 0 なら River の既定（1h）。設定キーは worker.rescue_stuck_jobs_after。
	RescueStuckJobsAfter time.Duration

	// Once が非 nil なら 1 件消化モード（`rokuban server --once`）になる。
	// KEDA ScaledJob が起こした k8s Job が自分で終了できるようにするための
	// 起動形態で、ジョブ 1 件の Work を抜けたら（成功・失敗を問わず）
	// プロセスを畳む（OnceGate、docs/operations.md §5）。
	//
	// このモードでは buildConfig が次の 3 つを強制する:
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

// buildConfig は Config から river.Config を組み立てる。
//
// newRegistryClient から分離してあるのは、DB 接続なしで queues 検証や
// PeriodicJobs の有無を単体テストできるようにするため。
func buildConfig(workers *river.Workers, cfg Config) (*river.Config, error) {
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
	if err := configurePeriodicJobs(riverCfg, cfg); err != nil {
		return nil, fmt.Errorf("configuring periodic jobs: %w", err)
	}
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
// （各 Args の Spec）は 1 ジョブの args.Site から物理名が 1 つに決まるので
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
func configurePeriodicJobs(riverCfg *river.Config, cfg Config) error {
	if !cfg.PeriodicJobs {
		return nil
	}
	if err := configureSitePeriodicJobs(riverCfg, cfg); err != nil {
		return err
	}
	return configureGlobalPeriodicJobs(riverCfg, cfg)
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
func configureSitePeriodicJobs(riverCfg *river.Config, cfg Config) error {
	if len(cfg.BoundSites) == 0 {
		return nil
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
		periodicJobs := []struct {
			interval time.Duration
			args     jobq.Args
		}{
			{epgSyncInterval, jobs.EpgSyncArgs{Site: site}},
			{tunerSyncInterval, jobs.TunerSyncArgs{Site: site}},
			{rulerPassInterval, jobs.RulerPassArgs{Site: site}},
			{reconcilePassInterval, jobs.ReconcilePassArgs{Site: site}},
			{recordSweepInterval, jobs.RecordSweepArgs{Site: site}},
		}
		for _, periodic := range periodicJobs {
			job, err := newPeriodicJob(periodic.interval, periodic.args, &river.PeriodicJobOpts{RunOnStart: true})
			if err != nil {
				return fmt.Errorf("building %s periodic job for site %q: %w", periodic.args.Kind(), site, err)
			}
			riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, job)
		}
	}
	return nil
}

// configureGlobalPeriodicJobs はサイトに依存しない定期ジョブを登録する。
func configureGlobalPeriodicJobs(riverCfg *river.Config, cfg Config) error {
	appendPeriodic := func(enabled bool, interval, defaultInterval time.Duration, args jobq.Args) error {
		if !enabled {
			return nil
		}
		if interval <= 0 {
			interval = defaultInterval
		}
		// 起動直後にも 1 回書いておく。日次なので RunOnStart で初回を確保する
		// （元は CatalogExport 単独の判断。EncodeReconcile / DeleteReconcile /
		// StorageSync も同じ RunOnStart: true を共有しているが、それぞれの間隔での
		// 「起動直後の 1 回」が同程度に有用かは個別に検証していない）。
		job, err := newPeriodicJob(interval, args, &river.PeriodicJobOpts{RunOnStart: true})
		if err != nil {
			return fmt.Errorf("building %s periodic job: %w", args.Kind(), err)
		}
		riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, job)
		return nil
	}
	periodicJobs := []struct {
		enabled         bool
		interval        time.Duration
		defaultInterval time.Duration
		args            jobq.Args
	}{
		{cfg.CatalogExport, cfg.CatalogExportInterval, defaultCatalogExportInterval, jobs.CatalogExportArgs{}},
		{cfg.DeleteReconcile, cfg.DeleteReconcileInterval, defaultDeleteReconcileInterval, jobs.DeleteReconcileArgs{}},
		{cfg.LabelRuleReconcile, cfg.LabelRuleReconcileInterval, defaultLabelRuleReconcileInterval, jobs.LabelRuleReconcileArgs{}},
		{cfg.EncodeReconcile, cfg.EncodeReconcileInterval, defaultEncodeReconcileInterval, jobs.EncodeReconcileArgs{}},
		{cfg.ThumbnailReconcile, cfg.ThumbnailReconcileInterval, defaultThumbnailReconcileInterval, jobs.ThumbnailReconcileArgs{}},
		{cfg.TSScanReconcile, 0, defaultTSScanReconcileInterval, tsscan.ReconcileArgs{}},
		{cfg.CMDetectReconcile, 0, defaultCMDetectReconcileInterval, jobs.CMDetectReconcileArgs{}},
		{cfg.StorageSync, cfg.StorageSyncInterval, defaultStorageSyncInterval, jobs.StorageSyncArgs{}},
	}
	for _, periodic := range periodicJobs {
		if err := appendPeriodic(periodic.enabled, periodic.interval, periodic.defaultInterval, periodic.args); err != nil {
			return err
		}
	}
	return nil
}

// newPeriodicJob maps the jobq specification to River options when the configuration
// is built. Args no longer carry River options, so a periodic callback that returned
// nil options would insert into River's default queue without unique/coalescing options
// (TestTSScanReconcilePeriodicUsesScanQueueAndCoalesces).
func newPeriodicJob(interval time.Duration, args jobq.Args, opts *river.PeriodicJobOpts) (*river.PeriodicJob, error) {
	riverArgs, ok := args.(river.JobArgs)
	if !ok {
		return nil, fmt.Errorf("%q args %T do not implement River JobArgs", args.Kind(), args)
	}
	insertOpts, err := makeInsertOpts(args)
	if err != nil {
		return nil, err
	}
	return river.NewPeriodicJob(river.PeriodicInterval(interval), func() (river.JobArgs, *river.InsertOpts) {
		options := insertOpts
		return riverArgs, &options
	}, opts), nil
}

func sortedQueueNames(m map[string]river.QueueConfig) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// recoveryMargin は RecoveryGrace が River の救済閾値に足す余裕。
//
// 救済閾値を過ぎた running ジョブを rescuer が実際に拾うまでの遅れを見込む値だが、
// rescuer の実行間隔は river@v0.47.0 の river.Config の公開ドキュメントに書かれておらず、
// この 2 分の根拠は未検証である。測定は 1 回だけで、共有契約テストのプロセス死ケース
// （閾値 6 秒、attempted_at はクラッシュ時刻）の再配送に 35.6 秒かかり、この余裕の範囲に
// 収まった。rescuer の間隔が環境で変わるかは見ていない。
const recoveryMargin = 2 * time.Minute

// RecoveryGrace は jobq.Spec.Timeout の契約が言う「回復猶予」の申告値を返す。
// プロセスが死んだ running ジョブが、Timeout の経過後この時間以内に再配送の対象になることを
// adapter として申告する。cmd/rokuban が worker へ渡し、共有契約テストの fixture も同じ値を使う。
//
// River の救済閾値 river.Config.RescueStuckJobsAfter は、公開ドキュメント（river@v0.47.0
// client.go）に「既定は 1 時間、JobTimeout が 1 時間を超えるなら JobTimeout + 1 時間」とある。
// 0 のときのこの解釈はここに閉じ込める。申告値は閾値に recoveryMargin を足す。
func (c Config) RecoveryGrace() time.Duration {
	after := c.RescueStuckJobsAfter
	if after == 0 {
		after = time.Hour
		if c.JobTimeout > time.Hour {
			after = c.JobTimeout + time.Hour
		}
	}
	return after + recoveryMargin
}
