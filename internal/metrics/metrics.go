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

// encode（M3-3）のメトリクス。
var (
	// EncodeDuration は encode 1 件の所要。録画長とコーデックで決まるため
	// バケットは秒〜数時間をカバーする。
	EncodeDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "rokuban_encode_duration_seconds",
		Help:    "Duration of encode jobs.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200},
	})

	// EncodeJobs は encode ジョブの結果別の件数。
	EncodeJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_encode_jobs_total",
		Help: "Encode jobs by result.",
	}, []string{"result"})
)

// thumbnail（M3-4）のメトリクス。
var (
	// ThumbnailDuration は thumbnail 1 件の所要（ffmpeg 抽出 + コピー + DB コミット）。
	ThumbnailDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "rokuban_thumbnail_duration_seconds",
		Help:    "Duration of thumbnail jobs.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 15, 30, 60, 120},
	})

	// ThumbnailJobs は thumbnail ジョブの結果別の件数。
	ThumbnailJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_thumbnail_jobs_total",
		Help: "Thumbnail jobs by result.",
	}, []string{"result"})
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

	// ReconcileCircuitBreakerTrips は reconciler のブレーカーが作動した回数。
	//
	// **M2-5 で意味が変わった**（メトリクス名は既存のダッシュボード・アラートを
	// 壊さないため据え置き）。以前は「1 パスの削除数が閾値を超えた」を数えて
	// いたが、その件数ベースの判定は誤発火しかしないので撤去した。今は
	// 「desired が空なのに自分の schedule が観測される」という全損シグネチャ
	// （breaker.ReconcileTotalLoss）の発動を数える。**件数の閾値ではないので、
	// 加算されたら本当に異常である。**
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

	// ReconcileStartDelayed は開始遅延検出器（issue #24 M2-7、docs/recording.md
	// §3.3「開始遅延検出器」）が直近のパスで検出した件数 --- 「開始時刻 + 猶予
	// （StartDelayGrace）を過ぎたのに recordings.started_at が観測されていない
	// 予約」の数。録画開始は mirakc に全面委譲済みで Rokuban 側から防ぐ手段は
	// ないが、EPGStation#724（チューナー再接続ハングで開始が 10 分遅延）の
	// ような mirakc 側の未知の不具合への保険として検出する。
	//
	// ReconcilePendingDiff と同じ理由でカウンタではなくゲージにする:
	// 「いま何件遅延しているか」が知りたい情報で、mirakc 側の遅延が解消して
	// recording.started が観測されれば次のパスでゼロに戻る。ゼロに戻らない
	// まま続くのは異常が解消していないということで、アラートすべきはゲージ側。
	//
	// site ラベルを持つのは reconciler がサイト単位のジョブで、複数サイトが
	// 並行してパスを走らせうるため（CircuitBreakerTripped と同じ理由）。
	ReconcileStartDelayed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_reconcile_start_delayed",
		Help: "Reservations whose start time plus grace has passed without an observed recording.started. Converges to zero when healthy.",
	}, []string{"site"})
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

	// RulerReservations は ruler が作成/更新/削除した予約の件数。
	// action は created / updated / deleted / released / gc の 5 値。
	// created/updated は差分書き込みで実際に行が変わった件数のみを数える
	// （変化のない行は IS DISTINCT FROM に弾かれ、ここにも計上されない）。
	// deleted は大量削除サーキットブレーカーを通った導出削除、released は
	// ブレーカーを通っていない削除（ユーザーが投資を手放す書き込みをしない限り
	// 起きないもの。docs/recording.md §3.2）で、混ぜると「閾値を下回る導出削除が
	// 素通りしていないか」を deleted の増え方で見る運用が汚れる。gc は番組終了後の
	// GC（ブレーカーの対象外）。
	//
	// `ruler.retract_grace`（issue #428）で削除を見送った件数はここに含めない ---
	// 他の 5 値と違い「行が 1 回寄与するエッジ」ではなく、猶予で残っている間は
	// 毎パス（既定 10 分）再計上される「水準」なので、カウンタの increase() に
	// 乗せると値がパス頻度に比例してしまい録れた予約の数を意味しない。ブレーカーの
	// ラッチと見分ける目的は internal/ruler の `ruler: pass complete` ログの
	// grace_protected フィールドが果たす。
	RulerReservations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_ruler_reservations_total",
		Help: "Reservations created, updated, or deleted by the ruler. " +
			"action: created / updated / deleted (derived deletes that passed the bulk-delete circuit breaker) / " +
			"released (deletes outside the breaker; they require an explicit write that drops the user's investment) / gc.",
	}, []string{"action"})

	// RulerCircuitBreakerTrips は大量削除サーキットブレーカーが**発動に遷移した**
	// 回数（M2-5）。
	//
	// M1-4 では「閾値を超えたパスの数」だったが、ラッチになった以降は遷移だけを
	// 数える。EPG が壊れ続ける間ずっと加算されると rate() が繰り返しの
	// インシデントに見えてしまい、1 件の障害が長引いているのか何度も起きて
	// いるのかを区別できなくなる。**いま止まっているか**は
	// CircuitBreakerTripped ゲージが答える。
	RulerCircuitBreakerTrips = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_ruler_circuit_breaker_trips_total",
		Help: "Times the bulk-delete circuit breaker transitioned into the tripped state for a ruler site. Use rokuban_circuit_breaker_tripped to see whether it is currently latched.",
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

