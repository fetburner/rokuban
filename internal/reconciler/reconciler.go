// Package reconciler は予約（desired）と mirakc の schedules（observed）の差分を
// POST/DELETE で消す 1 パス評価ロジック。
//
// reconciler はシングルトンではなく River のジョブ（internal/worker の
// ReconcilePassWorker）として実行される。周期的・冪等・パスを跨ぐ状態を持たない
// （サーキットブレーカーの発動状態も含め毎パス DB と mirakc から読み直す。発動の
// ラッチ自体は internal/breaker が circuit_breakers に持続させる）という性質が
// ruler / epg_sync と同じで、排他は advisory lock ではなくジョブロック +
// UniqueOpts（サイト単位）で担保する（docs/data.md §2、issue #24 M2-17）。
// このパッケージは 1 パス分のロジックだけを持ち、いつ・どの契機で呼ぶか
// （定期実行の起動契機はデプロイ形態に委ねる）は呼び出し側の責務。
package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// Config は Reconciler の設定。
type Config struct {
	// MaxRecreatesPerPass は 1 パスで行う予約オプション差分反映の再作成
	// （DELETE→POST）の上限。ルールの priority を編集するとマッチしている
	// 全予約が再作成対象になり得るため（N=200 なら 1 パスで 400 回の mirakc
	// 呼び出し）、上限を設けてレベルトリガーで数パスに分けて収束させる。
	//
	// かつて存在した MaxDeletesPerPass（件数ベースの大量削除ブレーカー）とは
	// 別物で、あちらは撤去した: reconciler が「desired に無い schedule がある」
	// と判定する経路（ruler の導出削除／ユーザーの明示操作／番組終了後の GC）を
	// reconciler 自身は区別できず、対象外と定められた後の 2 経路で誤発火する
	// だけだったため（breaker.ReconcileTotalLoss の doc コメント、
	// docs/recording.md §3.2、issue #2 の M2-5 決定コメント）。代わりに
	// 「desired が空なのに自分の schedule が観測される」という全損シグネチャを
	// breaker.ReconcileTotalLoss で守る。MaxRecreatesPerPass は削除の話ではなく
	// 単なるレート制限で、上限まではやって残りを次パスに送るだけ。
	MaxRecreatesPerPass int

	DefaultPriority int

	// StartDelayGrace は開始遅延検出器（issue #24 M2-7、docs/recording.md §3.3
	// 「開始遅延検出器」）の猶予。「開始時刻 + StartDelayGrace」を過ぎても
	// recordings.started_at が観測されない予約を検出対象にする。
	//
	// 猶予が要る理由: 番組開始の直後は mirakc の SSE イベント到達と watcher の
	// 処理（GetRecord → recordings 作成/更新のコミット）に遅れがある。猶予が
	// ゼロだと、正常に開始した録画でも「開始時刻ちょうどにはまだ started_at が
	// 埋まっていない」を毎回検出してしまい、アラートが常時誤発火する。
	// EPGStation#724（チューナー再接続ハングで開始が 10 分遅延）のような
	// 実際の異常と区別するには、正常系の遅れを上回る猶予が要る。
	// 0 なら既定値（3 分）を使う。
	StartDelayGrace time.Duration
}

func defaultConfig() Config {
	return Config{
		MaxRecreatesPerPass: 20,
		DefaultPriority:     10,
		StartDelayGrace:     3 * time.Minute,
	}
}

// Reconciler は予約の desired state と mirakc の observed state を突き合わせる
// 1 パス評価を行う。
type Reconciler struct {
	site   string
	mirakc *mirakc.Client
	pool   *pgxpool.Pool
	cfg    Config
}

// New は Reconciler を生成する。cfg が nil の場合はデフォルト設定を使う。
//
// 呼び出し元の internal/worker.ReconcilePassWorker はジョブ引数のサイト 1 つに
// 対して 1 個の Reconciler を作る（ジョブの排他がサイト単位のため）。
func New(site string, mc *mirakc.Client, pool *pgxpool.Pool, cfg *Config) *Reconciler {
	c := defaultConfig()
	if cfg != nil {
		if cfg.MaxRecreatesPerPass > 0 {
			c.MaxRecreatesPerPass = cfg.MaxRecreatesPerPass
		}
		if cfg.DefaultPriority > 0 {
			c.DefaultPriority = cfg.DefaultPriority
		}
		if cfg.StartDelayGrace > 0 {
			c.StartDelayGrace = cfg.StartDelayGrace
		}
	}
	return &Reconciler{
		site:   site,
		mirakc: mc,
		pool:   pool,
		cfg:    c,
	}
}

