// Package asynqadapter は Redis 上で jobq の契約を実装する。
package asynqadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/fetburner/rokuban/internal/jobq"
)

// DefaultMaxAttempts は Spec.MaxAttempts が 0 のときの失敗回数の上限。
// adapter を差し替えても製品から見た上限が変わらないよう、River の既定に合わせる。
const DefaultMaxAttempts = 25

// coalesceTTL は同一タスクの投入を束ねる最長時間。
// 成功や RevokeTask でロックが外れることは TestContract の合流テストが固定する。
// 滞留や Snooze の連鎖が TTL を超えると、以後の投入は合流せず、重複配送は
// worker の冪等性で吸収する。短い TTL では同じ args の連鎖が並びやすくなる。
// 外部削除などでロックだけが残ると、次の reconcile の投入は TTL まで合流扱いになる。
// River の既定の回収閾値と同じ 1 時間にして、この抑止時間に上限を設ける。
const coalesceTTL = time.Hour

// RecoveryGrace はプロセス死からの回収に見込む猶予。
// lease 切れの回収は asynq の CHANGELOG の v0.22.0 と、TaskInfo.IsOrphaned の doc に記載されている。
// asynq v0.26.0 の lease 30 秒、期限後の時計ずれ猶予 30 秒、巡回間隔
// 1 分に、転送・取得とスケジューリングの余裕 10 秒を足す。
// これらの内部間隔は公開設定ではないため、TestContract のプロセス死ケースで検査する。
const RecoveryGrace = 2*time.Minute + 10*time.Second

// QueueFunc は args の投入先となる物理キュー名を返す。
type QueueFunc func(args jobq.Args) (string, error)

// Config は購読と定期投入の仕様を持つ。
type Config struct {
	// Spec は購読するキューと定期ジョブを指定する。
	Spec jobq.WorkerSpec
	// SchedulePeriodicJobs は Start 直後と周期ごとの投入を有効にする。
	SchedulePeriodicJobs bool
	// Once は RunOnce で 1 件だけ消化するモードを有効にする。
	// 物理キューは 1 つ、並列数は 1、定期投入は無効とする。
	Once bool
}

// Adapter は jobq.Enqueuer と jobq.Runner を実装する。
type Adapter struct {
	redis     redis.UniversalClient
	client    *asynq.Client
	inspector *asynq.Inspector
	registry  *jobq.Registry
	queueFor  QueueFunc
	queues    map[string]int
	periodic  []jobq.PeriodicJobSpec
	once      bool
	now       func() time.Time

	mu  sync.Mutex
	run *serverRun
}

var (
	_ jobq.Enqueuer           = (*Adapter)(nil)
	_ jobq.Runner             = (*Adapter)(nil)
	_ jobq.DeliverableCounter = (*Adapter)(nil)
)

// NewAdapter は購読と定期投入の仕様を検査し、呼び出し元が所有する Redis 接続を借りる。
// 呼び出し元は利用を終え、Stop が nil を返してからクライアントを閉じる。
// Stop が期限切れを返した場合は、再度 Stop を呼んで停止完了を待つ。
// asynq v0.26.0 の subscriber.go では、停止前に PubSub のチャネルが閉じると、
// 受信した nil のメッセージを参照して panic することをソースで確認した。
func NewAdapter(redisClient redis.UniversalClient, registry *jobq.Registry, queueFor QueueFunc, cfg Config) (*Adapter, error) {
	if redisClient == nil {
		return nil, errors.New("asynqadapter: Redis client is required")
	}
	if registry == nil {
		return nil, errors.New("asynqadapter: registry is required")
	}
	if queueFor == nil {
		return nil, errors.New("asynqadapter: queueFor is required")
	}
	if err := cfg.Spec.Validate(); err != nil {
		return nil, fmt.Errorf("validating asynq worker spec: %w", err)
	}
	if err := cfg.Spec.CheckKinds(registry); err != nil {
		return nil, fmt.Errorf("checking asynq periodic job kinds: %w", err)
	}
	if err := cfg.Spec.CheckQueueFor(queueFor); err != nil {
		return nil, fmt.Errorf("checking asynq periodic job queues: %w", err)
	}
	for _, args := range registry.ZeroArgs() {
		if _, err := queueFor(args); err != nil {
			return nil, fmt.Errorf("checking asynq queue for registered kind %q: %w", args.Kind(), err)
		}
	}
	queues, err := physicalQueues(cfg.Spec)
	if err != nil {
		return nil, err
	}
	if cfg.Once {
		if len(queues) != 1 {
			return nil, fmt.Errorf("asynqadapter: once mode requires exactly one physical queue, got %d", len(queues))
		}
		for queue, concurrency := range queues {
			if concurrency != 1 {
				return nil, fmt.Errorf("asynqadapter: once mode requires MaxWorkers=1 for queue %q", queue)
			}
		}
		if cfg.SchedulePeriodicJobs {
			return nil, errors.New("asynqadapter: once mode requires periodic scheduling to be disabled")
		}
	}
	adapter := &Adapter{
		redis: redisClient, client: asynq.NewClientFromRedisClient(redisClient),
		inspector: asynq.NewInspectorFromRedisClient(redisClient), now: time.Now,
		registry: registry, queueFor: queueFor, queues: queues, once: cfg.Once,
	}
	if cfg.SchedulePeriodicJobs {
		adapter.periodic = append(adapter.periodic, cfg.Spec.PeriodicJobs...)
	}
	return adapter, nil
}

