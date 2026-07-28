// Package metrics は Prometheus メトリクスの定義と登録を行う。
//
// 2 種類を使い分ける。
//
//   - **プロセス内カウンタ / ヒストグラム**: そのプロセスで起きた事象を数える。
//     ingest のバイト数や所要、reconcile の差分数、recording.failed の理由別など。
//     再起動でリセットされるが、Prometheus はカウンタのリセットを扱えるので問題ない。
//
//   - **DB を引くゲージ**: scrape のたびに真実を DB から取り直す。未 ingest record の
//     滞留量など「今どうなっているか」を表すもの。プロセス内で積み上げないので、
//     どのロールが scrape されても同じ値が出る（分散構成で worker のメトリクスを
//     取りこぼさない）。「イベントはヒント、真実はテーブル再読」と同じ考え方。
//
// 命名は Prometheus の規約に従う（rokuban_ 接頭辞、基本単位、カウンタは _total）。
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// ingest（M1-5-2）のメトリクス。
var (
	// IngestBytes は ingest が転送したバイト数の累計。
	IngestBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_ingest_bytes_total",
		Help: "Total bytes transferred by ingest jobs.",
	})

	// IngestDuration は ingest 1 件の所要。転送は録画長と回線速度で決まるため
	// バケットは秒〜十数分をカバーする。
	IngestDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "rokuban_ingest_duration_seconds",
		Help:    "Duration of ingest jobs.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800},
	})

	// IngestJobs は ingest ジョブの結果別の件数。
	IngestJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_ingest_jobs_total",
		Help: "Ingest jobs by result.",
	}, []string{"result"})

	// 以下は TS のインラインドロップスキャン（M1-5-1）の観測値。
	// 個々の録画の内訳は drop_stats テーブルにあるので、ここでは全体の趨勢だけを見る。

	// IngestDroppedPackets は continuity counter 不連続の累計。
	IngestDroppedPackets = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_ingest_dropped_packets_total",
		Help: "Total TS packets detected as dropped during ingest.",
	})

	// IngestErrorPackets は transport_error_indicator が立ったパケットの累計。
	IngestErrorPackets = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_ingest_error_packets_total",
		Help: "Total TS packets with the transport error indicator set.",
	})

	// IngestScrambledPackets はスクランブルされたままのパケットの累計。
	// 復号が正常なら常に 0 で、0 以外は放送品質ではなくエッジ環境の異常
	// （B-CAS カード接触不良・pcscd 死亡・decode-filter 設定漏れ）を意味する。
	// ドロップとは別枠のアラート対象（docs/recording.md）。
	IngestScrambledPackets = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_ingest_scrambled_packets_total",
		Help: "Total TS packets still scrambled after decoding. Non-zero indicates a B-CAS or decode-filter problem, not broadcast quality.",
	})
)

// 大量削除サーキットブレーカー（M2-5）のメトリクス。
//
// 既存の *_circuit_breaker_trips_total（カウンタ）は「何回発動したか」を数えるが、
// **いま止まっているか**は答えられない。ブレーカーは手動で再開するまで止まり
// 続けるラッチなので（internal/breaker のコメント）、そちらを見るゲージを持つ。
var (
	// CircuitBreakerTripped は各ブレーカーが発動中かどうか（1 = 発動中）。
	//
	// **1 が続いている間、導出削除は一切実行されない**ので、これは
	// 「reconcile が収束できていない」ではなく「人間の確認を待っている」を
	// 意味する。放置すると mirakc 側に不要な schedule が残り続けるため、
	// 1 になったら（時間ではなく即座に）通知する対象。
	//
	// プロセス再起動でゲージは失われるので、各パスの先頭で DB の真実に
	// 合わせ直す（breaker.ObserveState）。
	CircuitBreakerTripped = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_circuit_breaker_tripped",
		Help: "1 while a bulk-delete circuit breaker is latched and withholding deletes until manually resumed.",
	}, []string{"site", "breaker"})
)