// RunPass は 1 サイトに対して突き合わせパスを 1 回実行する。
//
// reconciler はシングルトンではなく River のジョブとして呼ばれる。起動契機は
// 定期実行（真実）・予約の作成/取消・ruler パスの完了（いずれもヒント）の 3 つが
// あるが、すべて RunPass を呼ぶ 1 本の経路に合流する（docs/recording.md §3.2）。
// 削除の大量発動を守るサーキットブレーカー（breaker.ReconcileTotalLoss）は
// パスをまたぐラッチとして circuit_breakers に持続する。RunPass 自身（および
// Reconciler 構造体）は状態を持たないが、パスの先頭で breaker.ObserveState を
// 呼んで DB の真実に合わせ直し、発動中なら schedule の削除を一切実行しない
// （作成・再作成は続ける。止めたいのは削除だけ）。
func (r *Reconciler) RunPass(ctx context.Context) error {
	slog.Debug("reconciler: starting pass")

	q := sqlcgen.New(r.pool)

	// 発動状態を DB の真実に合わせ直す。判定できない場合も安全側に倒して
	// 発動中とみなし削除を止める（記録・確認ができないまま削除を続けるのが
	// 最悪の組み合わせという breaker.Trip のコメントと同じ理由）。
	tripped, err := breaker.ObserveState(ctx, q, r.site, breaker.ReconcileTotalLoss)
	if err != nil {
		slog.Error("reconciler: observing circuit breaker state; withholding deletes to be safe",
			"breaker", breaker.ReconcileTotalLoss, "err", err)
		tripped = true
	}

	schedules, err := r.mirakc.ListSchedules(ctx)
	if err != nil {
		return fmt.Errorf("listing mirakc schedules: %w", err)
	}

	sweepTime := time.Now()

	if err := r.observeSchedules(ctx, schedules); err != nil {
		return fmt.Errorf("observing schedules: %w", err)
	}

	reservations, err := r.listDesired(ctx)
	if err != nil {
		return fmt.Errorf("listing desired reservations: %w", err)
	}

	observedByProgram := make(map[int64]mirakc.Schedule, len(schedules))
	for _, s := range schedules {
		observedByProgram[s.Program.ID] = s
	}

	var created, deleted int
	var missing int
	var endGuarded int

	now := time.Now()
	for _, d := range reservations {
		if _, exists := observedByProgram[d.res.ProgramID]; exists {
			continue
		}
		if programEnded(d.snap, now) {
			// 番組はもう終わっている。POST しても mirakc は
			// need-rescheduling で数秒後に failed にするだけで、recordings に
			// content_length=0 の failed 行が残るだけ（issue #134 の実測:
			// record_sync 46 件中 41 件が failed）。この予約は本パスの
			// recordNeverScheduled が recordings に never-scheduled 行を作る
			// （同じ programEnded 判定を使うので食い違わない）ので、次パス
			// 以降は listDesired（ListReservationsForSyncEvaluation の
			// never-scheduled 除外）から外れて二度と create ループの対象に
			// ならない（issue #98。旧実装は orphaned_at IS NULL で絞っていた）。
			//
			// stateGuarded / limitCarriedOver は複数パスにまたがって残留し
			// うる持ち越しなので ReconcilePendingDiff で監視する価値があるが、
			// これは 1 パス限りで自己解消するのでゲージには混ぜない
			// （ログでの観測で足りると判断した。理由は下の Info ログの
			// コメント参照）。
			endGuarded++
			continue
		}
		missing++
		if err := r.createSchedule(ctx, d); err != nil {
			slog.Error("reconciler: creating schedule", "reservation_id", d.res.ID, "program_id", d.res.ProgramID, "err", err)
			continue
		}
		created++
		metrics.ReconcileSchedules.WithLabelValues("created").Inc()
	}

	desiredPrograms := make(map[int64]struct{}, len(reservations))
	for _, d := range reservations {
		desiredPrograms[d.res.ProgramID] = struct{}{}
	}

	var toDelete []mirakc.Schedule
	for _, s := range schedules {
		if !mirakc.IsOurs(s.Tags) {
			continue
		}
		if _, desired := desiredPrograms[s.Program.ID]; desired {
			continue
		}
		toDelete = append(toDelete, s)
	}

	// 全損シグネチャ: desired（reservations）が 1 件もないのに、自分が作った
	// schedule が観測されている。件数ではなく形で検知する — listDesired が
	// バグ・障害で空を返したときに自分が作った全 schedule を削除してしまう
	// 経路だけを捕まえる。GC・ユーザー操作による正当な一括削除は他の予約が
	// 残るのでここには当たらない（docs/recording.md §3.2、issue #2 の M2-5
	// 決定コメント）。
	totalLoss := len(reservations) == 0 && len(toDelete) > 0
	if totalLoss {
		// threshold に 0 を渡すのは値の欠落ではない。このブレーカーは件数の
		// 閾値を持たない（形で検知する）が、「desired が空のときに許される削除数」
		// はまさに 0 なので、pending = N と threshold = 0 の組は
		// 「0 件しか許されない状況で N 件消そうとした」と読めて正確である。
		if err := breaker.Trip(ctx, q, r.site, breaker.ReconcileTotalLoss, 0, totalLossSample(toDelete)); err != nil {
			slog.Error("reconciler: recording circuit breaker trip", "breaker", breaker.ReconcileTotalLoss, "err", err)
		}
		// 記録が失敗した場合も含め、このパスでは削除を実行しない。
		tripped = true
		metrics.ReconcileCircuitBreakerTrips.Inc()
	}

	if tripped {
		if !totalLoss {
			// 全損シグネチャは今パスでは検出していないが、ラッチは自動では
			// 解けない（手動 ResumeCircuitBreaker のみが解除する）ので削除は
			// 引き続き止める。
			slog.Error("reconciler: circuit breaker latched — withholding schedule deletes until manually resumed",
				"breaker", breaker.ReconcileTotalLoss,
				"pending_deletes", len(toDelete),
			)
		}
	} else {
		for _, s := range toDelete {
			if err := r.mirakc.DeleteSchedule(ctx, s.Program.ID); err != nil {
				slog.Error("reconciler: deleting schedule", "program_id", s.Program.ID, "err", err)
				continue
			}
			deleted++
			metrics.ReconcileSchedules.WithLabelValues("deleted").Inc()
		}
	}

	recreated, updateDiff, stateGuarded, limitCarriedOver := r.recreateChanged(ctx, reservations, observedByProgram)

	if err := q.DeleteStaleScheduleSyncs(ctx, sqlcgen.DeleteStaleScheduleSyncsParams{
		Site:       r.site,
		ObservedAt: sweepTime,
	}); err != nil {
		slog.Error("reconciler: cleaning stale schedule_syncs", "err", err)
	}

	// now は作成ループと同じ瞬間を渡す（同じ式だけでなく同じ材料にする）。
	// 別々に time.Now() を取ると、パス実行中に終了時刻を跨いだ番組が
	// 「作成ループでは未終了 → POST」かつ「recordNeverScheduled では終了 →
	// never-scheduled 行を作成」になり、収束はするが issue #134 が消したい
	// failed 行がちょうど 1 件出る。
	if err := r.recordNeverScheduled(ctx, reservations, schedules, now); err != nil {
		slog.Error("reconciler: recording never-scheduled outcome", "err", err)
	}

	startDelayed, err := r.detectStartDelays(ctx, reservations)
	if err != nil {
		slog.Error("reconciler: detecting start delays", "err", err)
	}
	for _, d := range startDelayed {
		slog.Error("reconciler: recording not started past start time + grace",
			"reservation_id", d.id,
			"program_id", d.programID,
			"title", d.title,
			"elapsed", d.elapsed,
		)
	}
	// DB に新しい状態は持たせない（不変条件 5: レベルトリガー）。毎パス
	// recordings.started_at から再計算する導出値なので、ゲージだけで表す。
	// 収束すれば（recording.started が観測されれば）次のパスでゼロに戻る。
	metrics.ReconcileStartDelayed.WithLabelValues(r.site).Set(float64(len(startDelayed)))

	// 差分そのものをゲージで出す。健全なら次のパスでゼロになる。
	// 作成/削除/再作成できずに残った量が知りたいので、実行した件数ではなく
	// 検出した件数（MaxRecreatesPerPass で持ち越した分も含む）。
	//
	// state の allowlist で意図的に触らなかった分は update ではなく
	// update_deferred に入れる。混ぜると「ゼロに戻らない = 異常」という
	// このゲージの読み方が壊れる（metrics.ReconcilePendingDiff のコメント参照）。
	metrics.ReconcileLastPass.SetToCurrentTime()
	metrics.ReconcilePendingDiff.WithLabelValues("create").Set(float64(missing))
	metrics.ReconcilePendingDiff.WithLabelValues("delete").Set(float64(len(toDelete)))
	metrics.ReconcilePendingDiff.WithLabelValues("update").Set(float64(updateDiff))
	metrics.ReconcilePendingDiff.WithLabelValues("update_deferred").Set(float64(stateGuarded))

	// 持ち越した件数を黙って落とすと「収束しない」原因が見えなくなるので、
	// state ガード・MaxRecreatesPerPass のどちらで持ち越したかを分けて出す。
	if stateGuarded > 0 {
		slog.Info("reconciler: recreate candidates deferred to next pass (schedule state guard)",
			"count", stateGuarded,
		)
	}
	if limitCarriedOver > 0 {
		slog.Info("reconciler: recreate candidates deferred to next pass (MaxRecreatesPerPass)",
			"count", limitCarriedOver,
			"max_recreates_per_pass", r.cfg.MaxRecreatesPerPass,
		)
	}
	// stateGuarded / limitCarriedOver と同じ扱いで、黙って落とさずログに出す。
	// メトリクスは増やさない — 上2つは複数パスにまたがって残留しうる持ち越し
	// （録画中は priority 変更が反映されないまま残る、MaxRecreatesPerPass で
	// 溢れた分が次パスに送られる）だからゲージで監視する価値があるのに対し、
	// endGuarded は本パスの markOrphaned が同じ判定式で orphaned_at を埋める
	// ので、次パスには同じ予約が二度と現れない（1 パスで自己解消する）。
	// ReconcilePendingDiff の「create」ゲージに混ぜると、埋まらない別の理由
	// （mirakc 障害等）と区別できなくなる。
	if endGuarded > 0 {
		slog.Info("reconciler: create candidates skipped (program already ended)",
			"count", endGuarded,
		)
	}

	slog.Info("reconciler: pass complete",
		"desired", len(reservations),
		"observed", len(schedules),
		"missing", missing,
		"created", created,
		"stale", len(toDelete),
		"deleted", deleted,
		"update_diff", updateDiff,
		"recreated", recreated,
		"start_delayed", len(startDelayed),
	)
	return nil
}