func physicalQueues(spec jobq.WorkerSpec) (map[string]int, error) {
	queues := make(map[string]int)
	for _, queue := range spec.Queues {
		if len(queue.Physical) == 0 {
			return nil, fmt.Errorf("asynqadapter: logical queue %q has no physical queue to subscribe", queue.Logical)
		}
		for _, physical := range queue.Physical {
			if strings.TrimSpace(physical) == "" {
				return nil, fmt.Errorf("asynqadapter: logical queue %q has an empty physical queue name", queue.Logical)
			}
			if _, exists := queues[physical]; exists {
				return nil, fmt.Errorf("asynqadapter: duplicate physical queue %q", physical)
			}
			queues[physical] = queue.MaxWorkers
		}
	}
	return queues, nil
}

// Enqueue は args を JSON に変換して物理キューに投入する。優先度ヒントは使わない。
func (adapter *Adapter) Enqueue(ctx context.Context, args jobq.Args, _ ...jobq.EnqueueOpt) (jobq.Result, error) {
	if args == nil {
		return jobq.Result{}, errors.New("asynqadapter: cannot enqueue nil args")
	}
	spec := args.Spec()
	if spec.Timeout == nil || spec.Timeout(args) <= 0 {
		return jobq.Result{}, fmt.Errorf("asynqadapter: %q args require a positive timeout", args.Kind())
	}
	if spec.MaxAttempts < 0 {
		return jobq.Result{}, fmt.Errorf("asynqadapter: %q args have a negative MaxAttempts", args.Kind())
	}
	queue, err := adapter.queueFor(args)
	if err != nil {
		return jobq.Result{}, fmt.Errorf("resolving asynq queue for %q: %w", args.Kind(), err)
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return jobq.Result{}, fmt.Errorf("encoding asynq args for %q: %w", args.Kind(), err)
	}
	// asynq の締切が先に来ると、Work の終了を待たずに次のタスクが開始され、
	// MaxWorkers を超えうる。Work の締切と結果の確定はラッパーが扱い、
	// Work が戻るまでスロットを保持できるよう asynq 自身の締切は十分先に置く。
	// v0.26.0 は Timeout(0) かつ Deadline 未指定のとき既定の 30 分を使う
	// （TestEnqueuePreservesLongWorkTimeout）。
	options := []asynq.Option{
		asynq.Queue(queue), asynq.MaxRetry(math.MaxInt32), asynq.Timeout(time.Duration(math.MaxInt64)),
	}
	if spec.Coalesce {
		options = append(options, asynq.Unique(coalesceTTL))
	}
	_, err = adapter.client.EnqueueContext(ctx, asynq.NewTask(args.Kind(), payload), options...)
	if errors.Is(err, asynq.ErrDuplicateTask) {
		return jobq.Result{Coalesced: true}, nil
	}
	if err != nil {
		return jobq.Result{}, fmt.Errorf("enqueuing asynq job %q: %w", args.Kind(), err)
	}
	return jobq.Result{}, nil
}
