package worker

import (
	"time"

	"github.com/fetburner/rokuban/internal/cmdetect"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/encode"
	"github.com/fetburner/rokuban/internal/ingest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/reconciler"
	"github.com/fetburner/rokuban/internal/ruler"
	"github.com/fetburner/rokuban/internal/storagemaint"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/thumbnail"
	"github.com/fetburner/rokuban/internal/tsscan"
	"github.com/fetburner/rokuban/internal/watcher"
	"github.com/fetburner/rokuban/internal/webhook"
)

// Deps は各ワーカーに注入する依存。
type Deps struct {
	// Enqueuer は後続ジョブの投入に使う。worker と queue adapter の構築順の循環は
	// jobq.Deferred を渡して切る。
	Enqueuer jobq.Enqueuer

	// EPGSync は EPG 同期の永続化契約。
	EPGSync store.EPGSync
	// RulerInputBumper は EPG 同期の完了時に ruler 入力の版を進める。
	RulerInputBumper store.RulerInputBumper

	// ProgramSearch は番組検索・ルール評価の永続化契約。
	ProgramSearch store.ProgramSearch
	// RulerPass は ruler が予約の導出と適用に使う永続化契約。
	RulerPass store.RulerPass
	// LoopMarkers はループの成功時刻（ruler / record_sweep / 全件パス / 削除 reconcile / EPG 同期 / チューナー同期 / 物理転送）を記録し、監視が読む契約。
	LoopMarkers store.LoopMarkers
	// Breakers は worker がサーキットブレーカーを発動・観測する契約。
	Breakers store.Breakers
	// NeverScheduled は、番組終了後に schedule が観測されなかった事実を記録・削除する契約。
	NeverScheduled store.NeverScheduledEvents
	// ScheduleSync は reconciler が schedule の観測と予約との差分を記録し、読み取る契約。
	ScheduleSync store.ScheduleSync
	// RecordObservations は mirakc の record 観測を永続化する契約。
	RecordObservations store.RecordObservations

	// MirakcClients は `--sites` で束縛された site 名から mirakc クライアントへの
	// map（issue #532。1 プロセスが N site を束縛できるようになったため、
	// 単一の MirakcClient / Site string の対を置き換えた）。mirakc/FS に触る
	// ワーカー（Ingest / EpgSync / TunerSync / ReconcilePass / RecordSweep）は
	// これと自分が処理するジョブの args.Site を jobs.VerifySite で照合してから
	// 実際に使うクライアントを取り出す（issue #139）。
	//
	// 空 map（0 site 束縛の中央プロセス）でも構築時にはエラーにしない ---
	// site 単位のキューを実際に購読していれば、届いたジョブは jobs.VerifySite が
	// 必ず拒む（起動時には validateSiteBinding が
	// この組み合わせ自体を防ぐ）。
	MirakcClients map[string]*mirakc.Client
	// UnscopedCMDetectionStore は CM 検出 worker・reconcile の永続化契約。置き場ごとの worker は
	// StoreDeps の値を使い、この値は置き場に束ねない reconcile が使う。StoreDeps.CMDetectionStore と
	// build 内で取り違えないよう、置き場に束ねないことを名前で示す。
	UnscopedCMDetectionStore store.CMDetection
	// TunerStore はチューナー同期の永続化契約。
	TunerStore store.TunerSync
	// CapacityStore はチューナー worker の容量判定読み取り契約。
	CapacityStore store.Capacity
	// LabelRuleReevaluation は分類ルールの全件再評価を永続化する。
	LabelRuleReevaluation store.LabelRuleReevaluation
	// UnscopedMediaReconcile は置き場に束ねない reconcile の永続化契約。
	// thumbnail / encode の周期 reconcile が使う値は意図的に未束縛である。StoreDeps.MediaReconcile と
	// build 内で取り違えないよう、置き場に束ねないことを名前で示す。
	UnscopedMediaReconcile store.MediaReconcile
	// DerivedObjects は派生物の置き場に束ねた object store で、全置き場のワーカーに同じ値を渡す。
	// StoreDeps.DerivedMediaPublish と組み合わせて使う。
	DerivedObjects         objstore.Store
	DerivedObjectStoreName string
	StorageObjects         map[string]objstore.Store
	// UnscopedStorageTransfer は置き場に束ねない transfer reconcile の永続化契約。
	// StoreDeps.StorageTransfer と build 内で取り違えないよう、置き場に束ねないことを名前で示す。
	UnscopedStorageTransfer store.StorageTransfer
	StorageTransferPolicy   config.StorageConfig
	StorageRoots            map[string]string
	// UnscopedTSScan は置き場に束ねない TS scan reconcile の永続化契約。
	// StoreDeps.TSScan と build 内で取り違えないよう、置き場に束ねないことを名前で示す。
	UnscopedTSScan store.TSScan
	// PerStore は、マウントした FS 置き場ごとに束ねた StoreDeps（置き場の名前から）。
	// root・名前と置き場に束ねた store（MediaReconcile など）を置き場ごとに持つ。
	// 空なら置き場で修飾したワーカーは登録されず、その kind のジョブは置き場の不一致で失敗する。
	// このトップレベルの store は置き場に束ねず、DB だけを触る reconcile が使う。
	PerStore map[string]*StoreDeps
	// MountedStores はマウントした FS 置き場の名前を `--stores` の順で持つ。ingest が規則で
	// 選べなかったときに書く先頭の置き場を決める（名前順ではない。内容は PerStore と同じ集合）。
	MountedStores []string
	// OriginalLocator は reconcile が読み取りジョブを積む置き場の選択に使う。置き場に束ねない。
	OriginalLocator store.OriginalLocator
	// IngestStores は ingest が書き込む置き場を規則から選ぶ契約。
	IngestStores    store.IngestStoreChooser
	TransferOverlap time.Duration
	// IngestStorageProbed は起動時に ingest.ProbeIngestStorage が成功したかを示す。
	// false なら IngestWorker が最初の ingest で、目印を照合した後に probe する。
	IngestStorageProbed bool
	ScratchDir          string

	// Encode は構造化エンコードプロファイルと ffmpeg パス（issue #64 / #65）。
	// worker ロール起動時に ValidateTools 済み（不変条件 4）。
	Encode              config.EncodeConfig
	CMDetect            config.CMDetectConfig
	RecordingHLSEnabled bool

	// EpgRetentionGrace は放送済み番組を刈り取るまでの猶予
	// （config.epg.retention_grace。config.defaults() が既定値 24 時間を埋める）。
	EpgRetentionGrace time.Duration

	// RulerRetentionGrace は ruler が reservations / program_intents を GC するまでの
	// 猶予。0 なら ruler 側の既定値を使う。epg.retention_grace と同じ値を流用する
	// 運用を想定している（docs/recording.md §3.2「番組終了後の GC」）。
	RulerRetentionGrace time.Duration

	// RulerMaxDeletesPerPass は大量削除サーキットブレーカーの閾値
	// （ruler.max_deletes_per_pass）。0 なら ruler 側の既定値を使う
	// （docs/recording.md §3.2「大量削除サーキットブレーカー」）。
	RulerMaxDeletesPerPass int

	// RulerRetractGrace は放送開始直前にルールから外れた予約を削除しない猶予
	// （ruler.retract_grace, issue #428）。**0 は無効**（他の 2 つと違い ruler 側の
	// 既定値へのフォールバックはない --- internal/config.RulerConfig.RetractGrace が
	// 既定 1h を埋めるので、ここに来る時点で「未設定」は既に解決済み。
	// internal/ruler.Config.RetractGrace のコメント参照）。
	RulerRetractGrace time.Duration

	// ReconcileStartDelayGrace は開始遅延検出器の猶予
	// （reconciler.start_delay_grace）。0 なら reconciler 側の既定値を使う
	// （docs/recording.md §3.3「開始遅延検出器」）。
	ReconcileStartDelayGrace time.Duration

	// IngestStallTimeout は ingest の無進捗検知タイムアウト
	// （ingest.stall_timeout。config.defaults() が既定値 30 秒を埋める）。
	// bounded slice / snooze の区切りとは別に、接続断を判定する
	// （docs/recording/ingest-retry.md §5.3「層 1」）。
	IngestStallTimeout time.Duration

	// RecoveryGrace はジョブエンジン adapter が申告する回復猶予（jobq.Spec.Timeout の契約）。
	// CM ロゴ候補の孤児判定が締切に足す。構成の根が adapter の申告値を渡す。
	RecoveryGrace time.Duration

	// Webhook は録画ライフサイクル通知用クライアント（M3-11）。nil 可。
	// recording.finished/failed は record_sweep 経由の processRecord から、
	// encode.finished/failed は EncodeWorker から、recording.deleted は
	// DeleteReconcileWorker から発火する（issue #73）。
	Webhook *webhook.Client

	// Cleanup は削除 reconcile（M3-8）の猶予・閾値設定。
	Cleanup config.CleanupConfig
}