// totalLossSample は breaker.ReconcileTotalLoss 発動時の breaker.Sample を組み立てる。
// title は予約行から引けない（desired が空という状況そのものが全損シグネチャの
// 前提なので）。mirakc から今パスで返ってきた Schedule.Program.Name を使う —
// 手動確認する人間が「何が消されようとしていたか」を判別できれば十分で、
// 予約行への問い合わせは全損の状況では成立しない。breaker.Trip 側で
// MaxSampleSize を超えた分は切り詰められる。
func totalLossSample(toDelete []mirakc.Schedule) breaker.Sample {
	sample := breaker.Sample{
		Total:    len(toDelete),
		Programs: make([]breaker.SampleProgram, 0, len(toDelete)),
	}
	for _, s := range toDelete {
		var title string
		if s.Program.Name != nil {
			title = *s.Program.Name
		}
		sample.Programs = append(sample.Programs, breaker.SampleProgram{
			ProgramID: s.Program.ID,
			Title:     title,
		})
	}
	return sample
}

func (r *Reconciler) observeSchedules(ctx context.Context, schedules []mirakc.Schedule) error {
	q := sqlcgen.New(r.pool)
	for _, s := range schedules {
		optionsJSON, err := json.Marshal(s.Options)
		if err != nil {
			return fmt.Errorf("marshalling options: %w", err)
		}

		tags := s.Tags
		if tags == nil {
			tags = []string{}
		}

		var failedReasonJSON json.RawMessage
		if s.FailedReason != nil {
			data, mErr := json.Marshal(s.FailedReason)
			if mErr != nil {
				return fmt.Errorf("marshalling failed_reason: %w", mErr)
			}
			failedReasonJSON = data
		}

		if err := q.UpsertScheduleSync(ctx, sqlcgen.UpsertScheduleSyncParams{
			Site:         r.site,
			ProgramID:    s.Program.ID,
			State:        s.State,
			Options:      optionsJSON,
			Tags:         tags,
			FailedReason: failedReasonJSON,
		}); err != nil {
			return fmt.Errorf("upserting schedule_sync for program %d: %w", s.Program.ID, err)
		}
	}
	return nil
}

