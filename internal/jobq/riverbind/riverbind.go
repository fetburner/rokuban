// Package riverbind は、worker.NewWorkers が登録する kind に River の型付き builder を預ける一覧を持つ。
//
// 一覧を worker パッケージに置かないのは、River の公開 AddWorker[T] が型引数を要求し、
// worker が riveradapter と River と pgx に依存することを避けるためである。この一覧は
// riveradapter を import する唯一の製品パッケージで、cmd/rokuban（River 構成の根）と
// riveradapter の外部テストだけが呼ぶ。cmd/rokuban は main なのでテストから import できず、
// 一覧を別パッケージに置く必要がある。
//
// 一覧は worker.NewWorkers の登録と同じ集合でなければならない。漏れは riveradapter.NewAdapter が
// 「kinds registered without an adapter "river" worker」で拒む。余分な kind は Bind が拒む。
package riverbind

import (
	"fmt"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/tsscan"
)

// bindings は worker.NewWorkers が登録する kind と同じ順に並べる。
var bindings = []func(*jobq.Registry) error{
	riveradapter.Bind[jobs.IngestJobArgs],
	riveradapter.Bind[jobs.EncodeReconcileArgs],
	riveradapter.Bind[jobs.EpgSyncArgs],
	riveradapter.Bind[jobs.TunerSyncArgs],
	riveradapter.Bind[jobs.RulerPassArgs],
	riveradapter.Bind[jobs.LabelRuleReconcileArgs],
	bindIfRegistered[jobs.NotifyPruneArgs],
	riveradapter.Bind[jobs.ReconcilePassArgs],
	riveradapter.Bind[jobs.RecordSweepArgs],
	riveradapter.Bind[jobs.ThumbnailReconcileArgs],
	riveradapter.Bind[jobs.CMDetectReconcileArgs],
	riveradapter.Bind[jobs.StorageTransferReconcileArgs],
	riveradapter.Bind[jobs.StorageTransferArgs],
	riveradapter.Bind[tsscan.ReconcileArgs],
	riveradapter.Bind[jobs.EncodeJobArgs],
	riveradapter.Bind[jobs.CatalogExportArgs],
	riveradapter.Bind[jobs.ThumbnailJobArgs],
	riveradapter.Bind[jobs.SeekTilesJobArgs],
	riveradapter.Bind[jobs.CMDetectJobArgs],
	riveradapter.Bind[jobs.CMLogoCandidateJobArgs],
	riveradapter.Bind[jobs.DeleteReconcileArgs],
	riveradapter.Bind[jobs.StorageSyncArgs],
	riveradapter.Bind[tsscan.ScanArgs],
}

// BindAll は registry の各 kind に River の worker builder を預ける。registry は
// worker.NewWorkers が返したものでなければならない（jobq.Register が先に済んでいること）。
// 呼んだ後に riveradapter.NewAdapter で River の client を組む。
func BindAll(registry *jobq.Registry) error {
	for _, bind := range bindings {
		if err := bind(registry); err != nil {
			return fmt.Errorf("riverbind: %w", err)
		}
	}
	return nil
}

// bindIfRegistered は kind T が registry にあるときだけ River の builder を預ける。
// notify_prune は NotificationPruner を持つ構成でだけ worker.NewWorkers が登録する。
// PostgreSQL 構成（River）ではその store が無いので、kind が無いのが正しい。
// 一覧から黙って外すのではなく、registry の有無を正として決める。
// 登録の漏れ（kind があるのに builder が無い）は riveradapter.NewAdapter が拒むので、この飛ばしでは隠れない。
func bindIfRegistered[T jobq.Args](registry *jobq.Registry) error {
	var zero T
	kind := zero.Kind()
	for _, args := range registry.ZeroArgs() {
		if args.Kind() == kind {
			return riveradapter.Bind[T](registry)
		}
	}
	return nil
}