// チューナー射影と容量超過（M2-10、issue #21 / docs/data.md §6.5）のメトリクス。
var (
	// TunersProjected は直近の同期で投影したチューナー本数。
	// EpgProgramsProjected と同じ位置づけ（射影が空になっていないかを見る）。
	TunersProjected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_tuners_projected",
		Help: "Tuners projected by the most recent tuner sync pass.",
	}, []string{"site"})

	// TunerSyncLastSuccess は最後に成功したチューナー全量同期の時刻（UNIX 秒）。
	// EpgSyncLastSuccess と同じ理由（他のゲージは値が凍結するので、これがないと
	// 「同期が止まっている」を検知できない）で持つ。
	TunerSyncLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_tuner_sync_last_success_timestamp_seconds",
		Help: "Unix time of the last successful full tuner sync. Use with time() to detect a stalled sync job.",
	}, []string{"site"})

	// CapacityOverages はチューナーが不足している区間の数（結合済み、地平線全体）。
	//
	// **非ゼロは信頼できるが、ゼロは「大丈夫」を意味しない。** 主張は下界に限る
	// （見えない消費者と excluded_channels により、既知の盲点はすべて「警告を
	// 見逃す」方向に偏っている。docs/data.md §6.5）。したがってこれは
	// 「ゼロに収束すべき異常」ではなく**構成の余裕を眺めるゲージ**であり、
	// 非ゼロだからといって録画が失敗するとは限らない（勝敗を決めるのは mirakc）。
	//
	// site ラベルを持つのは判定がサイトごとに独立だから（N 予約の決定により
	// 二部グラフがサイトごとに非連結になる。docs/data.md §6.5）。
	// tuner_sync パスの完了時に、そのサイト分だけを再計算して入れ直す。
	CapacityOverages = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_capacity_overages",
		Help: "Merged intervals where tuner capacity is exceeded. Non-zero is trustworthy; zero is not a guarantee that everything fits.",
	}, []string{"site"})
)