// desiredReservation は予約行・番組スナップショットと、そこから解決済みの
// 実効オプション。オプションは base（ruler の導出結果）と program_overrides
// （ユーザーの上書き）・program_intents（ユーザーの record/skip 意図）の合成
// なので、行だけを持ち回さず解決結果と組で扱う。
//
// snap（program_snapshots）を分けて持つのは #27 で番組の事実のスナップショット
// （title / 開始時刻 / 尺 / チャンネル識別）が reservations から抽出された
// ため。res（sqlcgen.Reservation）はもう ruler の 1 パスの導出出力だけを持つ
// （CLAUDE.md 不変条件 12）。
type desiredReservation struct {
	res  sqlcgen.Reservation
	snap sqlcgen.ProgramSnapshot
	opts db.ReservationOptions
}

// listDesired は mirakc への同期対象を返す。
//
// state（#28/#30 で orphaned_at に、issue #98 で recordings の存在に置き換わった
// 導出値）ではなく effective.skip で絞る（docs/schema.md §3「state を『mirakc
// への同期対象か』のフィルタに使ってはならない」、docs/recording.md §4.3）。
// 除外してよいのは「この予約について既に never-scheduled の recordings 行が
// ある」行だけ（番組終了後に schedule が観測されなかったと既に判定済みで、
// 番組が終わっているので schedule を作る意味がない）。
// ListReservationsForSyncEvaluation がこの条件で絞るのはこのため —
// 旧 ListActiveReservationsBySite は state = 'active' でしか絞っておらず、
// detached（「実質 manual として動く」はず）の予約に schedule が作られない
// バグの原因だった: 手動予約 → たまたまルールがマッチ（active, rule_id 付き）
// → そのルールを編集して外す（detached）→ 同期対象から外れる、という経路で
// ユーザーの手動予約が黙って録画されなくなっていた（M2-4 で修正）。
//
// effective.skip の絞り込みは db.EvaluateSyncCandidates に通す
// （internal/db/sync.go）。この関数はここ（reconciler.listDesired）と
// cmd/rokuban/shadowdiff.go の 2 箇所から呼ばれる共通処理で、以前は 2 箇所が
// 別々に db.EffectiveOptions を呼んでおり、shadow-diff 側が絞り込みの移植を
// 忘れたことが issue #54 の見逃しの原因だった。
func (r *Reconciler) listDesired(ctx context.Context) ([]desiredReservation, error) {
	rows, err := sqlcgen.New(r.pool).ListReservationsForSyncEvaluation(ctx, r.site)
	if err != nil {
		return nil, err
	}

	var desired []desiredReservation
	for _, c := range db.EvaluateSyncCandidates(rows) {
		if c.Err != nil {
			// 壊れた jsonb で mirakc に既定値の schedule を作ってしまわないよう、
			// この予約は同期対象から外してアラートする（握りつぶさない）。
			slog.Error("reconciler: resolving effective options", "err", c.Err)
			continue
		}
		if c.Skipped {
			continue
		}
		desired = append(desired, desiredReservation{res: c.Reservation, snap: c.Snapshot, opts: c.Options})
	}
	return desired, nil
}

// programEnded は予約に対応する番組がもう終了しているかどうかを判定する
// （program_snapshots のスナップショットのみが材料 —— 番組は終了後に
// epg_programs から消えうるので EPG 射影は見ない）。RunPass の作成ループと
// markOrphaned の両方から呼ばれる必要があるため、この 1 箇所に抽出してある。
// 同じ式を 2 箇所に書き下すと、片方だけ境界を直してもう片方を直し忘れる
// 事故が起きる（issue #134）。実際、作成ループだけ境界がずれると
// 「作らない」（作成ループ）と「まだ orphaned にしない」（markOrphaned）が
// 食い違い、同じ予約が毎パス作成対象のまま残って mirakc への POST を撃ち
// 続ける —— この抽出前より悪化する組み合わせなので、境界は 1 箇所でしか
// 定義しない。
//
// 終了時刻ちょうど（endTime == now）は「終了していない」側に倒す
// （markOrphaned が元々持っていた `!endTime.Before(now)` を素直に反転した
// だけで、境界を動かしていない）。進行中の番組への POST は止めない —— mirakc
// は放送中の番組の schedule を受け付けて途中から録る（issue #134 の実測:
// 23:10 開始の番組を 23:16:39 に予約しても正常に録画継続、426MB 到達）。
// ガードの対象は「終了済み」だけで、「開始済み」で切ると放送中の番組を
// 予約する経路が黙って壊れる。
func programEnded(snap sqlcgen.ProgramSnapshot, now time.Time) bool {
	endTime := snap.StartAt.Add(time.Duration(snap.DurationMs) * time.Millisecond)
	return endTime.Before(now)
}

