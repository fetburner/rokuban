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

// QueueFunc は args が投入される物理キュー名を返す。命名規則は製品の規則で、
// 起動形態では変わらないので Config ではなくコンストラクタの引数で受ける。
// 投入側（api / watcher / CLI）と worker の購読側が同じ関数を通ることで、
// 投入先と購読先が食い違う「黙って動かない」を作らない。
type QueueFunc func(args jobq.Args) (string, error)

// Config は River クライアントの設定。
//
// 何を購読し何を定期投入するかは Spec が持つ（製品の方針。組み立ては cmd/rokuban）。
// ここに残るのは、誰が定期投入の契機を持つか・どのキューを引くかといった起動形態と、
// River のチューニングだけである。
type Config struct {
	// Spec は購読するキューと定期ジョブの仕様。NewAdapter が Validate と、
	// 定期ジョブの kind が registry で解決できることの検査を行う。
	Spec jobq.WorkerSpec

	// JobTimeout は River の既定ジョブタイムアウト（1 分）。Spec.Timeout がある worker はその値を使う。
	// 共有契約テストが RescueStuckJobsAfter を短くするために 0 でない値を渡す。River は
	// RescueStuckJobsAfter < JobTimeout を設定エラーにする（river@v0.47.0 client.go の検証）ので、
	// この値はその検証を通すためにある。
	JobTimeout time.Duration

	// TestOnly は River の TestOnly を有効にする。メンテナンスサービスの起動遅延を省いて
	// Start / Stop を速くする契約テスト用で、本番では使わない（river@v0.47.0 の doc も
	// テスト以外での使用を禁じている）。
	TestOnly bool

	// SchedulePeriodicJobs が false なら、Spec.PeriodicJobs があっても River の PeriodicJobs を
	// 一切登録しない（Spec の定期ジョブの一覧は空にならない）。
	// k8s では false にして、CronJob が
	// `rokuban enqueue` を叩く形に委ねる（docs/data.md §2「定期実行の契機は
	// デプロイ形態に委ねる」。設定キーは worker.periodic_jobs、既定 true）。
	SchedulePeriodicJobs bool

	// Queues は実際に SKIP LOCKED で引くキューを論理名で絞り込む。空なら Spec の全キューを引く。
	// ロールを増やさずに「ruler / reconciler だけ別 Pod」のような構成を実現するための knob
	// （docs/configuration.md の worker.queues。docs/overview.md「ロールは
	// 『プロセスの形』を表し、『どの仕事をするか』は表さない」）。
	// 未知のキュー名が含まれる場合は起動時エラーにする（typo で静かに何も
	// 引かなくなる事故を防ぐ。判定は jobq.WorkerSpec.Select）。
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
	// 0 なら River の既定（公開ドキュメントは 1h、JobTimeout が長ければ JobTimeout + 1h。
	// RecoveryGrace は JobTimeout > 0 なら常に JobTimeout + 1h とみなす）。設定キーは worker.rescue_stuck_jobs_after。
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
	//   - SchedulePeriodicJobs は false（定期投入を Job の起動回数に委ねない）
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
// 定期ジョブの有無を単体テストできるようにするため。
func buildConfig(workers *river.Workers, queueFor QueueFunc, cfg Config) (*river.Config, error) {
	if err := cfg.Spec.Validate(); err != nil {
		return nil, err
	}
	// 論理名のまま選ぶ --- worker.queues の設定値・未知キューのエラーメッセージのいずれも
	// 論理名にするため。
	spec, err := cfg.Spec.Select(cfg.Queues)
	if err != nil {
		return nil, err
	}
	if err := configureOnceQueues(cfg, &spec); err != nil {
		return nil, err
	}
	physicalQueues, err := riverQueues(spec)
	if err != nil {
		return nil, err
	}
	if err := validateOncePhysicalQueues(cfg, physicalQueues); err != nil {
		return nil, err
	}

	// 実際に SKIP LOCKED で引く物理キュー名の集合をログに出す。全キュー購読（既定）が
	// 「全部購読しているつもりで実は site 束縛キューを引いていない」を起動時に伝える
	// 手段になる。KEDA のスケーラ定義を合わせる運用者と、「ジョブが available のまま
	// 動かない」を調べる人が最初に見る情報になる。
	slog.Info("worker: subscribing to queues", "queues", sortedQueueNames(physicalQueues))

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
	if err := configurePeriodicJobs(riverCfg, queueFor, cfg); err != nil {
		return nil, fmt.Errorf("configuring periodic jobs: %w", err)
	}
	return riverCfg, nil
}

// riverQueues は選ばれた仕様を、実際に SKIP LOCKED で引く物理キューの River 設定へ変換する。
// 物理名の展開は仕様が持っているので、ここでは写すだけである。
func riverQueues(spec jobq.WorkerSpec) (map[string]river.QueueConfig, error) {
	queues := make(map[string]river.QueueConfig, len(spec.Queues))
	for _, queue := range spec.Queues {
		if len(queue.Physical) == 0 {
			return nil, fmt.Errorf("queue %q has no physical queue to subscribe: "+
				"a site-bound queue needs at least one bound site (--sites)", queue.Logical)
		}
		for _, physical := range queue.Physical {
			queues[physical] = river.QueueConfig{MaxWorkers: queue.MaxWorkers}
		}
	}
	return queues, nil
}

// configureOnceQueues は once モードの論理キュー検証と同時実行数の制限を行う。
// spec.Queues は書き換える（Select が複製済み）。
func configureOnceQueues(cfg Config, spec *jobq.WorkerSpec) error {
	if cfg.Once == nil {
		return nil
	}
	// **1 キューだけ**を要求する。ScaledJob はキュー単位に作る（寿命 ×
	// site 束縛で切る。docs/operations.md §5）ので、1 件消化モードの Job が
	// 複数キューを引く形は KEDA 側に対応する定義が無い。加えて、複数キューを
	// 許すと MaxWorkers=1 でもキューごとに 1 件ずつ掴めるため「同時 claim は
	// 1 件」が成立しない（River の MaxWorkers はキュー単位）。
	if len(spec.Queues) != 1 {
		names := make([]string, 0, len(spec.Queues))
		for _, queue := range spec.Queues {
			names = append(names, queue.Logical)
		}
		sort.Strings(names)
		return fmt.Errorf("once mode requires exactly one queue, got %d (%s): "+
			"a KEDA ScaledJob is created per queue, so pass exactly one queue "+
			"(--queues <name> or worker.queues); an empty setting means all queues",
			len(spec.Queues), strings.Join(names, ", "))
	}
	// 定期投入を「Job が何回起きたか」に委ねない。River の PeriodicJobs は
	// リーダーに選出されたクライアントだけが投入するので、1 件で終わる Job が
	// リーダーになると投入の間隔が KEDA のスケール挙動で決まってしまう
	// （RunOnStart のぶんが起動ごとに入る）。docs/data.md §2 が定期実行の
	// 契機をデプロイ形態に委ねたのは CronJob に委ねるためであって、
	// Job の起動回数に委ねるためではない。
	if cfg.SchedulePeriodicJobs {
		return fmt.Errorf("once mode requires periodic jobs to be disabled " +
			"(set worker.periodic_jobs: false and insert periodic jobs with the " +
			"`rokuban enqueue` CronJobs instead)")
	}
	// 同時 claim を 1 件に抑える（仕様の並列数より優先する）。
	for i := range spec.Queues {
		spec.Queues[i].MaxWorkers = 1
	}
	return nil
}

// validateOncePhysicalQueues は site 展開後も once モードが 1 キューになることを検証する。
//
// once モードは KEDA ScaledJob がキュー単位（かつ site 単位。
// docs/operations.md §5「キュー × 置き場所 × site 軸」）に作る前提なので、
// 展開後もちょうど 1 つの物理キューでなければならない。configureOnceQueues の
// 「論理キューはちょうど 1 つ」の検査だけでは、その 1 つが site-bound キューで
// 束縛 site が 2 つ以上のとき、ここで初めて 2 物理キューに割れてしまう
// （この検査が無いと once モードの「同時 claim は 1 件」の前提が黙って崩れる）。
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
	// スケーラ定義と購読集合を突き合わせる運用者が見る行なので、形を変えない。
	// キューはちょうど 1 つ（configureOnceQueues / validateOncePhysicalQueues の
	// 検査）なので単数で出す。
	slog.Info("worker: once mode enabled (exits after one job)",
		"queue", sortedQueueNames(physicalQueues)[0])
}