// 削除 reconcile（M3-8、docs/storage.md §7）のメトリクス。
var (
	// DeleteReconcileDeleted は物理削除したアセット件数。source は
	// trash / until_encoded / orphan / pending（前パスの deleting 再開）。
	DeleteReconcileDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_delete_reconcile_deleted_total",
		Help: "Physically deleted assets by source.",
	}, []string{"source"})

	// DeleteReconcileBytes は物理削除で解放したバイト数。
	DeleteReconcileBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_delete_reconcile_bytes_total",
		Help: "Bytes freed by physical deletion, by source.",
	}, []string{"source"})

	// DeleteReconcileLastPass は最後に成功した削除 reconcile パスの時刻（UNIX 秒）。
	// EpgSyncLastSuccess と同じ理由（ゲージの凍結対策）で持つ。
	DeleteReconcileLastPass = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_delete_reconcile_last_pass_timestamp_seconds",
		Help: "Unix time of the last successful delete-reconcile pass. Use with time() to detect a stalled pass.",
	})

	// MediaAssetsMissing は state='active' なのに実体ファイルが無いと確認できた
	// media_asset の件数（kind 別。issue #343）。「確認できた」は
	// missing_media_assets の first_seen が MissingAssetAge を超えて連続して
	// いること --- 単発の観測揺れでは増減しない。**ゼロは「大丈夫」の証明では
	// ない** --- ファイルシステムの走査が 1 件もファイルを観測しなかったパス
	// （マウント失敗・空マウントの疑い）は DeleteReconcileWorker.Work がこの
	// パスの報告呼び出し自体（reportAgedMissingAssets、このゲージの Reset を
	// 含む）を丸ごとスキップするため、その間はこのゲージが更新されず前回値の
	// まま凍結する（下記 MissingAssetScanSuspectedStorageFailure と対で見る。
	// TestDeleteReconcileWorker_MissingAsset_SuspectedMountFailure_FreezesGauge
	// で実測）。EncodeReconcileUnsatisfiable と同じパターンで、該当 0 件の
	// kind はラベルの系列自体が消える（0 を出さない）。
	MediaAssetsMissing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_media_assets_missing",
		Help: "Active media_assets rows confirmed to have no file on disk, by kind. Not a deletion candidate list — file-missing is necessary but not sufficient for deletion.",
	}, []string{"kind"})

	// MissingAssetScanSuspectedStorageFailure は上記の検出パス自体が
	// 「ファイルシステム走査が 1 件も観測しなかったのに active な
	// media_assets が存在する」という形（全損シグネチャと同種、件数の閾値
	// ではない）でスキップされた回数。**このカウンタが進んでいるパスでは
	// MediaAssetsMissing の更新（reportAgedMissingAssets の呼び出し自体）が
	// 止まる**ため、増加を検出したら実際のマウント状態を確認する
	// （TestDeleteReconcileWorker_MissingAsset_SuspectedMountFailure_FreezesGauge
	// で実測）。
	MissingAssetScanSuspectedStorageFailure = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_missing_asset_scan_suspected_storage_failure_total",
		Help: "Times the missing-asset scan saw zero files on disk while active media_assets rows exist, and skipped reporting for that pass (suspected mount failure or empty mount).",
	})
)