// effectivePriority は mirakc に送る priority を決める: opts.Priority が
// あればそれ、なければ defaultPriority。初回作成（createSchedule）と予約
// オプション差分反映の再作成（recreateSchedule）の両方から呼ばれる必要が
// あるため、この 1 箇所に抽出してある。同じ式を 2 箇所に書き下すと、片方だけ
// 直してもう片方を直し忘れる事故が起きる。
func effectivePriority(defaultPriority int, opts db.ReservationOptions) int {
	if opts.Priority != nil {
		return *opts.Priority
	}
	return defaultPriority
}

// resolveContentPath は予約から新規に生成する contentPath を返す。
// 初回作成（createSchedule）から呼ばれるほか、再作成（recreateSchedule）が
// observed の contentPath を引き継げなかった場合（nil・空文字）の
// フォールバック経路としても呼ばれる。
//
// service_id は program_snapshots にスナップショットされた値のみを使う
// （#27 で reservations から抽出済み）。mirakc の programId 内部構造
// （Mirakurun 互換の ID 合成規則）を割り算して推測することはしない
// （不変条件: mirakc 固有の概念を永続テーブルの外で復元しない）。
//
// service_id が無い（推測せず schedule を作らない）という分岐はかつて
// ここにあったが、issue #101（00026）で program_snapshots のチャンネル・
// イベント識別 6 列が NOT NULL 化されたことで、この分岐が守っていた
// 「00009 以前の残骸で service_id が NULL」という状態自体が表現不可能に
// なったため落とした（起きない状態のための分岐を残さない）。
func resolveContentPath(res sqlcgen.Reservation, snap sqlcgen.ProgramSnapshot, opts db.ReservationOptions) (string, error) {
	// contentPath は filenameTemplate（ruler が base に載せたテンプレート、または
	// ユーザーの明示的な上書き）があればそれを展開し、なければ従来の固定形式
	// （buildContentPath 参照）。ContentPath（フルパスの直接指定）が別途あれば
	// そちらが最終的に勝つ — 展開結果よりユーザーの明示指定を優先する
	// （db.ReservationOptions のドキュメントコメント参照）。
	template := ""
	if opts.FilenameTemplate != nil {
		template = *opts.FilenameTemplate
	}
	contentPath, err := buildContentPath(snap, template)
	if err != nil {
		// テンプレートが構文的に正しく見えても実行時にエラーになるケース
		// （通常は api の validateRuleInput が作成時点で弾くので、ここに来るのは
		// ルール作成後にテンプレート仕様が変わった等の想定外の経路）。推測で
		// schedule を作らず、同期対象から外してアラートする。
		return "", fmt.Errorf("building content path for reservation %d (program %d): %w",
			res.ID, res.ProgramID, err)
	}
	if opts.ContentPath != nil && *opts.ContentPath != "" {
		contentPath = contentpath.SanitizeContentPath(*opts.ContentPath)
	}
	return contentPath, nil
}

func (r *Reconciler) createSchedule(ctx context.Context, d desiredReservation) error {
	res, opts := d.res, d.opts

	priority := effectivePriority(r.cfg.DefaultPriority, opts)

	contentPath, err := resolveContentPath(res, d.snap, opts)
	if err != nil {
		return err
	}

	input := mirakc.ScheduleInput{
		ProgramID: res.ProgramID,
		Options: mirakc.Options{
			ContentPath: &contentPath,
			Priority:    priority,
		},
		Tags: []string{mirakc.ProgramTag(res.ProgramID)},
	}

	schedule, err := r.mirakc.CreateSchedule(ctx, input)
	if err != nil {
		return fmt.Errorf("POST schedule: %w", err)
	}

	slog.Info("reconciler: created schedule",
		"reservation_id", res.ID,
		"program_id", res.ProgramID,
		"state", schedule.State,
		"content_path", contentPath,
	)
	return nil
}

// recreateCandidate は再作成の対象になりうる予約と、その observed schedule の組。
type recreateCandidate struct {
	d        desiredReservation
	observed mirakc.Schedule
}

