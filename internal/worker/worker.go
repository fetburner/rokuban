package worker

import (
	"fmt"
	"sort"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/tsscan"
	"github.com/fetburner/rokuban/internal/webhook"
)

// verifySite は、mirakc かファイルシステムに触るワーカーが処理しようとしている
// ジョブの site（jobSite）が、このワーカープロセスが束縛している mirakc
// クライアントの集合（clients、site → *mirakc.Client。Deps.MirakcClients /
// `--sites` の束縛）に含まれるかを検査する fail-fast ガード（issue #139。
// 1 プロセスが N site を束縛できるようになった issue #532 で map 化した）。
//
// mirakc の record id / programId はインスタンススコープであり、DB の site 列は
// そのスコープの境界を表現するために存在する（issue #31、docs/schema.md
// §1-5）。**一次防御はキュー選択そのもの**: site 単位のキュー（ingest/epg/
// reconciler/watcher）は物理名を site で修飾する（River adapter、issue #185
// M4-13）ため、site A 宛のキューを購読していない worker は site B 向けのジョブを
// 購読すらしない（cmd/rokuban.validateSiteBinding が worker.queues とサイト束縛の
// 組み合わせも検査する）。verifySite はその後ろに立つ**二次防御**で、キュー選択が
// 正しくてもジョブの Args.Site 自体が壊れている経路（手で INSERT した / 将来の
// ヒント投入のバグ / River の一意性判定が旧キューの残骸と合流して args だけが
// 別サイトのまま挿入された、等）を捕まえる。掴んだまま mirakc/FS に触れると、
// A の mirakc に B の id を投げて A の別番組を B の recording としてコミットし
// うる（不変条件 3「コミット = DB 行」への違反。詳細は issue #139 本文）。
//
// **1 プロセスが N site を束縛しうるため、もはや「一致するか」ではなく「clients に
// jobSite のエントリがあるか」を見る。** 同じ IngestWorker/EpgSyncWorker/... の
// 1 インスタンスが複数 site の物理キューを同時に購読しうる（riveradapter の buildConfig が
// site ごとに物理名を組み立てて subscribe する）ため、ワーカーはもう単一の
// 「自分自身の site」を持たない。見つかれば、呼び出し側がそのまま使う
// *mirakc.Client を返す（呼び出し側が再度 map を引く必要をなくし、verifySite が
// 通した site とその後実際に使うクライアントがズレる経路を作らない）。
//
// **jobSite は正規化しない。空文字列は無条件に拒む。** config のバリデーションが
// site 名の非空を要求するため、clients のキーは常に実際の site 名であり、
// 空文字列がキーになることはない。jobSite を store.DefaultSite へ読み替えてから
// 引くと、たまたま "default" という名前の site が束縛されているデプロイでは
// `Site: ""` の（本来壊れている）ジョブが素通りしてしまう --- 二次防御としての
// verifySite が拾いたいのはまさにこの「args.Site 自体が壊れている経路」
// （このコメント冒頭の説明）なので、jobSite を素の値のまま引く。
//
// 呼び出し側は mirakc/FS に触れる**前**（最初の HTTP 呼び出し・一時ファイル作成等より
// 前）に呼ぶこと。合わないジョブは即座に失敗させ、再試行は River に委ねる
// （同じジョブが必ず自サイトの worker に回る保証はないが、他サイトの worker が
// いずれ拾う。うるさいが安全 --- issue #139 本文の「id が自サイトに存在しない」
// ケースと同じ扱いに揃う）。
func verifySite(clients map[string]*mirakc.Client, jobSite, queue string) (*mirakc.Client, error) {
	client, ok := clients[jobSite]
	if !ok {
		return nil, fmt.Errorf(
			"%s: job site %q is not among this worker's bound sites %s, refusing before touching mirakc/FS (issue #139)",
			queue, jobSite, sortedSiteNames(clients))
	}
	return client, nil
}

