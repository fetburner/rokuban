package jobq

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// QueueSpec は 1 つの論理キューの購読の仕様。
type QueueSpec struct {
	// Logical は adapter に依らない論理名。Args の Spec().Queue と同じ名前である。
	Logical string
	// Physical は購読する物理キュー名。site で分けないキューは [Logical]。
	// site で分けるキューを購読する site が 0 件のときは空になり、
	// adapter はそのキューが選ばれたら起動を拒む。
	Physical []string
	// MaxWorkers は 1 つの物理キューあたりの同時実行の上限。1 以上。
	MaxWorkers int
}

// PeriodicJobSpec は定期的に投入するジョブの仕様。
//
// 定期投入を担う runner は、Start の直後に 1 回、その後は Interval ごとに Args を
// 投入する。起動直後の 1 回は仕様の一部で、設定では外せない（adapter のテストが固定する）。
type PeriodicJobSpec struct {
	// Interval は投入の間隔。正の値。
	Interval time.Duration
	// Args は投入する引数。site で分けるジョブは Site を埋めて、site ごとに 1 件並べる。
	Args Args
}

// WorkerSpec は worker が何を購読し、何を定期投入するかの仕様。
// 値の決め方（製品の方針）は組み立てる側が持ち、adapter はこの仕様を実現するだけである。
type WorkerSpec struct {
	Queues       []QueueSpec
	PeriodicJobs []PeriodicJobSpec
}

// Validate は仕様がエンジンに依らず成り立つことを検査する。
// Logical の空・重複、MaxWorkers < 1、Interval <= 0、Args == nil、
// 定期ジョブのキューが Queues に無いものを拒む。
func (spec WorkerSpec) Validate() error {
	logical := make(map[string]bool, len(spec.Queues))
	for _, queue := range spec.Queues {
		if queue.Logical == "" {
			return errors.New("jobq: worker spec has a queue with an empty logical name")
		}
		if logical[queue.Logical] {
			return fmt.Errorf("jobq: worker spec has queue %q more than once", queue.Logical)
		}
		logical[queue.Logical] = true
		if queue.MaxWorkers < 1 {
			return fmt.Errorf("jobq: worker spec queue %q: MaxWorkers must be >= 1, got %d", queue.Logical, queue.MaxWorkers)
		}
	}
	for i, periodic := range spec.PeriodicJobs {
		if periodic.Args == nil {
			return fmt.Errorf("jobq: worker spec periodic job %d has nil args", i)
		}
		kind := periodic.Args.Kind()
		if periodic.Interval <= 0 {
			return fmt.Errorf("jobq: worker spec periodic job %q: interval must be positive, got %s", kind, periodic.Interval)
		}
		if queue := periodic.Args.Spec().Queue; !logical[queue] {
			return fmt.Errorf("jobq: worker spec periodic job %q uses queue %q which is not in Queues", kind, queue)
		}
	}
	return nil
}

// Select は論理名 names のキューだけを残した仕様を返す。names が空なら全キューを残す。
// 同じ名前の重複は 1 つに畳む（config の worker.queues は重複を畳まずに届く。
// 畳まないと Validate が拒む Logical の重複を Select が作る）。
// 未知の名前は、有効な名前の一覧を添えて拒む。定期ジョブは絞り込まない
// （誰が投入するかは起動形態が決める）。Queues は複製して返すので、呼び出し元は書き換えてよい。
func (spec WorkerSpec) Select(names []string) (WorkerSpec, error) {
	selected := spec
	selected.Queues = slices.Clone(spec.Queues)
	if len(names) == 0 {
		return selected, nil
	}
	selected.Queues = make([]QueueSpec, 0, len(names))
	for _, name := range names {
		if slices.ContainsFunc(selected.Queues, func(queue QueueSpec) bool { return queue.Logical == name }) {
			continue
		}
		i := slices.IndexFunc(spec.Queues, func(queue QueueSpec) bool { return queue.Logical == name })
		if i < 0 {
			valid := make([]string, 0, len(spec.Queues))
			for _, queue := range spec.Queues {
				valid = append(valid, queue.Logical)
			}
			slices.Sort(valid)
			return WorkerSpec{}, fmt.Errorf("worker.queues: unknown queue %q (valid: %s)", name, strings.Join(valid, ", "))
		}
		selected.Queues = append(selected.Queues, spec.Queues[i])
	}
	return selected, nil
}

// CheckKinds は定期ジョブの kind がすべて registry で解決できることを検査する。
// Args を JSON で往復させて registry.Decode に通すので、worker 未登録の kind は
// 起動時に拒まれる（TestWorkerSpecCheckKinds）。Decode が見る往復後の kind の一致と
// Spec の妥当性は、Decode の契約に委ねていて、この関数では個別に検証していない。
func (spec WorkerSpec) CheckKinds(registry *Registry) error {
	for _, periodic := range spec.PeriodicJobs {
		if periodic.Args == nil {
			return errors.New("jobq: periodic job has nil args")
		}
		kind := periodic.Args.Kind()
		payload, err := json.Marshal(periodic.Args)
		if err != nil {
			return fmt.Errorf("jobq: encoding periodic job %q args: %w", kind, err)
		}
		if _, err := registry.Decode(kind, payload); err != nil {
			return fmt.Errorf("jobq: periodic job %q is not resolvable by the registry: %w", kind, err)
		}
	}
	return nil
}

// CheckQueueFor は、定期ジョブの投入先（queueFor の結果）が、そのキューの購読する
// 物理名のどれかであることを検査する。投入側の命名規則と購読側の物理名の展開が
// 食い違うと、投入したジョブを誰も引かない「黙って動かない」になる。
//
// 検査できるのは、起動時に Args の実体が分かる定期ジョブだけである。
// 登録済み worker の Args（site が実行時に決まる）の投入先は、この関数では見ない。
// 購読する物理名が空のキュー（site が 0 件）は、投入先と比べるものが無いので通す。
func (spec WorkerSpec) CheckQueueFor(queueFor func(Args) (string, error)) error {
	for _, periodic := range spec.PeriodicJobs {
		if periodic.Args == nil {
			return errors.New("jobq: periodic job has nil args")
		}
		kind := periodic.Args.Kind()
		target, err := queueFor(periodic.Args)
		if err != nil {
			return fmt.Errorf("jobq: resolving the queue of periodic job %q: %w", kind, err)
		}
		logical := periodic.Args.Spec().Queue
		i := slices.IndexFunc(spec.Queues, func(queue QueueSpec) bool { return queue.Logical == logical })
		if i < 0 || len(spec.Queues[i].Physical) == 0 {
			continue
		}
		if !slices.Contains(spec.Queues[i].Physical, target) {
			return fmt.Errorf("jobq: periodic job %q is inserted into queue %q, "+
				"but the worker subscribes to %v for logical queue %q: nobody would pull it",
				kind, target, spec.Queues[i].Physical, logical)
		}
	}
	return nil
}