// recreateChanged は effective options（priority）と観測結果（priority /
// reservation tag）の差分を DELETE→POST の再作成で消す。存在の有無ではなく
// 内容の差分なので、呼び出し元の create/delete ループとは独立に走る
// （docs/recording.md §3.2、issue #19）。
//
// 差分対象は priority と reservation tag のみ。contentPath は差分対象にしない
// （EPG の番組名が変わるたびに schedule が消えて作り直される churn になるため。
// recreateSchedule 側で observed の contentPath をそのまま引き継ぐ）。
//
// 戻り値は (recreated, updateDiff, stateGuarded, limitCarriedOver)。
// updateDiff は検出した差分のうち **このパスで反映しようとした** 件数
// （= 全候補から state の allowlist で除外した分を引いたもの。MaxRecreatesPerPass
// で持ち越した分は含む）。ReconcilePendingDiff の "実行した件数ではなく検出した
// 件数" という規約は守るが、state ガードで意図的に触らなかった分は stateGuarded
// として分けて返す — 録画中の番組の priority を変えると録画が終わるまで差分が
// 残り続けるので、これを updateDiff に混ぜると「ゼロに戻らない = 異常」という
// ゲージの読み方が壊れ、正常なユーザー操作でアラートが鳴る
// （metrics.ReconcilePendingDiff のコメント参照）。
func (r *Reconciler) recreateChanged(
	ctx context.Context,
	reservations []desiredReservation,
	observedByProgram map[int64]mirakc.Schedule,
) (recreated, updateDiff, stateGuarded, limitCarriedOver int) {
	var candidates []recreateCandidate
	for _, d := range reservations {
		s, exists := observedByProgram[d.res.ProgramID]
		if !exists {
			// 存在しない = create ループの対象（今パスで新規作成 or 未検出）。
			continue
		}
		if !mirakc.IsOurs(s.Tags) {
			// 自分が作った schedule だけ触る。tag のない schedule は外部産で、
			// 既存の delete ループの ours 判定と揃えてある。
			continue
		}

		wantPriority := effectivePriority(r.cfg.DefaultPriority, d.opts)
		priorityMismatch := s.Options.Priority != wantPriority
		// 新形式（program:{programId}）でない tag（旧形式の reservation ベース、
		// または内容が食い違っている）も再作成の契機にする。旧形式のまま残っている
		// schedule はこの分岐で新形式に移行する（レベルトリガー。#53 の決定）。
		// tags は ingest が record と予約を突き合わせるのに使うため、古い tag が
		// 残ると録画が別の予約に紐付く。
		tagProgramID, hasNewTag := mirakc.FindProgramTag(s.Tags)
		tagMismatch := !hasNewTag || tagProgramID != d.res.ProgramID
		if !priorityMismatch && !tagMismatch {
			continue
		}
		candidates = append(candidates, recreateCandidate{d: d, observed: s})
	}

	// ガードは state の allowlist。scheduled 以外（tracking/recording/
	// rescheduling/finished/failed、および将来 mirakc が増やす未知の値）は
	// 触らず次のパスに持ち越す（mirakc.ScheduleStateScheduled のコメント参照）。
	var eligible []recreateCandidate
	for _, c := range candidates {
		if c.observed.State != mirakc.ScheduleStateScheduled {
			stateGuarded++
			continue
		}
		eligible = append(eligible, c)
	}
	updateDiff = len(eligible)

	// MaxRecreatesPerPass はサーキットブレーカー（breaker.ReconcileTotalLoss）とは
	// 別物の単なるレート制限なので、超えた分は諦めずに次パスへ持ち越すだけ。
	// この再作成の DELETE はサーキットブレーカーの削除数（toDelete）には
	// 一切数えない — 混ぜるとルールの priority 一括変更でブレーカーが誤作動する。
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].d.res.ID < eligible[j].d.res.ID })

	for i, c := range eligible {
		if i >= r.cfg.MaxRecreatesPerPass {
			limitCarriedOver++
			continue
		}
		if err := r.recreateSchedule(ctx, c.d, c.observed); err != nil {
			slog.Error("reconciler: recreating schedule",
				"reservation_id", c.d.res.ID, "program_id", c.d.res.ProgramID, "err", err)
			continue
		}
		recreated++
		metrics.ReconcileSchedules.WithLabelValues("recreated").Inc()
	}

	return recreated, updateDiff, stateGuarded, limitCarriedOver
}

// recreateSchedule は 1 件の schedule を DELETE→POST で再作成する。
// mirakc に schedule の更新 API がない（GET/POST/GET{id}/DELETE{id} の 4 つだけ）
// ための回避策で、その間 schedule が存在しない窓ができる。
func (r *Reconciler) recreateSchedule(ctx context.Context, d desiredReservation, observed mirakc.Schedule) error {
	res, opts := d.res, d.opts

	// contentPath は初回生成値を base に固定し、以後変更しない
	// （docs/recording.md §3.2）。再作成では contentPath をテンプレートから
	// 再生成せず、observed（= 自分が過去に書いた値が往復してきたもの）を
	// そのまま使う。再生成すると EPG の番組名変更が priority 変更の副作用として
	// ファイル名を変えてしまう。SanitizeContentPath を通すのは、mirakc 側を
	// 直接触られていた場合の保険（安いので）。
	//
	// observed の contentPath が nil・空文字のときだけ、初回作成と同じ生成経路に
	// フォールバックする。
	var contentPath string
	if observed.Options.ContentPath != nil && *observed.Options.ContentPath != "" {
		contentPath = contentpath.SanitizeContentPath(*observed.Options.ContentPath)
	} else {
		cp, err := resolveContentPath(res, d.snap, opts)
		if err != nil {
			return err
		}
		contentPath = cp
	}

	priority := effectivePriority(r.cfg.DefaultPriority, opts)

	if err := r.mirakc.DeleteSchedule(ctx, res.ProgramID); err != nil {
		return fmt.Errorf("DELETE schedule for recreate: %w", err)
	}

	input := mirakc.ScheduleInput{
		ProgramID: res.ProgramID,
		Options: mirakc.Options{
			ContentPath: &contentPath,
			Priority:    priority,
		},
		Tags: []string{mirakc.ProgramTag(res.ProgramID)},
	}

	if _, err := r.mirakc.CreateSchedule(ctx, input); err != nil {
		// DELETE には成功したが POST が失敗した = schedule が消えたまま次の
		// パスまで残る。レベルトリガーで次パスが再作成を試みるが、その間に
		// 番組の開始時刻を越えると取りこぼす。
		//
		// docs/recording.md §3.2 は quality_events に記録するとしているが、
		// quality_events は recordings テーブルの列で、開始前の番組には
		// recordings 行がまだ存在しない（record は録画開始して初めて作られる）
		// ため書き込み先がない。実装できないので、専用のカウンタメトリクスと
		// Error ログで代替する（docs 側の記述の修正は別途行われる想定）。
		metrics.ReconcileScheduleLost.Inc()
		slog.Error("reconciler: schedule lost — DELETE succeeded but recreate POST failed; "+
			"next pass will recreate it (level-triggered), but the program may start before that",
			"reservation_id", res.ID, "program_id", res.ProgramID, "err", err)
		return fmt.Errorf("POST schedule after delete: %w", err)
	}

	slog.Info("reconciler: recreated schedule",
		"reservation_id", res.ID,
		"program_id", res.ProgramID,
		"priority", priority,
		"content_path", contentPath,
	)
	return nil
}

