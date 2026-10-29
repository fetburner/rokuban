package worker

import (
	"fmt"
	"sort"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
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
	// DerivedMediaPublish と DerivedObjects は、派生物の置き場が S3 のときだけ設定する。
	// DerivedMediaPublish は S3 の置き場に束ねた store で、encode・サムネイル・シークタイルが使う。
	DerivedMediaPublish    store.DerivedMediaPublisher
	DerivedObjects         objstore.Store
	DerivedObjectStoreName string
	// DerivedMediaDelete・DerivedPendingReclaim・DerivedStorage は、同じ S3 の置き場に束ねた
	// 削除 reconcile 用の store。
	DerivedMediaDelete    store.MediaDelete
	DerivedPendingReclaim store.PendingCopyReclaim
	DerivedStorage        store.StorageObservation
	IngestPrecheck        store.IngestPrecheck
	EncodeProgress        store.EncodeProgressReporter
	MediaDelete           store.MediaDelete
	TSScan                store.TSScan
	EncodeAttempts        store.EncodeAttempts
	StorageObservation    store.StorageObservation
	MediaDir              string
	MediaStoreName        string
	TransferOverlap       time.Duration
	// IngestStorageProbed は起動時に ProbeIngestStorage が成功したかを示す。
	// false なら IngestWorker が最初の ingest で、目印を照合した後に probe する。
	IngestStorageProbed bool
	ScratchDir          string

	// Encode は構造化エンコードプロファイルと ffmpeg パス（issue #64 / #65）。
	// worker ロール起動時に ValidateTools 済み（不変条件 4）。
	Encode      config.EncodeConfig
	CMDetect    config.CMDetectConfig
	LiveEnabled bool

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

// NewWorkers は全 kind のワーカーを登録した jobq.Registry を返す。
// 購読キューは client 設定で絞るため、ここでは登録を減らさない。
// k8s の常駐 client も `ruler` queue だけを購読しながら全 kind を登録する。
// River の公開 AddWorker[T] API は worker の具体型を要求するため、ここで riveradapter.Register を通す。
// River 以外の adapter は中立な jobq.Registry をそのまま使える。
func NewWorkers(deps *Deps) *jobq.Registry {
	workers := jobq.NewRegistry()
	riveradapter.Register(workers, &IngestWorker{
		storageProbed:    deps.IngestStorageProbed,
		Enqueuer:         deps.Enqueuer,
		MirakcClients:    deps.MirakcClients,
		Publisher:        deps.MediaPublish,
		Precheck:         deps.IngestPrecheck,
		RecordTarget:     deps.IngestTarget,
		CMDetectionStore: deps.CMDetectionStore,
		MediaDir:         deps.MediaDir,
		MediaStoreName:   deps.MediaStoreName,
		StallTimeout:     deps.IngestStallTimeout,
		CMDetect:         deps.CMDetect,
		CutProfiles:      deps.Encode.CutProfileSet(),
		LiveEnabled:      deps.LiveEnabled,
	})
	riveradapter.Register(workers, &EncodeWorker{
		Media:           deps.MediaReconcile,
		Attempts:        deps.EncodeAttempts,
		Publisher:       deps.MediaPublish,
		ObjectPublisher: deps.DerivedMediaPublish,
		Objects:         deps.DerivedObjects,
		Progress:        deps.EncodeProgress,
		MediaDir:        deps.MediaDir,
		MediaStoreName:  deps.MediaStoreName,
		ScratchDir:      deps.ScratchDir,
		FFmpeg:          deps.Encode.FFmpeg,
		FFprobe:         deps.Encode.FFprobe,
		Profiles:        deps.Encode,
		Webhook:         deps.Webhook,
	})
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
	riveradapter.Register(workers, &CatalogExportWorker{
		Store:          deps.Catalog,
		MediaDir:       deps.MediaDir,
		MediaStoreName: deps.MediaStoreName,
	})
	riveradapter.Register(workers, &ThumbnailWorker{
		Media:            deps.MediaReconcile,
		Publisher:        deps.MediaPublish,
		ObjectPublisher:  deps.DerivedMediaPublish,
		Objects:          deps.DerivedObjects,
		MediaDir:         deps.MediaDir,
		MediaStoreName:   deps.MediaStoreName,
		DerivedStoreName: deps.DerivedObjectStoreName,
		ScratchDir:       deps.ScratchDir,
		FFmpeg:           deps.Encode.FFmpeg,
		FFprobe:          deps.Encode.FFprobe,
	})
	riveradapter.Register(workers, &SeekTilesWorker{
		Media:           deps.MediaReconcile,
		Publisher:       deps.MediaPublish,
		ObjectPublisher: deps.DerivedMediaPublish,
		Objects:         deps.DerivedObjects,
		MediaDir:        deps.MediaDir,
		MediaStoreName:  deps.MediaStoreName,
		ScratchDir:      deps.ScratchDir,
		FFmpeg:          deps.Encode.FFmpeg,
		FFprobe:         deps.Encode.FFprobe,
	})
	riveradapter.Register(workers, &ThumbnailReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Media:    deps.MediaReconcile,
	})
	riveradapter.Register(workers, &CMDetectWorker{
		Enqueuer:       deps.Enqueuer,
		DetectionStore: deps.CMDetectionStore,
		MediaDir:       deps.MediaDir,
		ScratchDir:     deps.ScratchDir,
		CMDetect:       deps.CMDetect,
		FFprobe:        deps.Encode.FFprobe,
	})
	riveradapter.Register(workers, &CMLogoCandidateWorker{
		Store:      deps.CMStationStore,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
		CMDetect:   deps.CMDetect,
		FFprobe:    deps.Encode.FFprobe,
	})
	riveradapter.Register(workers, &CMDetectReconcileWorker{
		Enqueuer:       deps.Enqueuer,
		DetectionStore: deps.CMDetectionStore,
		RecoveryGrace:  deps.RecoveryGrace,
	})
	riveradapter.Register(workers, newDeleteReconcileWorker(deps))
	riveradapter.Register(workers, &StorageSyncWorker{
		Storage:    deps.StorageObservation,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
	})
	riveradapter.Register(workers, &tsscan.ScanWorker{Media: deps.TSScan, MediaDir: deps.MediaDir})
	riveradapter.Register(workers, &tsscan.ReconcileWorker{Enqueuer: deps.Enqueuer, Media: deps.TSScan})
	return workers
}

// newDeleteReconcileWorker は削除 reconcile worker を Deps から組み立てる。
func newDeleteReconcileWorker(deps *Deps) *DeleteReconcileWorker {
	return &DeleteReconcileWorker{
		MediaDelete:            deps.MediaDelete,
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
		TransferOverlap:        deps.TransferOverlap,
		TrashRetention:         deps.Cleanup.TrashRetention,
		OrphanMTimeGrace:       deps.Cleanup.OrphanMTimeGrace,
		OrphanAge:              deps.Cleanup.OrphanAge,
		MaxDeletesPerPass:      deps.Cleanup.MaxDeletesPerPass,
		MissingAssetAge:        deps.Cleanup.MissingAssetAge,
		Webhook:                deps.Webhook,
	}
}