// reconciler（M1-4）のメトリクス。
var (
	// ReconcilePendingDiff は直近のパスで検出した desired と observed の差分数。
	//
	// カウンタ（ReconcileSchedules）と違い、収束すればゼロに戻る。
	// ゼロに戻らないまま続くのは reconcile が収束できていないということで
	// （mirakc が作成を拒否し続ける、サーキットブレーカーが削除を止めている等）、
	// アラートすべきはこちら。実行した件数ではなく検出した件数を入れる。
	//
	// action:
	//   - "create" / "delete": 存在の差分
	//   - "update": 予約オプション（priority / tag）が観測結果と食い違っていて、
	//     かつ**このパスで反映しようとした**schedule の数。MaxRecreatesPerPass で
	//     次パスに持ち越した分は含む（数パスで収束するので、持続すれば上限が
	//     低すぎるというアラートすべき情報になる）
	//   - "update_deferred": 差分はあるが state の allowlist で**意図的に触らな
	//     かった**schedule の数。"update" と分けているのは、こちらが収束を待つ
	//     性質のものではないため — 録画中の番組の priority を変えると、録画が
	//     終わるまで（数時間）ずっと差分が残る。これを "update" に混ぜると
	//     「ゼロに戻らない = 異常」というこのゲージの読み方が壊れ、正常な
	//     ユーザー操作でアラートが鳴る。こちらは非ゼロが正常でありうる
	ReconcilePendingDiff = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_reconcile_pending_diff",
		Help: "Differences between desired reservations and observed mirakc schedules found in the most recent pass. Converges to zero when healthy.",
	}, []string{"action"})

	// ReconcileSchedules は mirakc schedule に対する操作の件数。
	// 実際に差分を消した量。action="recreated" は予約オプション（priority /
	// tag）の差分反映のための DELETE→POST 再作成が成功した件数
	// （docs/recording.md §3.2、issue #19）。
	ReconcileSchedules = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_reconcile_schedules_total",
		Help: "mirakc recording schedules created, deleted, or recreated by the reconciler.",
	}, []string{"action"})

	// ReconcileCircuitBreakerTrips は大量削除サーキットブレーカーが作動した回数。
	// 0 以外はアラート対象（1 パスの削除が閾値を超えて停止している）。
	//
	// 予約オプションの差分反映（再作成）の DELETE はこのブレーカーの対象では
	// ない（MaxRecreatesPerPass という別のレート制限を持つ。ブレーカーは
	// 「ルール x EPG から導出された削除」の暴走を止めるためのもので、優先度の
	// 一括変更のような正当な再作成をここに混ぜると誤作動する。
	// docs/recording.md §3.2「大量削除サーキットブレーカー」参照）。
	ReconcileCircuitBreakerTrips = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_reconcile_circuit_breaker_trips_total",
		Help: "Times the bulk-delete circuit breaker stopped a reconcile pass.",
	})

	// ReconcileScheduleLost は予約オプションの差分反映（再作成）で DELETE には
	// 成功したが直後の POST が失敗し、schedule が消えたまま残った回数。
	//
	// レベルトリガーにより次パスが再作成を試みるが、その間に番組の開始時刻を
	// 過ぎると取りこぼす。docs/recording.md §3.2 は quality_events への記録を
	// 想定しているが、quality_events は recordings 行に紐づく列であり、
	// まだ開始していない番組には recordings 行が存在しないため書き込めない
	// （internal/reconciler.Reconciler.recreateSchedule のコメント参照）。
	// 0 以外はアラート対象。
	ReconcileScheduleLost = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_reconcile_schedule_lost_total",
		Help: "Times a schedule recreate's DELETE succeeded but the following POST failed, leaving the schedule missing until the next pass. Non-zero indicates a recording may be missed before then.",
	})

	// ReconcileLastPass は最後に完走したパスの時刻（UNIX 秒）。
	//
	// ゲージは値が凍結するので、ReconcilePendingDiff だけでは
	// 「収束した」と「reconciler が動いていない」を区別できない。
	// シングルトンのロックを取れていない・ループが死んでいる場合を
	// time() - この値 で検出する。
	ReconcileLastPass = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_reconcile_last_pass_timestamp_seconds",
		Help: "Unix time of the last completed reconcile pass. Use with time() to detect a stalled reconciler.",
	})
)

// ruler（M2-3）のメトリクス。
var (
	// RulerPassDuration は 1 パス（全サイト分）の所要。ルール数 x 番組数の
	// 全量評価だが pg_trgm GIN 込みで秒未満に収まる想定（docs/recording.md §3.1）。
	RulerPassDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "rokuban_ruler_pass_duration_seconds",
		Help:    "Duration of a full rule-evaluation pass across all sites.",
		Buckets: []float64{0.05, 0.1, 0.5, 1, 2, 5, 10, 30, 60},
	})

	// RulerReservations はルールが作成/更新/削除した予約の件数。
	// created/updated は差分書き込みで実際に行が変わった件数のみを数える
	// （変化のない行は IS DISTINCT FROM に弾かれ、ここにも計上されない）。
	RulerReservations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_ruler_reservations_total",
		Help: "Reservations created, updated, or deleted by the ruler.",
	}, []string{"action"})

	// RulerCircuitBreakerTrips は大量削除サーキットブレーカーが作動した回数
	// （サイト x パスごとに 1 回まで）。0 以外はアラート対象。
	RulerCircuitBreakerTrips = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_ruler_circuit_breaker_trips_total",
		Help: "Times the bulk-delete circuit breaker stopped a ruler pass for a site.",
	})

	// RulerLastPass は最後に（全サイトとも）成功したパスの時刻（UNIX 秒）。
	// reconciler.ReconcileLastPass と同じ理由でゲージの凍結対策として持つ。
	RulerLastPass = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_ruler_last_pass_timestamp_seconds",
		Help: "Unix time of the last successful ruler pass. Use with time() to detect a stalled ruler.",
	})
)