// recordNeverScheduled は「番組終了後に schedule が観測されなかった」予約に
// ついて、recordings に never-scheduled の試行行を作る（issue #98 の決定）。
// reservations.orphaned_at という不可逆な列は廃止し、この観測は recordings の
// 恒久行として持つ（CLAUDE.md 不変条件 12「表は行の寿命で割る」）。
//
// now は RunPass の作成ループが使ったものをそのまま受け取る —— 判定式だけで
// なく判定の瞬間まで揃える（別々に取ると、パス中に終了時刻を跨いだ番組が
// POST されたうえで同じパスで never-scheduled 行が作られる）。終了判定は
// programEnded（RunPass の作成ループと共有）で、schedule の非観測は今パスで
// observeSchedules した schedules をそのまま使う。境界がここと作成ループで
// ずれると、同じ予約が「作らない」と「まだ never-scheduled にしない」の間に
// 落ちて mirakc への POST を撃ち続けるので、programEnded 以外の場所で終了
// 判定を書き下さないこと（issue #134）。
//
// 冪等性（CLAUDE.md 不変条件 5: レベルトリガー）は 2 重に担保される:
//
//  1. CreateNeverScheduledRecording 自身が ON CONFLICT ... DO NOTHING で、
//     同一 active-event に既に生きている recordings 行（前パスで作った
//     never-scheduled 行、または本物の record）があれば何もしない
//  2. 前パスで作った never-scheduled 行がある予約は、次パスの
//     ListReservationsForSyncEvaluation（listDesired の元クエリ）で既に
//     除外されるので、reservations 自体がこの関数に渡ってこない
//
// (1) は「本物の record が推論に必ず勝つ」規則（issue #129 症状 2）の
// 適用でもあり、これが #59（markOrphaned が recordings を見ないので成功
// 録画も orphaned 扱いになる）を解消する ---
// 書き込み条件が「その放送イベントに生きている recordings 行が無いこと」に
// なり、条件を満たさなければ索引（recordings_unique_active_event）が
// 二重に弾く。
func (r *Reconciler) recordNeverScheduled(ctx context.Context, reservations []desiredReservation, schedules []mirakc.Schedule, now time.Time) error {
	scheduledPrograms := make(map[int64]struct{}, len(schedules))
	for _, s := range schedules {
		scheduledPrograms[s.Program.ID] = struct{}{}
	}

	q := sqlcgen.New(r.pool)
	for _, d := range reservations {
		res, snap := d.res, d.snap
		if !programEnded(snap, now) {
			continue
		}
		if _, scheduled := scheduledPrograms[res.ProgramID]; scheduled {
			continue
		}

		// チャンネル識別（network_id/service_id/event_id/channel_type/
		// channel/service_name）が欠けている行を弾く分岐はかつてここにあった
		// （00009 以前の残骸、または event_id/service_name が未対応だった頃
		// （issue #98 より前）の program_snapshots 行を想定）。issue #101
		// （00026）でこの 6 列が NOT NULL 化され、その状態自体が表現不可能に
		// なったため落とした（起きない状態のための分岐を残さない）。

		source, err := db.DeriveRecordingSource(ctx, q, r.site, res.ProgramID, true)
		if err != nil {
			slog.Error("reconciler: deriving recording source for never-scheduled outcome",
				"reservation_id", res.ID, "program_id", res.ProgramID, "err", err)
			continue
		}

		qeJSON, err := neverScheduledQualityEvents(now, snap)
		if err != nil {
			slog.Error("reconciler: marshalling never-scheduled quality event",
				"reservation_id", res.ID, "program_id", res.ProgramID, "err", err)
			continue
		}

		rows, err := q.CreateNeverScheduledRecording(ctx, sqlcgen.CreateNeverScheduledRecordingParams{
			ReservationID:     &res.ID,
			RuleID:            res.RuleID,
			Source:            source,
			Site:              r.site,
			NetworkID:         snap.NetworkID,
			ServiceID:         snap.ServiceID,
			EventID:           snap.EventID,
			ServiceName:       snap.ServiceName,
			ChannelType:       snap.ChannelType,
			Channel:           snap.Channel,
			Title:             snap.Title,
			ProgramStartAt:    snap.StartAt,
			ProgramDurationMs: snap.DurationMs,
			QualityEvents:     qeJSON,
		})
		if err != nil {
			return fmt.Errorf("recording never-scheduled outcome for reservation %d: %w", res.ID, err)
		}
		// :execrows なので実際に INSERT できたか（1）か、ON CONFLICT で
		// 何もしなかったか（0）が分かる。0 行なら（他パスとの競合で既に
		// 記録済み、または本物の record が先にその枠を占有している）ログを
		// 出さない —— 出すと実態と食い違う。
		if rows == 0 {
			continue
		}
		slog.Info("reconciler: recorded never-scheduled outcome (program ended without an observed schedule)",
			"reservation_id", res.ID,
			"program_id", res.ProgramID,
		)
	}
	return nil
}