// StoreDeps は、1 つの置き場に束ねた root・名前・store だけを持つ。
// 置き場（store）に束ねた依存で、build がプロセス共通の Deps と対で受け取るので、この名前にした。
// Deps を複製して値を上書きする形では、置き場に束ねた store の代入漏れでも、base の束ねない store が
// 黙って残ってコンパイルが通る。この型ではその状態を表せない。代入漏れで生じた nil は
// TestNewStoreScopedDepsInitializesEveryStoreDependency が検出する。
type StoreDeps struct {
	// MediaDir と MediaStoreName は、このワーカー群が束ねた 1 つの置き場の root と名前である。
	// 置き場で修飾するキューのワーカー（PerStore）と ingest は、置き場ごとの StoreDeps のこの値を使う。
	MediaDir       string
	MediaStoreName string

	// MediaReconcile は、この置き場に束ねた reconcile の永続化契約。
	MediaReconcile store.MediaReconcile
	MediaPublish   store.MediaPublish
	// IngestPrecheck は ingest が既存メディアを確認する永続化契約。
	IngestPrecheck store.IngestPrecheck
	// EncodeProgress は encode の進捗を報告する永続化契約。
	EncodeProgress store.EncodeProgressReporter
	MediaDelete    store.MediaDelete
	// MediaPendingReclaim は共有 FS にある派生物 pending copy の aged reclaim 用。
	MediaPendingReclaim store.PendingCopyReclaim
	StorageTransfer     store.StorageTransfer
	TSScan              store.TSScan
	EncodeAttempts      store.EncodeAttempts
	StorageObservation  store.StorageObservation
	// CMStationStore は局単位のロゴ状態と候補解析の永続化契約。
	CMStationStore store.CMStation
	// CMDetectionStore は CM 検出 worker の永続化契約。
	CMDetectionStore store.CMDetection
	// IngestTarget は ingest が record_sync から転送先を読む契約。
	IngestTarget store.IngestTarget
	// RecordingPurge は録画の物理削除の永続化契約。
	RecordingPurge store.RecordingPurge
	// Catalog は catalog の export と restore を行う契約。
	Catalog store.Catalog

	// DerivedMediaPublish は、この置き場で生成した派生物を DB に公開する契約。
	DerivedMediaPublish store.DerivedMediaPublisher
	// DerivedObjectDeleteStores は、派生物の置き場を掃く独立した削除パスの store 契約である。
	// nil なら、この置き場の worker は S3 の独立パスを持たない。
	DerivedObjectDeleteStores *store.DerivedObjectDeleteStores
}