// sortedSiteNames は clients のキー（site 名）をソートして返す。verifySite の
// エラーメッセージ用（sortedQueueNames と同じ理由 --- map の反復順は不定なので、
// エラーメッセージが実行のたびに揺れないようにする）。
func sortedSiteNames(clients map[string]*mirakc.Client) []string {
	sites := make([]string, 0, len(clients))
	for site := range clients {
		sites = append(sites, site)
	}
	sort.Strings(sites)
	return sites
}

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
	// LoopMarkers は ruler と record_sweep の成功時刻を記録する契約。
	LoopMarkers store.LoopMarkers
	// Breakers は worker がサーキットブレーカーを発動・観測する契約。
	Breakers store.Breakers
	// NeverScheduled は、番組終了後に schedule が観測されなかった事実を記録・削除する契約。
	NeverScheduled store.NeverScheduledEvents
	// ScheduleSync は reconciler が schedule の観測と予約との差分を記録し、読み取る契約。
	ScheduleSync store.ScheduleSync
	// RecordObservations は mirakc の record 観測を永続化する契約。
	RecordObservations store.RecordObservations
	// IngestTarget は ingest が record_sync から転送先を読む契約。
	IngestTarget store.IngestTarget
	// Catalog は catalog の export と restore を行う契約。
	Catalog store.Catalog

	// MirakcClients は `--sites` で束縛された site 名から mirakc クライアントへの
	// map（issue #532。1 プロセスが N site を束縛できるようになったため、
	// 単一の MirakcClient / Site string の対を置き換えた）。mirakc/FS に触る
	// ワーカー（Ingest / EpgSync / TunerSync / ReconcilePass / RecordSweep）は
	// これと自分が処理するジョブの args.Site を verifySite で照合してから
	// 実際に使うクライアントを取り出す（issue #139）。
	//
	// 空 map（0 site 束縛の中央プロセス）でも構築時にはエラーにしない ---
	// site 単位のキューを実際に購読していれば、届いたジョブは verifySite が
	// 必ず拒む（起動時には validateSiteBinding が
	// この組み合わせ自体を防ぐ）。
	MirakcClients map[string]*mirakc.Client
	// CMStationStore は局単位のロゴ状態と候補解析の永続化契約。
	CMStationStore store.CMStation
	// CMDetectionStore は CM 検出 worker・reconcile の永続化契約。
	CMDetectionStore store.CMDetection
	// TunerStore はチューナー同期の永続化契約。
	TunerStore store.TunerSync
	// CapacityStore はチューナー worker の容量判定読み取り契約。
	CapacityStore store.Capacity
	// RecordingPurge は録画の物理削除の永続化契約。
	RecordingPurge store.RecordingPurge
	// LabelRuleReevaluation は分類ルールの全件再評価を永続化する。
	LabelRuleReevaluation store.LabelRuleReevaluation
	MediaReconcile        store.MediaReconcile
	MediaPublish          store.MediaPublish
	// DerivedMediaPublish と DerivedObjects は派生物の置き場に束ねた契約。
	DerivedMediaPublish    store.DerivedMediaPublisher
	DerivedObjects         objstore.Store
	DerivedObjectStoreName string
	// DerivedMediaDelete・DerivedPendingReclaim・DerivedStorage は、独立した派生物置き場の
	// 削除 reconcile 用の store。
	DerivedMediaDelete    store.MediaDelete
	DerivedPendingReclaim store.PendingCopyReclaim
	DerivedStorage        store.StorageObservation
	StorageObjects        map[string]objstore.Store
	IngestPrecheck        store.IngestPrecheck
	EncodeProgress        store.EncodeProgressReporter
	MediaDelete           store.MediaDelete
	// MediaPendingReclaim は共有 FS にある派生物 pending copy の aged reclaim 用。
	MediaPendingReclaim   store.PendingCopyReclaim
	StorageTransfer       store.StorageTransfer
	StorageTransferPolicy config.StorageConfig
	StorageRoots          map[string]string
	TSScan                store.TSScan
	EncodeAttempts        store.EncodeAttempts
	StorageObservation    store.StorageObservation
	// MediaDir と MediaStoreName は、このワーカー群が束ねた 1 つの置き場の root と名前である。
	// 置き場で修飾するキューのワーカー（PerStore）と ingest は、置き場ごとの Deps のこの値を使う。
	MediaDir       string
	MediaStoreName string
	// PerStore は、マウントした FS 置き場ごとに束ねた Deps（置き場の名前から）。MediaDir・
	// MediaStoreName と、置き場に束ねた store（MediaReconcile など）を置き場ごとに持つ。
	// 空なら置き場で修飾したワーカーは登録されず、その kind のジョブは置き場の不一致で失敗する。
	// このトップレベルの store は置き場に束ねず、DB だけを触る reconcile が使う。
	PerStore map[string]*Deps
	// MountedStores はマウントした FS 置き場の名前を `--stores` の順で持つ。ingest が規則で
	// 選べなかったときに書く先頭の置き場を決める（名前順ではない。内容は PerStore と同じ集合）。
	MountedStores []string
	// OriginalLocator は reconcile が読み取りジョブを積む置き場の選択に使う。置き場に束ねない。
	OriginalLocator store.OriginalLocator
	// IngestStores は ingest が書き込む置き場を規則から選ぶ契約。
	IngestStores    store.IngestStoreChooser
	TransferOverlap time.Duration
	// IngestStorageProbed は起動時に ProbeIngestStorage が成功したかを示す。
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

