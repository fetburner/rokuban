package worker

import (
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/mirakc"
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
// 空文字列がキーになることはない。jobSite を db.DefaultSite へ読み替えてから
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
	Pool          *pgxpool.Pool
	MediaDir      string
	ScratchDir    string

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
	// （docs/recording/ingest.md §5.3「層 1」）。
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
		Enqueuer:      deps.Enqueuer,
		MirakcClients: deps.MirakcClients,
		Pool:          deps.Pool,
		MediaDir:      deps.MediaDir,
		StallTimeout:  deps.IngestStallTimeout,
		CMDetect:      deps.CMDetect,
		CutProfiles:   deps.Encode.CutProfileSet(),
		LiveEnabled:   deps.LiveEnabled,
	})
	riveradapter.Register(workers, &EncodeWorker{
		Pool:       deps.Pool,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
		FFmpeg:     deps.Encode.FFmpeg,
		FFprobe:    deps.Encode.FFprobe,
		Profiles:   deps.Encode,
		Webhook:    deps.Webhook,
	})
	riveradapter.Register(workers, &EncodeRebuildWorker{Enqueuer: deps.Enqueuer, Pool: deps.Pool, Profiles: deps.Encode})
	riveradapter.Register(workers, &EncodeReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Pool:     deps.Pool,
		// desired の絞り込みにプロファイル名だけを使う（EncodeWorker と同じ
		// 設定を読む。EncodeReconcileArgs.Spec のコメント参照）。
		Profiles: deps.Encode,
	})
	riveradapter.Register(workers, &EpgSyncWorker{
		Enqueuer:       deps.Enqueuer,
		MirakcClients:  deps.MirakcClients,
		Pool:           deps.Pool,
		RetentionGrace: deps.EpgRetentionGrace,
	})
	riveradapter.Register(workers, &TunerSyncWorker{
		MirakcClients: deps.MirakcClients,
		Pool:          deps.Pool,
	})
	riveradapter.Register(workers, &RulerPassWorker{
		Enqueuer:          deps.Enqueuer,
		Pool:              deps.Pool,
		RetentionGrace:    deps.RulerRetentionGrace,
		MaxDeletesPerPass: deps.RulerMaxDeletesPerPass,
		RetractGrace:      deps.RulerRetractGrace,
	})
	riveradapter.Register(workers, &LabelRuleReconcileWorker{Pool: deps.Pool})
	riveradapter.Register(workers, &ReconcilePassWorker{
		MirakcClients:   deps.MirakcClients,
		Pool:            deps.Pool,
		StartDelayGrace: deps.ReconcileStartDelayGrace,
	})
	riveradapter.Register(workers, &RecordSweepWorker{
		Enqueuer:      deps.Enqueuer,
		MirakcClients: deps.MirakcClients,
		Pool:          deps.Pool,
		Webhook:       deps.Webhook,
	})
	riveradapter.Register(workers, &CatalogExportWorker{
		Pool:     deps.Pool,
		MediaDir: deps.MediaDir,
	})
	riveradapter.Register(workers, &ThumbnailWorker{
		Pool:       deps.Pool,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
		FFmpeg:     deps.Encode.FFmpeg,
		FFprobe:    deps.Encode.FFprobe,
	})
	riveradapter.Register(workers, &SeekTilesWorker{
		Pool:       deps.Pool,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
		FFmpeg:     deps.Encode.FFmpeg,
		FFprobe:    deps.Encode.FFprobe,
	})
	riveradapter.Register(workers, &ThumbnailReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Pool:     deps.Pool,
	})
	riveradapter.Register(workers, &CMDetectWorker{
		Enqueuer:   deps.Enqueuer,
		Pool:       deps.Pool,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
		CMDetect:   deps.CMDetect,
		FFprobe:    deps.Encode.FFprobe,
	})
	riveradapter.Register(workers, &CMLogoCandidateWorker{
		Pool:       deps.Pool,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
		CMDetect:   deps.CMDetect,
		FFprobe:    deps.Encode.FFprobe,
	})
	riveradapter.Register(workers, &CMDetectReconcileWorker{
		Enqueuer: deps.Enqueuer,
		Pool:     deps.Pool, RecoveryGrace: deps.RecoveryGrace,
	})
	riveradapter.Register(workers, &DeleteReconcileWorker{
		Pool:              deps.Pool,
		MediaDir:          deps.MediaDir,
		TrashRetention:    deps.Cleanup.TrashRetention,
		OrphanMTimeGrace:  deps.Cleanup.OrphanMTimeGrace,
		OrphanAge:         deps.Cleanup.OrphanAge,
		MaxDeletesPerPass: deps.Cleanup.MaxDeletesPerPass,
		MissingAssetAge:   deps.Cleanup.MissingAssetAge,
		Webhook:           deps.Webhook,
	})
	riveradapter.Register(workers, &StorageSyncWorker{
		Pool:       deps.Pool,
		MediaDir:   deps.MediaDir,
		ScratchDir: deps.ScratchDir,
	})
	riveradapter.Register(workers, &tsscan.ScanWorker{Pool: deps.Pool, MediaDir: deps.MediaDir})
	riveradapter.Register(workers, &tsscan.ReconcileWorker{Enqueuer: deps.Enqueuer, Pool: deps.Pool})
	return workers
}