// watcher（M1-3）のメトリクス。
var (
	// RecordingsFailed は録画失敗の理由別件数。
	// reason は mirakc の FailedReason.type（need-rescheduling / io-error 等）で
	// 値域は有界なのでラベルに使える。
	RecordingsFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_recordings_failed_total",
		Help: "Failed recordings by mirakc failure reason.",
	}, []string{"reason"})

	// RecordsBroken は録画中の異常（record-broken）の理由別件数。
	// 同じ録画で複数回発生しうる。
	RecordsBroken = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_records_broken_total",
		Help: "mirakc record-broken events by reason.",
	}, []string{"reason"})

	// SweepLastPass は最後に成功した record_sweep パス（3 段構えの (c)、
	// docs/recording.md §3.3）の時刻（UNIX 秒）。ReconcileLastPass / RulerLastPass /
	// EpgSyncLastSuccess と同じ理由（ゲージの凍結対策）で持つ（M2-18）。
	SweepLastPass = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_sweep_last_pass_timestamp_seconds",
		Help: "Unix time of the last successful record_sweep pass. Use with time() to detect a stalled sweep.",
	})
)

// EPG プロジェクション（M1-6）のメトリクス。
var (
	// EpgSyncDuration は EPG 全量同期 1 パスの所要。
	EpgSyncDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "rokuban_epg_sync_duration_seconds",
		Help:    "Duration of a full EPG projection sync pass.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 300},
	})

	// EpgProgramsProjected は直近の同期で投影した番組数。
	EpgProgramsProjected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_epg_programs_projected",
		Help: "Programs projected by the most recent EPG sync pass.",
	})

	// EpgChannelsWithoutPrograms は直近の同期で番組を返さなかったチャンネル数。
	// 0 以外が続くなら特定チャンネルの EPG 収集が慢性的に失敗している。
	EpgChannelsWithoutPrograms = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_epg_channels_without_programs",
		Help: "Channels that returned no programs in the most recent EPG sync pass.",
	})

	// EpgSyncLastSuccess は最後に成功した全量同期の時刻（UNIX 秒）。
	//
	// 定期ジョブが投入されなくなっても他のゲージは最後の値のまま残るため、
	// これがないと「同期が止まっている」ことを検知できない。
	// 実際に UniqueOpts の設定ミスで定期ジョブがワンショット化していた事故があり、
	// このメトリクスがあれば気づけた。
	EpgSyncLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_epg_sync_last_success_timestamp_seconds",
		Help: "Unix time of the last successful full EPG sync. Use with time() to detect a stalled sync job.",
	})
)

// NewRegistry は Rokuban のメトリクスを登録した registry を返す。
//
// backlog が非 nil なら、未 ingest record の滞留量を scrape のたびに DB から
// 取り直すコレクタも登録する。
func NewRegistry(backlog prometheus.Collector) *prometheus.Registry {
	reg := prometheus.NewRegistry()

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),

		IngestBytes,
		IngestDuration,
		IngestJobs,
		IngestDroppedPackets,
		IngestErrorPackets,
		IngestScrambledPackets,

		CircuitBreakerTripped,

		ReconcilePendingDiff,
		ReconcileSchedules,
		ReconcileCircuitBreakerTrips,
		ReconcileScheduleLost,
		ReconcileLastPass,

		RulerPassDuration,
		RulerReservations,
		RulerCircuitBreakerTrips,
		RulerLastPass,

		RecordingsFailed,
		RecordsBroken,
		SweepLastPass,

		EpgSyncDuration,
		EpgProgramsProjected,
		EpgChannelsWithoutPrograms,
		EpgSyncLastSuccess,
	)

	if backlog != nil {
		reg.MustRegister(backlog)
	}
	return reg
}