// perStore は置き場ごとの Deps を返す。空なら置き場で修飾したワーカーを 1 つも登録しない。
func (deps *Deps) perStore() map[string]*Deps {
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
	riveradapter.Register(workers, newIngestRoute(deps, func(d *Deps) jobq.Worker[jobs.IngestJobArgs] {
		return &IngestWorker{
			storageProbed:       d.IngestStorageProbed,
			Enqueuer:            d.Enqueuer,
			MirakcClients:       d.MirakcClients,
			Publisher:           d.MediaPublish,
			Precheck:            d.IngestPrecheck,
			RecordTarget:        d.IngestTarget,
			CMDetectionStore:    d.CMDetectionStore,
			MediaDir:            d.MediaDir,
			MediaStoreName:      d.MediaStoreName,
			StallTimeout:        d.IngestStallTimeout,
			CMDetect:            d.CMDetect,
			CutProfiles:         d.Encode.CutProfileSet(),
			RecordingHLSEnabled: d.RecordingHLSEnabled,
		}
	}))
	riveradapter.Register(workers, &EncodeReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Media:    deps.MediaReconcile,
		// desired の絞り込みにプロファイル名だけを使う（EncodeWorker と同じ
		// 設定を読む。EncodeReconcileArgs.Spec のコメント参照）。
		Profiles: deps.Encode,
	})
	riveradapter.Register(workers, &EpgSyncWorker{
		Enqueuer:       deps.Enqueuer,
		MirakcClients:  deps.MirakcClients,
		Store:          deps.EPGSync,
		RulerInput:     deps.RulerInputBumper,
		RetentionGrace: deps.EpgRetentionGrace,
	})
	riveradapter.Register(workers, &TunerSyncWorker{
		MirakcClients: deps.MirakcClients,
		TunerStore:    deps.TunerStore,
		CapacityStore: deps.CapacityStore,
	})
	riveradapter.Register(workers, &RulerPassWorker{
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
	riveradapter.Register(workers, &ReconcilePassWorker{
		MirakcClients:   deps.MirakcClients,
		ScheduleSync:    deps.ScheduleSync,
		Breakers:        deps.Breakers,
		NeverScheduled:  deps.NeverScheduled,
		StartDelayGrace: deps.ReconcileStartDelayGrace,
	})
	riveradapter.Register(workers, &RecordSweepWorker{
		Enqueuer:           deps.Enqueuer,
		MirakcClients:      deps.MirakcClients,
		RecordObservations: deps.RecordObservations,
		LoopMarkers:        deps.LoopMarkers,
		Webhook:            deps.Webhook,
	})
	riveradapter.Register(workers, &ThumbnailReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Media:    deps.MediaReconcile,
	})
	riveradapter.Register(workers, &CMDetectReconcileWorker{
		Enqueuer:       deps.Enqueuer,
		Locator:        deps.OriginalLocator,
		DetectionStore: deps.CMDetectionStore,
		RecoveryGrace:  deps.RecoveryGrace,
	})
	riveradapter.Register(workers, &StorageTransferReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Storage:  deps.StorageTransfer,
		Config:   deps.StorageTransferPolicy,
	})
	riveradapter.Register(workers, &StorageTransferWorker{
		Storage:      deps.StorageTransfer,
		Config:       deps.StorageTransferPolicy,
		Roots:        deps.StorageRoots,
		ObjectStores: deps.StorageObjects,
		ScratchDir:   deps.ScratchDir,
	})
	riveradapter.Register(workers, &tsscan.ReconcileWorker{Enqueuer: deps.Enqueuer, Media: deps.TSScan, Locator: deps.OriginalLocator})
	return workers
}

