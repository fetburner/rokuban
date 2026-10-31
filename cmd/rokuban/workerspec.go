package main

import (
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/tsscan"
)

// newWorkerSpec は worker が購読するキューと定期投入するジョブの仕様を組み立てる。
//
// ここに書く周期と並列数は製品の仕様である。設定キーを持つ 4 つ
// （ingest.concurrency / encode.concurrency / encode.thumbnail_concurrency /
// epg.sync_interval）だけが config から来て、残りは上書きできないのでリテラルで書く。
// 設定キーの無い値を config の既定値にすると「変えられる値」に見えてしまう。
// 並べ替えや絞り込みは jobq.WorkerSpec と起動形態（--queues / --sites /
// worker.periodic_jobs）が担う。
//
// この関数が cmd にあるのは、tsscan が internal/jobs を import しているので、
// jobs 側に置くと tsscan.ReconcileArgs を並べられないからである（構成の根で組む）。
//
// stores は --stores で解決したマウント済みの FS 置き場。置き場で修飾する物理キューは
// この集合で展開する（config の名前ではなく、このプロセスが実際に持つ置き場で決める）。
func newWorkerSpec(cfg *config.Config, bound []config.MirakcSite, stores []string) jobq.WorkerSpec {
	sites := registryNames(bound)
	return jobq.WorkerSpec{
		Queues:       workerQueueSpecs(cfg, sites, stores),
		PeriodicJobs: workerPeriodicJobSpecs(cfg, sites),
	}
}

// workerQueueSpecs は論理キューごとの並列数と、site ごとに展開した物理キューを返す。
// 一覧は jobs.AllQueueNames と一致させる（TestNewWorkerSpec_QueuesMatchAllQueueNames）。
func workerQueueSpecs(cfg *config.Config, sites, stores []string) []jobq.QueueSpec {
	queue := func(logical string, maxWorkers int) jobq.QueueSpec {
		return jobq.QueueSpec{Logical: logical, Physical: physicalQueueNames(logical, sites), MaxWorkers: maxWorkers}
	}
	storeQueue := func(logical string, maxWorkers int) jobq.QueueSpec {
		physical := make([]string, 0, len(stores))
		for _, name := range stores {
			physical = append(physical, jobs.PhysicalStoreQueueName(logical, name))
		}
		return jobq.QueueSpec{Logical: logical, Physical: physical, MaxWorkers: maxWorkers}
	}
	return []jobq.QueueSpec{
		queue(jobs.DefaultQueue, 100),
		queue(jobs.IngestQueue, cfg.Ingest.Concurrency),
		// 全量同期が重ならないよう 1 本に絞る。tuner_sync も同じキューを使う
		// （どちらも使い捨てプロジェクションの全量同期。TunerSyncArgs.Spec 参照）。
		queue(jobs.EpgQueue, 1),
		// サイト単位の排他は UniqueOpts が担うので、こちらも複数ワーカーは要らない。
		queue(jobs.RulerQueue, 1),
		// reconciler も ruler と同じ理由（UniqueOpts がサイト単位の排他を担う）で 1 本に絞る。
		queue(jobs.ReconcilerQueue, 1),
		// record_sweep も同じ理由（UniqueOpts がサイト単位の排他を担う）で 1 本に絞る。
		queue(jobs.RecordSweepQueue, 1),
		// encode / thumbnail は CPU 拘束なので ingest とは独立に絞る。
		queue(jobs.EncodeQueue, cfg.Encode.Concurrency),
		queue(jobs.ThumbnailQueue, cfg.Encode.ThumbnailConcurrency),
		queue(jobs.CMDetectQueue, 1),
		// delete_reconcile と catalog_export はそれぞれ UniqueOpts{ByArgs} で自分自身の
		// 重複実行を防ぐので、「両方が同時に 1 本ずつ走れる」ぶんだけ確保すれば足り、
		// それ以上に増やす理由がない（label_rule_reconcile も同じキュー）。
		queue(jobs.CleanupQueue, 2),
		// UniqueOpts{ByArgs} が重複実行を防ぐので 1 本で足りる。
		queue(jobs.StorageQueue, 1),
		// 1 つの置き場のファイルを読む・書く job は、その置き場に束縛して処理する。
		storeQueue(jobs.TransferQueue, 1),
		// ts_scan は原本全体を読み直すため、同時 I/O を 1 件に抑える。
		queue(jobs.TSScanQueue, 1),
	}
}

// physicalQueueNames は論理キューの物理名を、束縛 site ごとに展開して返す。
// site で分けるキューは site の数だけ、それ以外は論理名 1 つ。0 サイトで site を
// 分けるキューは空になる（起動時に validateSiteBinding が、0 サイトの worker が
// これらのキューを要求しないことを強制する）。
// site で分けるかの判定は jobs.RequiresSiteBinding と同じ集合を通す。
func physicalQueueNames(logical string, sites []string) []string {
	if !jobs.RequiresSiteBinding([]string{logical}) {
		return []string{logical}
	}
	physical := make([]string, 0, len(sites))
	for _, site := range sites {
		physical = append(physical, jobs.PhysicalQueueName(logical, site))
	}
	return physical
}