// encode の desired−observed 定期 reconcile（internal/worker/encode_reconcile.go）の
// メトリクス。
//
// このパスは「ヒントを落とした録画が黙ってエンコードされない」を塞ぐバックストップ
// なので、**パス自身が止まったこと・見切れたことも黙って起きうる**。2 つのゲージは
// その 2 通りの黙り方に 1 対 1 で対応する。
var (
	// EncodeReconcileLastPass は最後に完走した encode reconcile パスの時刻（UNIX 秒）。
	// DeleteReconcileLastPass と同じ理由（ゲージの凍結対策）で持つ。
	EncodeReconcileLastPass = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_encode_reconcile_last_pass_timestamp_seconds",
		Help: "Unix time of the last completed encode-reconcile pass. Use with time() to detect a stalled pass.",
	})

	// EncodeReconcileCandidates は直近のパスが見た候補件数（desired なのに
	// active な encoded が無い録画）。
	//
	// **上限（encodeReconcileRowLimit）に張り付いている = バックログが上限以上
	// ある**ことを意味する（候補は recording_id 昇順で切られるため）。窓は
	// パスをまたいで回る（EncodeReconcileWorker.resumeAfter。doc コメント
	// 「窓を回す」参照）ので、張り付いていても到達性は失われない --- 次パスは
	// 続きから見る。
	//
	// 窓が埋まっているバックログのある系では、末尾を越えたパスでこのゲージが
	// 上限を下回る（候補数 |S| ・上限 L に対して `|S| mod L`）。**0 まで落ちるのは
	// |S| が L のちょうど倍数のときだけ**（例: |S|=3, L=2 なら 6 パスの値は
	// [2 1 2 1 2 1] で 0 には一度もならない。TestEncodeReconcileWorker_
	// WindowRotatesPastStuckCandidates は L=1 で回しているため `|S| mod L` が
	// 恒等的に 0 になる特殊ケースしか観測していない）。窓が継続して埋まって
	// いるかを見たいなら `max_over_time` で瞬間的な下振れを均す
	// （docs/operations/monitoring.md）。エンコード実行中の録画も候補に数えるので、
	// 0 でないこと自体は異常ではない。
	EncodeReconcileCandidates = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_encode_reconcile_candidates",
		Help: "Recordings seen by the last encode-reconcile pass that still lack an active encoded asset. Sitting at the pass row limit means the backlog is at least that large, not that recordings beyond it are unreachable (the window resumes from where the previous pass stopped). For a stable backlog, this gauge periodically dips below the row limit as the window wraps around (to backlog size mod row limit, which is 0 only when the backlog is an exact multiple); use max_over_time to see whether the window remains full.",
	})

	// EncodeReconcileUnsatisfiable は「凍結済みの desired が現在の
	// encode.profiles に存在しない」ために、このパスが投入対象から外している
	// 録画数（プロファイル名別）。
	//
	// プロファイルを改名 / 削除すると、その名前で凍結済みの過去録画が一斉に
	// ここへ落ちる。投入しても EncodeWorker が `unknown encode profile` で
	// 弾く（encode.go）ため投入しないのが正しいが、**黙って落とすと
	// 「エンコードされない録画」が静かに増える**ので数えて見せる。
	EncodeReconcileUnsatisfiable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_encode_reconcile_unsatisfiable",
		Help: "Recordings whose frozen encode profile no longer exists in encode.profiles, by profile name. Non-zero means a rename/removal left past recordings unencodable.",
	}, []string{"profile"})
)

// ストレージ観測（issue #238 M7-5）のメトリクス。
//
// StorageSyncLastSuccess はジョブ全体（1 パス）の完走を見るゲージで、
// **root ごとの健全性は見分けられない**。「media は何日も観測できていないが
// scratch は健全」という部分故障（PR #258 のレビューで指摘）では、
// StorageSyncLastSuccess は scratch が成功するたびに現在時刻へ進み続けるため、
// このゲージ単体では検知できない --- 根本原因（存在理由そのものである
// アーカイブの容量）を見失う。そのための root 別シグナルが
// StorageRootLastSuccess / StorageRootTotalBytes / StorageRootUsedBytes /
// StorageRootAvailableBytes の 4 つ。「観測が止まっている」と「statfs が
// 失敗し続けている」をログ無しで区別できるのはこの 4 つの組であり、
// StorageSyncLastSuccess 単体ではできない。
var (
	// StorageSyncLastSuccess は最後に**全 root を観測できた**パスの時刻（UNIX 秒）。
	// 1 root でも失敗したパスでは進めない（部分成功を「成功」に数えない。
	// internal/worker/storage.go の StorageSyncWorker.Work 参照）。
	// site ラベルは持たない --- アーカイブ/スクラッチは単一で site に従属しない
	// （StorageSyncArgs のコメント参照）。
	StorageSyncLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_storage_sync_last_success_timestamp_seconds",
		Help: "Unix time of the last storage sync pass where every configured root was observed successfully. Use with time() to detect a degraded sync; check rokuban_storage_root_last_success_timestamp_seconds{root} for which root is stale.",
	})

	// StorageRootLastSuccess は root ごとに最後に観測できた時刻（UNIX 秒）。
	// 統計に失敗した root は更新しない（前回の値のまま残る）ので、
	// time() との差分がその root だけ伸び続けることで壊れた root を特定できる。
	StorageRootLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_storage_root_last_success_timestamp_seconds",
		Help: "Unix time of the last successful statfs observation for this root. Use with time() to detect a specific root that has stopped being observed.",
	}, []string{"root"})

	// StorageRootTotalBytes / StorageRootUsedBytes / StorageRootAvailableBytes は
	// root ごとの直近の観測バイト数。GET /api/storage を経由せずに Prometheus 側で
	// 直接容量アラートを組めるようにするための対（PR #258 のレビュー指摘）。
	// 統計に失敗した root は更新しない（前回値のまま。StorageRootLastSuccess と
	// 同じ「沈黙は保証ではない」姿勢 --- 値が古くなっていることは
	// StorageRootLastSuccess の鮮度で判別する）。
	StorageRootTotalBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_storage_total_bytes",
		Help: "Total filesystem capacity of this storage root, as of the last successful observation.",
	}, []string{"root"})
	StorageRootUsedBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_storage_used_bytes",
		Help: "Used bytes of this storage root (total - free, counting the root-reserved region as used), as of the last successful observation.",
	}, []string{"root"})
	StorageRootAvailableBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rokuban_storage_available_bytes",
		Help: "Bytes an unprivileged process can actually write to this storage root (statfs Bavail), as of the last successful observation.",
	}, []string{"root"})
)