// perStore は置き場ごとの StoreDeps を返す。空なら置き場で修飾したワーカーを 1 つも登録しない。
func (deps *Deps) perStore() map[string]*StoreDeps {
	return deps.PerStore
}

// NewWorkers は全 kind のワーカーを登録した jobq.Registry を返す。
// 購読キューは client 設定で絞るため、ここでは登録を減らさない。
// k8s の常駐 client も `ruler` queue だけを購読しながら全 kind を登録する。
// River の公開 AddWorker[T] API は worker の具体型を要求するため、ここで riveradapter.Register を通す。
// River 以外の adapter は中立な jobq.Registry をそのまま使える。
func NewWorkers(deps *Deps) *jobq.Registry {
	workers := jobq.NewRegistry()
	registerStoreRoutedWorkers(workers, deps)
	riveradapter.Register(workers, newIngestRoute(deps, func(d *Deps, s *StoreDeps) jobq.Worker[jobs.IngestJobArgs] {
		return newIngestWorker(d, s)
	}))
	riveradapter.Register(workers, &encode.EncodeReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Media:    deps.UnscopedMediaReconcile,
		// 転送待ちが backlog_limit に達した置き場への encode 投入を止める（背圧）。
		Transfers:     deps.UnscopedStorageTransfer,
		StoragePolicy: deps.StorageTransferPolicy,
		// desired の絞り込みにプロファイル名だけを使う（EncodeWorker と同じ
		// 設定を読む。EncodeReconcileArgs.Spec のコメント参照）。
		Profiles: deps.Encode,
		Markers:  deps.LoopMarkers,
	})
	riveradapter.Register(workers, &EpgSyncWorker{
		Enqueuer:       deps.Enqueuer,
		MirakcClients:  deps.MirakcClients,
		Store:          deps.EPGSync,
		RulerInput:     deps.RulerInputBumper,
		Markers:        deps.LoopMarkers,
		RetentionGrace: deps.EpgRetentionGrace,
	})
	riveradapter.Register(workers, &TunerSyncWorker{
		MirakcClients: deps.MirakcClients,
		TunerStore:    deps.TunerStore,
		CapacityStore: deps.CapacityStore,
		Markers:       deps.LoopMarkers,
	})
	riveradapter.Register(workers, &ruler.PassWorker{
		Enqueuer:          deps.Enqueuer,
		RulerPass:         deps.RulerPass,
		Breakers:          deps.Breakers,
		NeverScheduled:    deps.NeverScheduled,
		LoopMarkers:       deps.LoopMarkers,
		ProgramSearch:     deps.ProgramSearch,
		RetentionGrace:    deps.RulerRetentionGrace,
		MaxDeletesPerPass: deps.RulerMaxDeletesPerPass,
		RetractGrace:      deps.RulerRetractGrace,
	})
	riveradapter.Register(workers, &LabelRuleReconcileWorker{Store: deps.LabelRuleReevaluation})
	riveradapter.Register(workers, &reconciler.PassWorker{
		MirakcClients:   deps.MirakcClients,
		ScheduleSync:    deps.ScheduleSync,
		Breakers:        deps.Breakers,
		NeverScheduled:  deps.NeverScheduled,
		LoopMarkers:     deps.LoopMarkers,
		StartDelayGrace: deps.ReconcileStartDelayGrace,
	})
	riveradapter.Register(workers, &watcher.SweepWorker{
		Enqueuer:           deps.Enqueuer,
		MirakcClients:      deps.MirakcClients,
		RecordObservations: deps.RecordObservations,
		LoopMarkers:        deps.LoopMarkers,
		Webhook:            deps.Webhook,
	})
	riveradapter.Register(workers, &thumbnail.ThumbnailReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Media:    deps.UnscopedMediaReconcile,
		Markers:  deps.LoopMarkers,
	})
	riveradapter.Register(workers, &cmdetect.CMDetectReconcileWorker{
		Enqueuer:       deps.Enqueuer,
		Locator:        deps.OriginalLocator,
		DetectionStore: deps.UnscopedCMDetectionStore,
		RecoveryGrace:  deps.RecoveryGrace,
	})
	riveradapter.Register(workers, &storagemaint.StorageTransferReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Storage:  deps.UnscopedStorageTransfer,
		Config:   deps.StorageTransferPolicy,
	})
	riveradapter.Register(workers, &storagemaint.StorageTransferWorker{
		Storage:      deps.UnscopedStorageTransfer,
		Config:       deps.StorageTransferPolicy,
		Roots:        deps.StorageRoots,
		ObjectStores: deps.StorageObjects,
		ScratchDir:   deps.ScratchDir,
	})
	riveradapter.Register(workers, &tsscan.ReconcileWorker{Enqueuer: deps.Enqueuer, Media: deps.UnscopedTSScan, Locator: deps.OriginalLocator, Markers: deps.LoopMarkers})
	return workers
}

