package riveradapter

import (
	"context"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
)

// このパッケージのテストが使う仕様の fixture。製品の方針（どのジョブを何分で、
// 何本並列で）はここには無く cmd/rokuban の workerspec_test.go が主張する。
// adapter のテストは「渡した仕様が N サイト × M 種なら N×M 本」の形で、
// 自分が渡した仕様の数に対して主張する。

// testQueueFor は製品の命名規則（jobs.PhysicalQueueFor）を注入する。
var testQueueFor QueueFunc = jobs.PhysicalQueueFor

// logicalQueueFor は Args の論理キュー名をそのまま物理名にする（契約テスト用）。
func logicalQueueFor(args jobq.Args) (string, error) { return args.Spec().Queue, nil }

// plainQueue は site で分けないキュー 1 本の仕様。
func plainQueue(name string, maxWorkers int) jobq.QueueSpec {
	return jobq.QueueSpec{Logical: name, Physical: []string{name}, MaxWorkers: maxWorkers}
}

// siteQueue は site 単位のキューを sites へ展開した仕様。
func siteQueue(logical string, maxWorkers int, sites ...string) jobq.QueueSpec {
	physical := make([]string, 0, len(sites))
	for _, site := range sites {
		physical = append(physical, jobs.PhysicalQueueName(logical, site))
	}
	return jobq.QueueSpec{Logical: logical, Physical: physical, MaxWorkers: maxWorkers}
}

// siteSpec は site 単位の 4 キューと site 非依存のキューを並べた仕様（定期ジョブは空）。
func siteSpec(ingestWorkers int, sites ...string) jobq.WorkerSpec {
	return jobq.WorkerSpec{Queues: []jobq.QueueSpec{
		plainQueue(jobs.DefaultQueue, 100),
		siteQueue(jobs.IngestQueue, ingestWorkers, sites...),
		siteQueue(jobs.EpgQueue, 1, sites...),
		plainQueue(jobs.RulerQueue, 1),
		siteQueue(jobs.ReconcilerQueue, 1, sites...),
		siteQueue(jobs.RecordSweepQueue, 1, sites...),
		plainQueue(jobs.EncodeQueue, 1),
		plainQueue(jobs.ThumbnailQueue, 1),
		plainQueue(jobs.CleanupQueue, 2),
		plainQueue(jobs.TSScanQueue, 1),
	}}
}

// nopWorker は何もしない worker。定期ジョブの kind を registry に解決させるために使う。
type nopWorker[T jobq.Args] struct{}

func (nopWorker[T]) Work(context.Context, T) error { return nil }

// oneHour は定期ジョブの周期のうち「このテストの間は 2 回目が来ない」長さ。
const oneHour = time.Hour