// ライブ視聴（HLS streamer、issue #91）のメトリクス。
var (
	// LiveActiveSessions はこのプロセスが現在持っているライブセッション（≒ ffmpeg
	// プロセス）数。
	//
	// **per-process gauge。** グローバルな天井はチューナー数で裁定者は mirakc
	// であり、この値を全体像として読む UI を作らない（docs/operations.md §5
	// 「既定を 1 にする根拠と、増やす判定基準」）。全体を見たいときは Prometheus 側で
	// sum する。
	LiveActiveSessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_live_active_sessions",
		Help: "Live-viewing sessions (ffmpeg processes) currently held by this process. Per-process; sum across replicas in Prometheus for the whole picture.",
	})

	// LiveSessionStartFailures はライブセッションの開始に失敗した回数の理由別件数。
	//
	// reason:
	//   - "session_limit": このプロセスの同時セッション上限（live.max_sessions、
	//     プロセスローカル）に達していた
	//   - "upstream_error": mirakc への stream 要求が失敗した（チューナー枯渇を含む）
	//   - "ffmpeg_error": ffmpeg の起動に失敗した
	LiveSessionStartFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_live_session_start_failures_total",
		Help: "Live-viewing session start failures by reason (session_limit, upstream_error, ffmpeg_error).",
	}, []string{"reason"})

	// LiveIdleGCReclaimed は idle GC が回収した（クライアントが離れて ffmpeg を
	// 止めた）ライブセッションの累計件数。
	LiveIdleGCReclaimed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rokuban_live_idle_gc_reclaimed_total",
		Help: "Live-viewing sessions stopped by the idle GC because no segment request arrived within the idle timeout.",
	})

	// LiveLeaveHints は離脱ヒント（POST .../live/leave）の受信数。
	//
	// **LiveIdleGCReclaimed と対で読む。** ヒントは停止命令ではなく idle 期限を
	// 詰めるだけなので、「ヒントを受けた数」と「実際に回収した数」は一致しない
	// （他に視聴者がいれば次のセグメント要求が期限を戻す）。差が常に 0 に近ければ
	// 「離脱＝即回収」、開いていれば「共有セッションが多い」と読める。
	//
	// result:
	//   - "deadline_shortened": 該当セッションがあり、idle 期限を実際に詰めた
	//   - "no_session": 該当サービスのセッションが無かった（既に回収済み・未開始）。
	//     ヒントは何も起こさない --- セッションを作らないことがこの口の契約
	//   - "no_effect": セッションはあったが期限は動かなかった。**設定上ヒントが
	//     効かない**（猶予 = 3×segment_seconds + 2s が live.idle_timeout 以上）か、
	//     **連打の 2 発目以降**（既に詰めた期限より後ろにしか詰められない）。
	//     前者が定常的に出ているなら「離脱ヒントが効かない設定」を意味する
	LiveLeaveHints = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rokuban_live_leave_hints_total",
		Help: "Live-viewing leave hints received, by result (deadline_shortened, no_session, no_effect). A hint shortens the idle deadline; it never stops a session directly.",
	}, []string{"result"})

	// LiveIdleGCLastPass は最後に完走した idle GC パスの時刻（UNIX 秒）。
	//
	// DeleteReconcileLastPass / EpgSyncLastSuccess と同じ理由（ゲージの凍結対策。
	// docs/operations.md の「ゲージには最後に成功した時刻を対で持つ」規律）。
	// LiveActiveSessions は idle GC ループが止まっても直近の値のまま凍結するため、
	// それだけでは「セッションが本当にゼロになった」のか「GC ループが死んでいて
	// チューナーを解放できていない」のかを区別できない。time() - この値 で
	// idle GC の停止を検出する。
	LiveIdleGCLastPass = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rokuban_live_idle_gc_last_pass_timestamp_seconds",
		Help: "Unix time of the last completed live idle-GC pass. Use with time() to detect a stalled idle GC (which would leave tuners unreleased).",
	})
)