// newIngestWorker はプロセス共通の Deps と置き場の StoreDeps から ingest worker を組み立てる。
func newIngestWorker(d *Deps, s *StoreDeps) *ingest.IngestWorker {
	return &ingest.IngestWorker{
		StorageProbed:       d.IngestStorageProbed,
		Enqueuer:            d.Enqueuer,
		MirakcClients:       d.MirakcClients,
		Publisher:           s.MediaPublish,
		Precheck:            s.IngestPrecheck,
		RecordTarget:        s.IngestTarget,
		CMDetectionStore:    s.CMDetectionStore,
		MediaDir:            s.MediaDir,
		MediaStoreName:      s.MediaStoreName,
		StallTimeout:        d.IngestStallTimeout,
		CMDetect:            d.CMDetect,
		CutProfiles:         d.Encode.CutProfileSet(),
		RecordingHLSEnabled: d.RecordingHLSEnabled,
	}
}

// registerStoreRoutedWorkers は、原本のある置き場に束ねて動くワーカーを登録する。ジョブの置き場を
// マウントしていないプロセスでは、どれもファイルに触れる前に失敗する（newStoreRoute）。
func registerStoreRoutedWorkers(workers *jobq.Registry, deps *Deps) {
	riveradapter.Register[jobs.EncodeJobArgs](workers, newStoreRoute("encode", deps, deps.perStore(), func(d *Deps, s *StoreDeps) jobq.Worker[jobs.EncodeJobArgs] {
		return &encode.EncodeWorker{
			Media:           s.MediaReconcile,
			Attempts:        s.EncodeAttempts,
			Publisher:       s.MediaPublish,
			ObjectPublisher: s.DerivedMediaPublish,
			Objects:         d.DerivedObjects,
			Progress:        s.EncodeProgress,
			MediaDir:        s.MediaDir,
			MediaStoreName:  s.MediaStoreName,
			ScratchDir:      d.ScratchDir,
			FFmpeg:          d.Encode.FFmpeg,
			FFprobe:         d.Encode.FFprobe,
			Profiles:        d.Encode,
			Webhook:         d.Webhook,
		}
	}))
	riveradapter.Register[jobs.CatalogExportArgs](workers, newStoreRoute("catalog_export", deps, deps.perStore(), func(d *Deps, s *StoreDeps) jobq.Worker[jobs.CatalogExportArgs] {
		return &CatalogExportWorker{
			Store:          s.Catalog,
			MediaDir:       s.MediaDir,
			MediaStoreName: s.MediaStoreName,
		}
	}))
	riveradapter.Register[jobs.ThumbnailJobArgs](workers, newStoreRoute("thumbnail", deps, deps.perStore(), func(d *Deps, s *StoreDeps) jobq.Worker[jobs.ThumbnailJobArgs] {
		return &thumbnail.ThumbnailWorker{
			Media:            s.MediaReconcile,
			ObjectPublisher:  s.DerivedMediaPublish,
			Objects:          d.DerivedObjects,
			MediaDir:         s.MediaDir,
			MediaStoreName:   s.MediaStoreName,
			DerivedStoreName: d.DerivedObjectStoreName,
			ScratchDir:       d.ScratchDir,
			FFmpeg:           d.Encode.FFmpeg,
			FFprobe:          d.Encode.FFprobe,
		}
	}))
	riveradapter.Register[jobs.SeekTilesJobArgs](workers, newStoreRoute("seek_tiles", deps, deps.perStore(), func(d *Deps, s *StoreDeps) jobq.Worker[jobs.SeekTilesJobArgs] {
		return &thumbnail.SeekTilesWorker{
			Media:           s.MediaReconcile,
			ObjectPublisher: s.DerivedMediaPublish,
			Objects:         d.DerivedObjects,
			MediaDir:        s.MediaDir,
			MediaStoreName:  s.MediaStoreName,
			ScratchDir:      d.ScratchDir,
			FFmpeg:          d.Encode.FFmpeg,
			FFprobe:         d.Encode.FFprobe,
		}
	}))
	riveradapter.Register[jobs.CMDetectJobArgs](workers, newStoreRoute("cm_detect", deps, deps.perStore(), func(d *Deps, s *StoreDeps) jobq.Worker[jobs.CMDetectJobArgs] {
		return &cmdetect.CMDetectWorker{
			Enqueuer:       d.Enqueuer,
			DetectionStore: s.CMDetectionStore,
			MediaDir:       s.MediaDir,
			StoreName:      s.MediaStoreName,
			ScratchDir:     d.ScratchDir,
			CMDetect:       d.CMDetect,
			FFprobe:        d.Encode.FFprobe,
		}
	}))
	riveradapter.Register[jobs.CMLogoCandidateJobArgs](workers, newStoreRoute("cm_logo_candidate", deps, deps.perStore(), func(d *Deps, s *StoreDeps) jobq.Worker[jobs.CMLogoCandidateJobArgs] {
		return &cmdetect.CMLogoCandidateWorker{
			Store:      s.CMStationStore,
			MediaDir:   s.MediaDir,
			ScratchDir: d.ScratchDir,
			CMDetect:   d.CMDetect,
			FFprobe:    d.Encode.FFprobe,
		}
	}))
	riveradapter.Register[jobs.DeleteReconcileArgs](workers, newStoreRoute("delete_reconcile", deps, deps.perStore(), func(d *Deps, s *StoreDeps) jobq.Worker[jobs.DeleteReconcileArgs] {
		return newDeleteReconcileWorker(d, s)
	}))
	riveradapter.Register[jobs.StorageSyncArgs](workers, newStoreRoute("storage_sync", deps, deps.perStore(), func(d *Deps, s *StoreDeps) jobq.Worker[jobs.StorageSyncArgs] {
		return &StorageSyncWorker{
			Storage:    s.StorageObservation,
			MediaDir:   s.MediaDir,
			ScratchDir: d.ScratchDir,
		}
	}))
	riveradapter.Register[tsscan.ScanArgs](workers, newStoreRoute("ts_scan", deps, deps.perStore(), func(d *Deps, s *StoreDeps) jobq.Worker[tsscan.ScanArgs] {
		return &tsscan.ScanWorker{Media: s.TSScan, MediaDir: s.MediaDir}
	}))
}

