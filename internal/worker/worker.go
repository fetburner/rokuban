package worker

import (
	"fmt"
	"sort"
	"strings"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/mirakc"
)

const (
	defaultIngestConcurrency = 2
	defaultEpgSyncInterval   = 10 * time.Minute
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

// Deps は各ワーカーに注入する依存。
type Deps struct {
	MirakcClient *mirakc.Client
	Pool         *pgxpool.Pool
	MediaDir     string

	// EpgRetentionGrace は放送済み番組を刈り取るまでの猶予。0 なら既定値。
	EpgRetentionGrace time.Duration

	// RulerRetentionGrace は ruler が reservations / program_intents を GC するまでの
	// 猶予。0 なら ruler 側の既定値を使う。epg.retention_grace と同じ値を流用する
	// 運用を想定している（docs/recording.md §3.2「番組終了後の GC」）。
	RulerRetentionGrace time.Duration
}

// NewWorkers は全ワーカーを登録した river.Workers を返す。
func NewWorkers(deps *Deps) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, &IngestWorker{
		MirakcClient: deps.MirakcClient,
		Pool:         deps.Pool,
		MediaDir:     deps.MediaDir,
	})
	river.AddWorker(workers, &EpgSyncWorker{
		MirakcClient:   deps.MirakcClient,
		Pool:           deps.Pool,
		RetentionGrace: deps.EpgRetentionGrace,
	})
	river.AddWorker(workers, &RulerPassWorker{
		Pool:           deps.Pool,
		RetentionGrace: deps.RulerRetentionGrace,
	})
	return workers
}

// allQueues はこのプロセスが知っている全キューとその設定。worker.queues
// （ClientConfig.Queues）で絞り込む際の許可リストにもなる。
func allQueues(ingestConcurrency int) map[string]river.QueueConfig {
	return map[string]river.QueueConfig{
		river.QueueDefault: {MaxWorkers: 100},
		ingestQueue:        {MaxWorkers: ingestConcurrency},
		// 全量同期が重ならないよう 1 本に絞る。
		epgQueue: {MaxWorkers: 1},
		// サイト単位の排他は UniqueOpts が担うので、こちらも複数ワーカーは要らない。
		rulerQueue: {MaxWorkers: 1},
	}
}

// ClientConfig は River クライアントの設定。
type ClientConfig struct {
	// IngestConcurrency は ingest キューの同時実行数（サイト単位のキャップ）。0 なら既定値。
	IngestConcurrency int

	// EpgSyncSite が空でない場合、そのサイトの EPG 全量同期を定期ジョブとして登録する
	// （PeriodicJobs が true のときのみ）。
	EpgSyncSite string

	// EpgSyncInterval は EPG 全量同期の間隔。0 なら既定値。
	EpgSyncInterval time.Duration

	// RulerPassSite が空でない場合、そのサイトの ruler 1 パス評価を定期ジョブとして
	// 登録する（PeriodicJobs が true のときのみ）。
	RulerPassSite string

	// RulerPassInterval は ruler 定期パスの間隔。0 なら既定値。
	RulerPassInterval time.Duration

	// PeriodicJobs が false なら、EpgSyncSite / RulerPassSite が設定されていても
	// River の PeriodicJobs を一切登録しない。k8s では false にして、CronJob が
	// `rokuban enqueue` を叩く形に委ねる（docs/data.md §2「定期実行の契機は
	// デプロイ形態に委ねる」。設定キーは worker.periodic_jobs、既定 true）。
	PeriodicJobs bool

	// Queues は実際に SKIP LOCKED で引くキューを絞り込む。空なら全キューを引く。
	// ロールを増やさずに「ruler だけ別 Pod」のような構成を実現するための knob
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

	all := allQueues(ingestConcurrency)

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
	}

	return riverCfg, nil
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
func NewInsertOnlyClient(pool *pgxpool.Pool) (*river.Client[pgx5.Tx], error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return nil, fmt.Errorf("creating insert-only river client: %w", err)
	}
	return client, nil
}