// NewRegistry は Rokuban のメトリクスを登録した registry を返す。
//
// backlog は束縛サイトごとの BacklogCollector（未 ingest record の滞留量を
// scrape のたびに DB から取り直すコレクタ）。1 プロセスが N site を束縛できる
// ため（issue #532）可変長引数にした --- 0 site（中央プロセス）は
// `NewRegistry()`、N site 束縛は束縛サイトの数だけ渡す
// （cmd/rokuban.newBoundBacklogCollectors）。
//
// **nil 要素はスキップする。** これは「具体型 nil を interface に入れると
// 非 nil interface になる」という Go の罠（Register が nil レシーバの
// Describe を呼んで panic する。issue #183 のレビューで実バイナリが起動時
// panic した実例）そのものへの防御ではない --- `== nil` はその形の値を
// 捕まえられない。防御の本体は呼び出し側（newBoundBacklogCollectors）が
// 具体型 nil を一切構築しないこと（束縛サイトの数だけ本物のコレクタを作る
// だけで、"無い" を表すのに nil の *BacklogCollector を使わない）。ここでの
// nil スキップは、テスト等が明示的に `NewRegistry(nil)` を渡す（本物の nil
// interface 値）呼び出し規約を壊さないための後方互換でしかない。
func NewRegistry(backlog ...prometheus.Collector) *prometheus.Registry {
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

		EncodeDuration,
		EncodeJobs,

		ThumbnailDuration,
		ThumbnailJobs,

		CircuitBreakerTripped,

		ReconcilePendingDiff,
		ReconcileSchedules,
		ReconcileCircuitBreakerTrips,
		ReconcileScheduleLost,
		ReconcileLastPass,
		ReconcileStartDelayed,

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

		TunersProjected,
		TunerSyncLastSuccess,
		CapacityOverages,

		DeleteReconcileDeleted,
		DeleteReconcileBytes,
		DeleteReconcileLastPass,
		MediaAssetsMissing,
		MissingAssetScanSuspectedStorageFailure,

		EncodeReconcileLastPass,
		EncodeReconcileCandidates,
		EncodeReconcileUnsatisfiable,

		StorageSyncLastSuccess,
		StorageRootLastSuccess,
		StorageRootTotalBytes,
		StorageRootUsedBytes,
		StorageRootAvailableBytes,

		LiveActiveSessions,
		LiveSessionStartFailures,
		LiveIdleGCReclaimed,
		LiveLeaveHints,
		LiveIdleGCLastPass,
	)

	for _, b := range backlog {
		if b == nil {
			continue
		}
		reg.MustRegister(b)
	}
	return reg
}