// workerPeriodicJobSpecs は定期投入するジョブを返す。
//
// 起動直後の 1 回は jobq.PeriodicJobSpec の契約で、ここでは外せない。
// 日次の catalog_export は起動直後の 1 回で初回を確保する意味があるが、
// 他のジョブについて、それぞれの間隔での「起動直後の 1 回」が同程度に有用かは
// 個別に検証していない。
func workerPeriodicJobSpecs(cfg *config.Config, sites []string) []jobq.PeriodicJobSpec {
	var specs []jobq.PeriodicJobSpec
	// site 単位の 5 種は束縛 site ごとに 1 本ずつ。0 サイト（中央プロセス）は
	// site 単位の定期パスを持つ理由がない。ruler は mirakc に触れないが、
	// args.Site が DB 行の絞り込みなので site ごとに 1 本で正しい
	// （internal/jobs/queue.go の siteBoundQueueNames のコメント参照）。
	for _, site := range sites {
		specs = append(specs,
			jobq.PeriodicJobSpec{Interval: cfg.Epg.SyncInterval, Args: jobs.EpgSyncArgs{Site: site}},
			// チューナー構成の変更には mirakc の再起動が要るので更新頻度は低くてよく、
			// EPG 全量同期と同じ 10 分で足りる。専用の設定キーは設けていない
			// （運用者が触る理由がない）。
			jobq.PeriodicJobSpec{Interval: 10 * time.Minute, Args: jobs.TunerSyncArgs{Site: site}},
			// docs/recording.md §3.1 の起動契機の表の「定期（既定 10 分）」と揃える。
			jobq.PeriodicJobSpec{Interval: 10 * time.Minute, Args: jobs.RulerPassArgs{Site: site}},
			// 従来の reconciler の自前タイマーが使っていた 30 秒を引き継ぐ
			// （docs/recording.md §3.2「定期（既定 30 秒）」）。
			jobq.PeriodicJobSpec{Interval: 30 * time.Second, Args: jobs.ReconcilePassArgs{Site: site}},
			// 従来の watcher の自前タイマーが使っていた 5 分を引き継ぐ（docs/recording.md §3.3）。
			jobq.PeriodicJobSpec{Interval: 5 * time.Minute, Args: jobs.RecordSweepArgs{Site: site}},
		)
	}
	specs = append(specs,
		// catalog エクスポートは日次相当（docs/storage.md §8「日次 + 世代保持」）。
		jobq.PeriodicJobSpec{Interval: 24 * time.Hour, Args: jobs.CatalogExportArgs{}},
		jobq.PeriodicJobSpec{Interval: config.DeleteReconcileInterval, Args: jobs.DeleteReconcileArgs{}},
		// 必須の窓閉じである。分類ルールの編集の前後でコミットした録画は、旧ルールで
		// 評価された当たりを持ったままになる（トリガーは自分の行しか見ない）。
		// encode 系の定期パスと同じ 15 分に揃える。
		jobq.PeriodicJobSpec{Interval: 15 * time.Minute, Args: jobs.LabelRuleReconcileArgs{}},
		// このパスが新しく投入するのは、ヒント経路（ingest 完了時のベストエフォート投入と
		// POST /api/recordings/{id}/encode-profiles のヒントジョブ）が落ちたぶんだけである。
		// 落としたヒントの回復が数十分遅れても失うものは無い一方、何時間も放置は
		// したくないので、同じく「普段は新しい仕事を作らないバックストップ」である
		// delete_reconcile と同じ 15 分に揃える。
		jobq.PeriodicJobSpec{Interval: 15 * time.Minute, Args: jobs.EncodeReconcileArgs{}},
		// ingest 完了時のヒント投入を補うバックストップとして encode reconcile と揃える。
		jobq.PeriodicJobSpec{Interval: 15 * time.Minute, Args: jobs.ThumbnailReconcileArgs{}},
		jobq.PeriodicJobSpec{Interval: 15 * time.Minute, Args: tsscan.ReconcileArgs{}},
	)
	if cfg.CMDetect.Enabled {
		specs = append(specs, jobq.PeriodicJobSpec{Interval: 15 * time.Minute, Args: jobs.CMDetectReconcileArgs{}})
	}
	// ストレージ観測。tuner_sync（10 分）より短くしているが、この値そのものに測定的根拠は
	// ない（未検証。record_sweep の 5 分に合わせただけの初期値）。専用の設定キーは設けて
	// いない --- 運用者が調整する理由が今のところ無い。
	specs = append(specs, jobq.PeriodicJobSpec{Interval: 5 * time.Minute, Args: jobs.StorageSyncArgs{}})
	specs = append(specs, jobq.PeriodicJobSpec{Interval: config.DeleteReconcileInterval, Args: jobs.StorageTransferReconcileArgs{}})
	return specs
}