// configurePeriodicJobs は Spec の定期ジョブを River 設定へ登録する。
// 誰が投入の契機を持つか（SchedulePeriodicJobs）だけを見て、一覧と周期は Spec に従う。
func configurePeriodicJobs(riverCfg *river.Config, queueFor QueueFunc, cfg Config) error {
	if !cfg.SchedulePeriodicJobs {
		return nil
	}
	for _, periodic := range cfg.Spec.PeriodicJobs {
		// jobq.PeriodicJobSpec の契約どおり、Start 直後に 1 回投入する
		// （TestPeriodicJobsRunOnStartAndEveryInterval が固定する）。
		job, err := newPeriodicJob(queueFor, periodic.Interval, periodic.Args, &river.PeriodicJobOpts{RunOnStart: true})
		if err != nil {
			return fmt.Errorf("building %s periodic job: %w", periodic.Args.Kind(), err)
		}
		riverCfg.PeriodicJobs = append(riverCfg.PeriodicJobs, job)
	}
	return nil
}

// newPeriodicJob maps the jobq specification to River options when the configuration
// is built. Args no longer carry River options, so a periodic callback that returned
// nil options would insert into River's default queue without unique/coalescing options
// (TestTSScanReconcilePeriodicUsesScanQueueAndCoalesces).
func newPeriodicJob(queueFor QueueFunc, interval time.Duration, args jobq.Args, opts *river.PeriodicJobOpts) (*river.PeriodicJob, error) {
	riverArgs, ok := args.(river.JobArgs)
	if !ok {
		return nil, fmt.Errorf("%q args %T do not implement River JobArgs", args.Kind(), args)
	}
	insertOpts, err := makeInsertOpts(queueFor, args)
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
// ドキュメントは 1 時間を超える場合だけを書いている。0 < JobTimeout <= 1h でも JobTimeout + 1h を
// 申告すれば、ドキュメントの値（1h）以上になる。回復猶予は長い側に誤るほうが安全
// （孤児判定が生きている候補を失敗させない）ので、JobTimeout > 0 なら常にこちらを使う。
// 0 のときのこの解釈はここに閉じ込める。申告値は閾値に recoveryMargin を足す。
func (c Config) RecoveryGrace() time.Duration {
	after := c.RescueStuckJobsAfter
	if after == 0 {
		after = time.Hour
		if c.JobTimeout > 0 {
			after = c.JobTimeout + time.Hour
		}
	}
	return after + recoveryMargin
}