// newDeleteReconcileWorker は削除 reconcile worker を Deps と StoreDeps から組み立てる。
func newDeleteReconcileWorker(deps *Deps, scoped *StoreDeps) *storagemaint.DeleteReconcileWorker {
	return &storagemaint.DeleteReconcileWorker{
		MediaDelete:               scoped.MediaDelete,
		MediaPendingReclaim:       scoped.MediaPendingReclaim,
		StorageTransfer:           scoped.StorageTransfer,
		RecordingPurge:            scoped.RecordingPurge,
		Breakers:                  deps.Breakers,
		Storage:                   scoped.StorageObservation,
		MediaDir:                  scoped.MediaDir,
		MediaStoreName:            scoped.MediaStoreName,
		DerivedObjects:            deps.DerivedObjects,
		DerivedObjectStoreName:    deps.DerivedObjectStoreName,
		DerivedObjectDeleteStores: scoped.DerivedObjectDeleteStores,
		StorageObjects:            deps.StorageObjects,
		TransferOverlap:           deps.TransferOverlap,
		TrashRetention:            deps.Cleanup.TrashRetention,
		OrphanMTimeGrace:          deps.Cleanup.OrphanMTimeGrace,
		OrphanAge:                 deps.Cleanup.OrphanAge,
		MaxDeletesPerPass:         deps.Cleanup.MaxDeletesPerPass,
		MissingAssetAge:           deps.Cleanup.MissingAssetAge,
		Webhook:                   deps.Webhook,
		Markers:                   deps.LoopMarkers,
	}
}