// registerStoreRoutedWorkers は、原本のある置き場に束ねて動くワーカーを登録する。ジョブの置き場を
// マウントしていないプロセスでは、どれもファイルに触れる前に失敗する（newStoreRoute）。
func registerStoreRoutedWorkers(workers *jobq.Registry, deps *Deps) {
	riveradapter.Register[jobs.EncodeJobArgs](workers, newStoreRoute("encode", deps.perStore(), func(d *Deps) jobq.Worker[jobs.EncodeJobArgs] {
		return &EncodeWorker{
			Media:           d.MediaReconcile,
			Attempts:        d.EncodeAttempts,
			Publisher:       d.MediaPublish,
			ObjectPublisher: d.DerivedMediaPublish,
			Objects:         d.DerivedObjects,
			Progress:        d.EncodeProgress,
			MediaDir:        d.MediaDir,
			MediaStoreName:  d.MediaStoreName,
			ScratchDir:      d.ScratchDir,
			FFmpeg:          d.Encode.FFmpeg,
			FFprobe:         d.Encode.FFprobe,
			Profiles:        d.Encode,
			Webhook:         d.Webhook,
		}
	}))
	riveradapter.Register[jobs.CatalogExportArgs](workers, newStoreRoute("catalog_export", deps.perStore(), func(d *Deps) jobq.Worker[jobs.CatalogExportArgs] {
		return &CatalogExportWorker{
			Store:          d.Catalog,
			MediaDir:       d.MediaDir,
			MediaStoreName: d.MediaStoreName,
		}
	}))
	riveradapter.Register[jobs.ThumbnailJobArgs](workers, newStoreRoute("thumbnail", deps.perStore(), func(d *Deps) jobq.Worker[jobs.ThumbnailJobArgs] {
		return &ThumbnailWorker{
			Media:            d.MediaReconcile,
			ObjectPublisher:  d.DerivedMediaPublish,
			Objects:          d.DerivedObjects,
			MediaDir:         d.MediaDir,
			MediaStoreName:   d.MediaStoreName,
			DerivedStoreName: d.DerivedObjectStoreName,
			ScratchDir:       d.ScratchDir,
			FFmpeg:           d.Encode.FFmpeg,
			FFprobe:          d.Encode.FFprobe,
		}
	}))
	riveradapter.Register[jobs.SeekTilesJobArgs](workers, newStoreRoute("seek_tiles", deps.perStore(), func(d *Deps) jobq.Worker[jobs.SeekTilesJobArgs] {
		return &SeekTilesWorker{
			Media:           d.MediaReconcile,
			ObjectPublisher: d.DerivedMediaPublish,
			Objects:         d.DerivedObjects,
			MediaDir:        d.MediaDir,
			MediaStoreName:  d.MediaStoreName,
			ScratchDir:      d.ScratchDir,
			FFmpeg:          d.Encode.FFmpeg,
			FFprobe:         d.Encode.FFprobe,
		}
	}))
	riveradapter.Register[jobs.CMDetectJobArgs](workers, newStoreRoute("cm_detect", deps.perStore(), func(d *Deps) jobq.Worker[jobs.CMDetectJobArgs] {
		return &CMDetectWorker{
			Enqueuer:       d.Enqueuer,
			DetectionStore: d.CMDetectionStore,
			MediaDir:       d.MediaDir,
			StoreName:      d.MediaStoreName,
			ScratchDir:     d.ScratchDir,
			CMDetect:       d.CMDetect,
			FFprobe:        d.Encode.FFprobe,
		}
	}))
	riveradapter.Register[jobs.CMLogoCandidateJobArgs](workers, newStoreRoute("cm_logo_candidate", deps.perStore(), func(d *Deps) jobq.Worker[jobs.CMLogoCandidateJobArgs] {
		return &CMLogoCandidateWorker{
			Store:      d.CMStationStore,
			MediaDir:   d.MediaDir,
			ScratchDir: d.ScratchDir,
			CMDetect:   d.CMDetect,
			FFprobe:    d.Encode.FFprobe,
		}
	}))
	riveradapter.Register[jobs.DeleteReconcileArgs](workers, newStoreRoute("delete_reconcile", deps.perStore(), func(d *Deps) jobq.Worker[jobs.DeleteReconcileArgs] {
		return newDeleteReconcileWorker(d)
	}))
	riveradapter.Register[jobs.StorageSyncArgs](workers, newStoreRoute("storage_sync", deps.perStore(), func(d *Deps) jobq.Worker[jobs.StorageSyncArgs] {
		return &StorageSyncWorker{
			Storage:    d.StorageObservation,
			MediaDir:   d.MediaDir,
			ScratchDir: d.ScratchDir,
		}
	}))
	riveradapter.Register[tsscan.ScanArgs](workers, newStoreRoute("ts_scan", deps.perStore(), func(d *Deps) jobq.Worker[tsscan.ScanArgs] {
		return &tsscan.ScanWorker{Media: d.TSScan, MediaDir: d.MediaDir}
	}))
}

