package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
)

// storeRoute は、置き場で修飾したキューのジョブを、その置き場に束ねたワーカーへ振り分ける。
//
// 置き場ごとのワーカーは MediaDir・MediaStoreName・置き場に束ねた store を持つ 1 つの
// 通常のワーカーで、置き場を取り違えて書く余地を持たない。この型は、ジョブが指す置き場を
// このプロセスがマウントしているかを実行時に照合する二次防御でもある（site の verifySite と同じ形）。
// 一次防御はキュー選択そのもので、置き場 A のキューを購読しない worker は置き場 B のジョブを
// 取らない。照合に落ちたジョブは失敗にし、メトリクスを出す。
type storeRoute[T jobq.Args] struct {
	kind    string
	workers map[string]jobq.Worker[T]
	// pick はジョブを実行する置き場を返す。投入側が選んだ置き場（QueueStore）か、
	// ingest のように実行時に決める置き場である。
	pick func(ctx context.Context, args T) (string, error)
}

// Work は args の置き場に束ねたワーカーを呼ぶ。マウントしていない置き場なら失敗にする。
func (r *storeRoute[T]) Work(ctx context.Context, args T) error {
	name, err := r.pick(ctx, args)
	if err != nil {
		return fmt.Errorf("%s: choosing the store: %w", r.kind, err)
	}
	w, ok := r.workers[name]
	if !ok {
		metrics.StoreRouteMismatches.WithLabelValues(r.kind, name).Inc()
		return fmt.Errorf("%s: store %q is not among this worker's mounted stores %v, refusing before touching the filesystem",
			r.kind, name, r.mountedNames())
	}
	return w.Work(ctx, args)
}

func (r *storeRoute[T]) mountedNames() []string {
	names := make([]string, 0, len(r.workers))
	for name := range r.workers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// queueStoreArgs は、投入側が実行する置き場を決める Args（jobs の QueueStore）。
type queueStoreArgs interface {
	jobq.Args
	QueueStore() string
}

// newStoreRoute は置き場ごとにワーカーを作り、Args の QueueStore で振り分ける worker を返す。
func newStoreRoute[T queueStoreArgs](kind string, stores map[string]*Deps, build func(*Deps) jobq.Worker[T]) *storeRoute[T] {
	workers := make(map[string]jobq.Worker[T], len(stores))
	for name, deps := range stores {
		workers[name] = build(deps)
	}
	return &storeRoute[T]{
		kind:    kind,
		workers: workers,
		pick:    func(_ context.Context, args T) (string, error) { return args.QueueStore(), nil },
	}
}

// chooseIngestStore は規則で ingest の置き場を選ぶ。規則の符号化と評価の失敗はそのまま返す。
func chooseIngestStore(ctx context.Context, deps *Deps, site string) (string, error) {
	rules, err := deps.StorageTransferPolicy.OriginalRulesJSON()
	if err != nil {
		return "", fmt.Errorf("encoding the original store rules: %w", err)
	}
	name, err := deps.IngestStores.DesiredOriginalStoreAtIngest(ctx, rules, deps.StorageTransferPolicy.Derived, site)
	if err != nil {
		return "", fmt.Errorf("evaluating the original store rules: %w", err)
	}
	return name, nil
}

// newIngestRoute は ingest を、マウントした置き場のうち規則に合う置き場のワーカーへ振り分ける。
//
// 書き込み先は、置き場の規則を年齢 0 で評価して一致した置き場のうち、このプロセスがマウントして
// いるものである。無ければ、マウントしている置き場のうち `--stores` の順で先頭の置き場に書く。
// desired との差は転送が埋めるので、置き場を理由に ingest が止まる経路は作らない。
// 規則の評価は SQL の関数が行い、ここには規則の写しを置かない。
func newIngestRoute(deps *Deps, build func(*Deps) jobq.Worker[jobs.IngestJobArgs]) *storeRoute[jobs.IngestJobArgs] {
	stores := deps.perStore()
	workers := make(map[string]jobq.Worker[jobs.IngestJobArgs], len(stores))
	for name, d := range stores {
		workers[name] = build(d)
	}
	mounted := deps.MountedStores
	return &storeRoute[jobs.IngestJobArgs]{
		kind:    "ingest",
		workers: workers,
		pick: func(ctx context.Context, args jobs.IngestJobArgs) (string, error) {
			if len(mounted) == 0 {
				return "", fmt.Errorf("no store is mounted")
			}
			if deps.IngestStores != nil && len(deps.StorageTransferPolicy.Originals) > 0 {
				// 置き場の選択の失敗で ingest を止めない。転送が後で埋めるので、先頭のマウント済みの置き場に書く。
				name, err := chooseIngestStore(ctx, deps, args.Site)
				if err != nil {
					slog.Error("ingest: choosing the original store failed; writing to the first mounted store",
						"site", args.Site, "store", mounted[0], "error", err)
					return mounted[0], nil
				}
				if _, ok := workers[name]; ok {
					return name, nil
				}
			}
			return mounted[0], nil
		},
	}
}