// neverScheduledQualityEvents は recordNeverScheduled が書く quality_events の
// JSON 配列（要素 1 つ）を組み立てる。理由の内訳に番組の終了時刻を含めるのは
// 「なぜ録れなかったかを説明可能にする」という docs/schema.md §5 の要求
// （手動確認する人間が判断材料を持てるように）。
func neverScheduledQualityEvents(now time.Time, snap sqlcgen.ProgramSnapshot) (json.RawMessage, error) {
	programEndedAt := snap.StartAt.Add(time.Duration(snap.DurationMs) * time.Millisecond)
	reason, err := json.Marshal(map[string]any{
		"programEndedAt": programEndedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling reason: %w", err)
	}
	qe := db.QualityEvent{
		At:     now,
		Event:  db.QualityEventNeverScheduled,
		Reason: reason,
	}
	qeJSON, err := json.Marshal([]db.QualityEvent{qe})
	if err != nil {
		return nil, fmt.Errorf("marshalling quality events: %w", err)
	}
	return qeJSON, nil
}

// startDelayed は開始遅延検出器が検出した 1 件（ログ出力用の要約）。
type startDelayed struct {
	id        int64
	programID int64
	title     string
	elapsed   time.Duration
}

// detectStartDelays は「開始時刻 + StartDelayGrace を過ぎたのに録画開始
// （recordings.started_at）が観測されていない予約」を検出する（issue #24 M2-7、
// docs/recording.md §3.3「開始遅延検出器」）。録画開始は mirakc に全面委譲済みで
// Rokuban 側から防ぐ手段はないが、EPGStation#724（チューナー再接続ハングで
// 開始が 10 分遅延）のような mirakc 側の未知の不具合への保険として、reconcile
// の 1 パスの中で毎回検出し直す（不変条件 5: レベルトリガー。DB に新しい状態は
// 持たせず、呼び出し元がゲージに反映するだけの導出値）。
//
// reservations は呼び出し元（RunPass）が listDesired で得た desired 予約
// リストをそのまま渡す想定で、次の 2 つは既にそこで除外されている。
//   - effective.skip の予約（listDesired が db.EvaluateSyncCandidates の Skipped
//     で絞っている）
//   - orphaned_at が非 NULL の予約（listDesired の元クエリ
//     ListReservationsForSyncEvaluation が orphaned_at IS NULL で絞っている）
//
// ここではさらに時間窓で絞る: 「開始時刻 + StartDelayGrace < now() < 終了時刻」。
// 終了時刻を過ぎた予約は markOrphaned の領分であり、ここで検出し続けると
// 終わった番組についてアラートが鳴り止まなくなる（markOrphaned が拾うのを待つ）。
//
// 観測の有無は recordings.started_at で判定する。予約に対応する recordings 行
// そのものが無い場合（録画が一切観測されていない）も「観測なし」として扱う —
// ListStartedReservationIDs は渡した予約 ID のうち started_at が埋まっている
// ものだけを返すので、返らなかった ID がそのまま「観測なし」の集合になる。
func (r *Reconciler) detectStartDelays(ctx context.Context, reservations []desiredReservation) ([]startDelayed, error) {
	now := time.Now()

	var candidates []desiredReservation
	for _, d := range reservations {
		startAt := d.snap.StartAt
		endAt := startAt.Add(time.Duration(d.snap.DurationMs) * time.Millisecond)
		if !now.After(startAt.Add(r.cfg.StartDelayGrace)) {
			// まだ猶予の内側。開始直後の SSE 到達・watcher 処理の遅れを
			// 誤検知しないための窓（Config.StartDelayGrace のコメント参照）。
			continue
		}
		if !now.Before(endAt) {
			// 番組終了後は markOrphaned の領分。ここで拾うと終わった番組を
			// 遅延として報告し続けてアラートが鳴り止まなくなる。
			continue
		}
		candidates = append(candidates, d)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	ids := make([]int64, len(candidates))
	for i, d := range candidates {
		ids[i] = d.res.ID
	}

	started, err := sqlcgen.New(r.pool).ListStartedReservationIDs(ctx, sqlcgen.ListStartedReservationIDsParams{
		Site:           r.site,
		ReservationIds: ids,
	})
	if err != nil {
		return nil, fmt.Errorf("listing started reservation ids: %w", err)
	}
	startedSet := make(map[int64]struct{}, len(started))
	for _, id := range started {
		if id != nil {
			startedSet[*id] = struct{}{}
		}
	}

	var delayed []startDelayed
	for _, d := range candidates {
		if _, ok := startedSet[d.res.ID]; ok {
			continue
		}
		delayed = append(delayed, startDelayed{
			id:        d.res.ID,
			programID: d.res.ProgramID,
			title:     d.snap.Title,
			elapsed:   now.Sub(d.snap.StartAt),
		})
	}
	return delayed, nil
}