// newDeleteReconcileWorker は削除 reconcile worker を Deps から組み立てる。
func newDeleteReconcileWorker(deps *Deps) *DeleteReconcileWorker {
	return &DeleteReconcileWorker{
		MediaDelete:            deps.MediaDelete,
		MediaPendingReclaim:    deps.MediaPendingReclaim,
		StorageTransfer:        deps.StorageTransfer,
		StorageTransferPolicy:  deps.StorageTransferPolicy,
		RecordingPurge:         deps.RecordingPurge,
		Breakers:               deps.Breakers,
		Storage:                deps.StorageObservation,
		MediaDir:               deps.MediaDir,
		MediaStoreName:         deps.MediaStoreName,
		DerivedObjects:         deps.DerivedObjects,
		DerivedObjectStoreName: deps.DerivedObjectStoreName,
		DerivedMediaDelete:     deps.DerivedMediaDelete,
		DerivedPendingReclaim:  deps.DerivedPendingReclaim,
		DerivedStorage:         deps.DerivedStorage,
		StorageObjects:         deps.StorageObjects,
		TransferOverlap:        deps.TransferOverlap,
		TrashRetention:         deps.Cleanup.TrashRetention,
		OrphanMTimeGrace:       deps.Cleanup.OrphanMTimeGrace,
		OrphanAge:              deps.Cleanup.OrphanAge,
		MaxDeletesPerPass:      deps.Cleanup.MaxDeletesPerPass,
		MissingAssetAge:        deps.Cleanup.MissingAssetAge,
		Webhook:                deps.Webhook,
	}
}
