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
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reservation"
)

// broadcastEventIDUniquenessWindow は ARIB TR-B14 第四編 8.2.1 が同一サービスの
// event_id に保証する一意性の期間。運用で変更する値ではないため設定にはしない。
const broadcastEventIDUniquenessWindow = 24 * time.Hour

// Config は Reconciler の設定。
type Config struct {
	// MaxRecreatesPerPass は 1 パスで行う予約オプション差分反映の再作成
	// （DELETE→POST）の上限。判断は docs/recording/reconciler.md §1 パスの
	// 再作成数に上限を設ける、ruler 側の大量削除ブレーカーとの別物性は
	// docs/recording/breaker.md §止められる場所は ruler だけ にある。
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

	tripped := r.observeDeleteBreaker(ctx, q)

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

	now := time.Now()
	created, missing, endGuarded := r.createMissingSchedules(ctx, reservations, observedByProgram, now)
	toDelete := collectDeleteCandidates(reservations, schedules)
	totalLoss, tripped := r.observeTotalLoss(ctx, q, reservations, toDelete, tripped)
	deleted := r.deleteSchedules(ctx, toDelete, tripped, totalLoss)

	recreated, updateDiff, stateGuarded, limitCarriedOver := r.recreateChanged(ctx, reservations, observedByProgram)

	if err := q.DeleteStaleScheduleSyncs(ctx, sqlcgen.DeleteStaleScheduleSyncsParams{
		Site:       r.site,
		ObservedAt: sweepTime,
	}); err != nil {
		slog.Error("reconciler: cleaning stale schedule_syncs", "err", err)
	}

	if err := r.recordNeverScheduledOutcome(ctx, reservations, schedules, now); err != nil {
		slog.Error("reconciler: recording never-scheduled outcome", "err", err)
	}
	startDelayed := r.observeStartDelays(ctx, reservations)
	r.observePendingDiff(missing, len(toDelete), updateDiff, stateGuarded)
	r.logCarriedOver(stateGuarded, limitCarriedOver, endGuarded)

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

// observeDeleteBreaker は発動状態を DB の真実に合わせ直す。判定できない場合も安全側に
// 倒して発動中とみなし削除を止める（記録・確認ができないまま削除を続けるのが最悪の
// 組み合わせという breaker.Trip のコメントと同じ理由）。
func (r *Reconciler) observeDeleteBreaker(ctx context.Context, q *sqlcgen.Queries) bool {
	tripped, err := breaker.ObserveState(ctx, q, r.site, breaker.ReconcileTotalLoss)
	if err != nil {
		slog.Error("reconciler: observing circuit breaker state; withholding deletes to be safe", "breaker", breaker.ReconcileTotalLoss, "err", err)
		return true
	}
	return tripped
}

// createMissingSchedules は観測されていない desired reservation を POST する。番組は
// もう終わっている場合、POST しても mirakc は need-rescheduling で数秒後に failed に
// するだけで、recordings に content_length=0 の failed 行が残るだけ（issue #134 の実測:
// record_sync 46 件中 41 件が failed）。この予約は本パスの recordNeverScheduled が
// never_scheduled_events に欠測行を作る（同じ programEnded 判定を使うので食い違わない）
// ので、次パス以降は listDesired（ListReservationsForSyncEvaluation の欠測除外）から
// 外れて二度と create ループの対象にならない。
func (r *Reconciler) createMissingSchedules(ctx context.Context, reservations []desiredReservation, observedByProgram map[int64]mirakc.Schedule, now time.Time) (int, int, int) {
	var created, missing, endGuarded int
	for _, d := range reservations {
		if _, exists := observedByProgram[d.res.ProgramID]; exists {
			continue
		}
		if programEnded(d.snap, now) {
			endGuarded++
			continue
		}
		missing++
		if err := r.createSchedule(ctx, d); err != nil {
			slog.Error("reconciler: creating schedule", "program_id", d.res.ProgramID, "err", err)
			continue
		}
		created++
		metrics.ReconcileSchedules.WithLabelValues("created").Inc()
	}
	return created, missing, endGuarded
}

// collectDeleteCandidates は自分が作った schedule のうち desired にないものを削除候補に
// 集める。mirakc が所有しない schedule は対象にしない。
func collectDeleteCandidates(reservations []desiredReservation, schedules []mirakc.Schedule) []mirakc.Schedule {
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
	return toDelete
}

// observeTotalLoss は全損シグネチャを検知する。desired（reservations）が 1 件もないのに、
// 自分が作った schedule が観測されている。件数ではなく形で検知する — listDesired が
// バグ・障害で空を返したときに自分が作った全 schedule を削除してしまう経路だけを捕まえる。
// GC・ユーザー操作による正当な一括削除は他の予約が残るのでここには当たらない
// （docs/recording.md §3.2、issue #2 の M2-5 決定コメント）。
//
// threshold に 0 を渡すのは値の欠落ではない。このブレーカーは件数の閾値を持たない
// （形で検知する）が、「desired が空のときに許される削除数」はまさに 0 なので、
// pending = N と threshold = 0 の組は「0 件しか許されない状況で N 件消そうとした」と
// 読めて正確である。記録が失敗した場合も含め、このパスでは削除を実行しない。
func (r *Reconciler) observeTotalLoss(ctx context.Context, q *sqlcgen.Queries, reservations []desiredReservation, toDelete []mirakc.Schedule, tripped bool) (bool, bool) {
	totalLoss := len(reservations) == 0 && len(toDelete) > 0
	if !totalLoss {
		return false, tripped
	}
	if err := breaker.Trip(ctx, q, r.site, breaker.ReconcileTotalLoss, 0, totalLossSample(toDelete)); err != nil {
		slog.Error("reconciler: recording circuit breaker trip", "breaker", breaker.ReconcileTotalLoss, "err", err)
	}
	metrics.ReconcileCircuitBreakerTrips.Inc()
	return true, true
}

// deleteSchedules はラッチ中の schedule 削除を止め、通常時だけ mirakc から削除する。
// 全損シグネチャは今パスでは検出していないが、ラッチは自動では解けない（手動
// ResumeCircuitBreaker のみが解除する）ので削除は引き続き止める。
func (r *Reconciler) deleteSchedules(ctx context.Context, toDelete []mirakc.Schedule, tripped, totalLoss bool) int {
	if tripped {
		if !totalLoss {
			slog.Error("reconciler: circuit breaker latched — withholding schedule deletes until manually resumed", "breaker", breaker.ReconcileTotalLoss, "pending_deletes", len(toDelete))
		}
		return 0
	}
	var deleted int
	for _, s := range toDelete {
		if err := r.mirakc.DeleteSchedule(ctx, s.Program.ID); err != nil {
			slog.Error("reconciler: deleting schedule", "program_id", s.Program.ID, "err", err)
			continue
		}
		deleted++
		metrics.ReconcileSchedules.WithLabelValues("deleted").Inc()
	}
	return deleted
}

// recordNeverScheduledOutcome は作成ループと同じ瞬間を渡す（同じ式だけでなく同じ材料にする）。
// 別々に time.Now() を取ると、パス実行中に終了時刻を跨いだ番組が「作成ループでは未終了
// → POST」かつ「recordNeverScheduled では終了 → never-scheduled 行を作成」になり、収束は
// するが issue #134 が消したい failed 行がちょうど 1 件出る。
func (r *Reconciler) recordNeverScheduledOutcome(ctx context.Context, reservations []desiredReservation, schedules []mirakc.Schedule, now time.Time) error {
	return r.recordNeverScheduled(ctx, reservations, schedules, now)
}

// observeStartDelays は開始遅延をログに出す。DB に新しい状態は持たせない（不変条件 5:
// レベルトリガー）。毎パス recordings.started_at から再計算する導出値なので、ゲージだけ
// で表す。収束すれば（recording.started が観測されれば）次のパスでゼロに戻る。
func (r *Reconciler) observeStartDelays(ctx context.Context, reservations []desiredReservation) []startDelayed {
	startDelayed, err := r.detectStartDelays(ctx, reservations)
	if err != nil {
		slog.Error("reconciler: detecting start delays", "err", err)
	}
	for _, d := range startDelayed {
		slog.Error("reconciler: recording not started past start time + grace", "program_id", d.programID, "title", d.title, "elapsed", d.elapsed)
	}
	metrics.ReconcileStartDelayed.WithLabelValues(r.site).Set(float64(len(startDelayed)))
	return startDelayed
}

// observePendingDiff は差分そのものをゲージで出す。健全なら次のパスでゼロになる。
// 作成/削除/再作成できずに残った量が知りたいので、実行した件数ではなく検出した件数
// （MaxRecreatesPerPass で持ち越した分も含む）。state の allowlist で意図的に触らなかった
// 分は update ではなく update_deferred に入れる。混ぜると「ゼロに戻らない = 異常」という
// このゲージの読み方が壊れる（metrics.ReconcilePendingDiff のコメント参照）。
func (r *Reconciler) observePendingDiff(missing, stale, updateDiff, stateGuarded int) {
	metrics.ReconcileLastPass.SetToCurrentTime()
	metrics.ReconcilePendingDiff.WithLabelValues("create").Set(float64(missing))
	metrics.ReconcilePendingDiff.WithLabelValues("delete").Set(float64(stale))
	metrics.ReconcilePendingDiff.WithLabelValues("update").Set(float64(updateDiff))
	metrics.ReconcilePendingDiff.WithLabelValues("update_deferred").Set(float64(stateGuarded))
}

// logCarriedOver は持ち越した件数を黙って落とさず、「収束しない」原因が見えるように
// state ガード・MaxRecreatesPerPass のどちらで持ち越したかを分けて出す。
// stateGuarded / limitCarriedOver と同じ扱いで endGuarded もログに出す。
// メトリクスは増やさない — 上2つは複数パスにまたがって残留しうる持ち越し（録画中は
// priority 変更が反映されないまま残る、MaxRecreatesPerPass で溢れた分が次パスに送られる）
// だからゲージで監視する価値があるのに対し、endGuarded は本パスの recordNeverScheduled
// （旧 markOrphaned）が同じ判定式で recordings に never-scheduled 行を作るので、次パスには
// 同じ予約が二度と現れない（1 パスで自己解消する）。ReconcilePendingDiff の「create」
// ゲージに混ぜると、埋まらない別の理由（mirakc 障害等）と区別できなくなる。
func (r *Reconciler) logCarriedOver(stateGuarded, limitCarriedOver, endGuarded int) {
	if stateGuarded > 0 {
		slog.Info("reconciler: recreate candidates deferred to next pass (schedule state guard)", "count", stateGuarded)
	}
	if limitCarriedOver > 0 {
		slog.Info("reconciler: recreate candidates deferred to next pass (MaxRecreatesPerPass)", "count", limitCarriedOver, "max_recreates_per_pass", r.cfg.MaxRecreatesPerPass)
	}
	if endGuarded > 0 {
		slog.Info("reconciler: create candidates skipped (program already ended)", "count", endGuarded)
	}
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
	opts reservation.Options
}

// listDesired は mirakc への同期対象を返す。
//
// state（過去に存在した列。#28/#30 で orphaned_at に、issue #98 で recordings の
// 存在に置き換わって現在は削除済みの導出値）ではなく effective.skip で絞る
// （docs/schema.md §3「state を『mirakc への同期対象か』のフィルタに
// 使ってはならない」、docs/recording.md §4.3）。
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
// effective.skip の絞り込みは reservation.EvaluateSyncCandidates に通す
// （internal/reservation/sync.go）。この関数はここ（reconciler.listDesired）と
// cmd/rokuban/shadowdiff.go の 2 箇所から呼ばれる共通処理 --- 2 箇所が別々に
// reservation.EffectiveOptions を呼ぶ形だと移植漏れが起きうる（issue #54 の見逃しの原因）。
func (r *Reconciler) listDesired(ctx context.Context) ([]desiredReservation, error) {
	rows, err := sqlcgen.New(r.pool).ListReservationsForSyncEvaluation(ctx, r.site)
	if err != nil {
		return nil, err
	}

	var desired []desiredReservation
	for _, c := range reservation.EvaluateSyncCandidates(rows) {
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
// recordNeverScheduled（旧 markOrphaned）の両方から呼ばれる必要があるため、
// この 1 箇所に抽出してある。同じ式を 2 箇所に書き下すと、片方だけ境界を
// 直してもう片方を直し忘れる事故が起きる（issue #134）。実際、作成ループ
// だけ境界がずれると「作らない」（作成ループ）と「まだ never-scheduled
// にしない」（recordNeverScheduled）が食い違い、同じ予約が毎パス作成対象の
// まま残って mirakc への POST を撃ち続ける —— この抽出前より悪化する
// 組み合わせなので、境界は 1 箇所でしか定義しない。
//
// 終了時刻ちょうど（endTime == now）は「終了していない」側に倒す
// （抽出前の markOrphaned が元々持っていた `!endTime.Before(now)` を素直に
// 反転しただけで、境界を動かしていない）。進行中の番組への POST は止めない —— mirakc
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
func effectivePriority(defaultPriority int, opts reservation.Options) int {
	if opts.Priority != nil {
		return *opts.Priority
	}
	return defaultPriority
}

// explicitContentPath はユーザーが overrides.contentPath に明示指定した値を
// サニタイズ済みで返す。ok=false は「明示指定が無い」= テンプレート生成に委ねる。
//
// effective の ContentPath が非 nil であることが「ユーザーが書いた」と同値である
// のは、reservations.base に contentPath を載せる書き手が存在しないため
// （ruler の computeBase が意図的に除外している）。ruler が base に contentPath を
// 載せるようになったらこの同値が崩れ、テンプレート生成値が差分対象に混ざって
// #19 が潰した churn が戻る。
func explicitContentPath(opts reservation.Options) (string, bool) {
	if opts.ContentPath == nil || *opts.ContentPath == "" {
		return "", false
	}
	return contentpath.SanitizeContentPath(*opts.ContentPath), true
}

// buildContentPath は録画ファイルの content_path を組み立てる。
//
// template が空文字なら contentpath.DefaultTemplate（テンプレート未指定時の
// 従来の見た目 YYYYMMDD/HHMMSS_タイトル_サービスID.m2ts と同じ結果になる
// template 文字列。番組名が空文字の場合だけ "_" 昇格の有無で 1 文字ずれるが、
// EPG 射影が空名の番組を落とすため到達しない —— internal/worker/epg.go の
// projectable）を使う。いずれも text/template として contentpath.Build
// で展開する。渡す contentpath.Data は program_snapshots の
// title/channel/channelType から contentpath.NewData で組む — この時点で各
// フィールドがパス成分としてサニタイズされるため、EPG データ（番組名等）に
// "/" や ".." が混ざっても意図しない階層やパストラバーサルを構造的に作れない。
// 最終的な拡張子付与とパス全体のサニタイズは contentpath.Build の責務。
//
// テンプレートは実行時にもエラーになりうる（未知フィールドの参照等。ありえ
// ないはずだが）。推測せずそのままエラーを返す。
func buildContentPath(snap sqlcgen.ProgramSnapshot, template string) (string, error) {
	if template == "" {
		template = contentpath.DefaultTemplate
	}

	serviceID := int(snap.ServiceID)
	data := contentpath.NewData(snap.Title, snap.StartAt, snap.Channel, serviceID, snap.ChannelType)

	path, err := contentpath.Build(template, data)
	if err != nil {
		return "", fmt.Errorf("expanding filename template: %w", err)
	}
	return path, nil
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
// service_id が無い（推測せず schedule を作らない）という分岐は持たない ---
// issue #101 で program_snapshots のチャンネル・イベント識別 6 列が
// NOT NULL 化され、service_id が NULL になる状態自体が表現不可能になった
// （起きない状態のための分岐を残さない）。
func resolveContentPath(res sqlcgen.Reservation, snap sqlcgen.ProgramSnapshot, opts reservation.Options) (string, error) {
	// contentPath は filenameTemplate（ruler が base に載せたテンプレート、または
	// ユーザーの明示的な上書き）があればそれを展開し、なければ従来の固定形式
	// （buildContentPath 参照）。ContentPath（フルパスの直接指定）が別途あれば
	// そちらが最終的に勝つ — 展開結果よりユーザーの明示指定を優先する
	// （reservation.Options のドキュメントコメント参照）。
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
		return "", fmt.Errorf("building content path for program %d: %w",
			res.ProgramID, err)
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
	// reason は再作成の契機（"priority" / "tag" / "content_path" を立った順に
	// 連結したもの）。recreateSchedule の Info ログに載せる —— mirakc が
	// contentPath をそのまま返さない実装だった場合、この理由が反復して
	// 出続けることが唯一の観測手段になる（explicitContentPath のコメント参照）。
	reason string
}

// recreateChanged は effective options（priority / contentPath の明示指定）と
// 観測結果（priority / reservation tag / contentPath）の差分を DELETE→POST の
// 再作成で消す。存在の有無ではなく内容の差分なので、呼び出し元の create/delete
// ループとは独立に走る（docs/recording.md §3.2、issue #19）。
//
// 差分対象は priority・reservation tag・明示指定された contentPath
// （explicitContentPath が ok を返す場合のみ）。テンプレート生成の contentPath
// （filenameTemplate 展開・既定形式）は差分対象にしない（EPG の番組名が変わる
// たびに schedule が消えて作り直される churn になるため。recreateSchedule 側で
// 明示指定が無ければ observed の contentPath をそのまま引き継ぐ）。
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
		// tag の programId が予約とずれていたら再作成する。tags は ingest が
		// record と予約を突き合わせるのに使うため、ずれたまま残ると録画が別の
		// 予約に紐付く。
		//
		// tag が読めないケースはここに来ない（上の IsOurs が弾いている）ので、
		// programID の比較だけでよい。
		tagProgramID, _ := mirakc.FindProgramTag(s.Tags)
		tagMismatch := tagProgramID != d.res.ProgramID

		// contentPath は明示指定（overrides.contentPath）があるときだけ比較する。
		// 比較の左辺は observed の生値（再サニタイズしない） — POST する値
		// （wantContentPath）と比較する値を同一にすることで、SanitizeContentPath
		// の冪等性に依存せず 1 パスで収束する。明示指定が無いとき（reset された
		// 場合を含む）は何も比較しない。テンプレート展開を desired として計算する
		// 式をここに書くと #19 が潰した churn が戻る。
		wantContentPath, hasExplicitContentPath := explicitContentPath(d.opts)
		var observedContentPath string
		if s.Options.ContentPath != nil {
			observedContentPath = *s.Options.ContentPath
		}
		contentPathMismatch := hasExplicitContentPath && observedContentPath != wantContentPath

		if !priorityMismatch && !tagMismatch && !contentPathMismatch {
			continue
		}

		var reasons []string
		if priorityMismatch {
			reasons = append(reasons, "priority")
		}
		if tagMismatch {
			reasons = append(reasons, "tag")
		}
		if contentPathMismatch {
			reasons = append(reasons, "content_path")
		}
		candidates = append(candidates, recreateCandidate{d: d, observed: s, reason: strings.Join(reasons, ",")})
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
	// listDesired は r.site = $1 で絞るので、1 パス内の候補は単一サイトに限られる
	// （サイトをまたいだ比較は起こらない）。
	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].d.res.ProgramID < eligible[j].d.res.ProgramID
	})

	for i, c := range eligible {
		if i >= r.cfg.MaxRecreatesPerPass {
			limitCarriedOver++
			continue
		}
		if err := r.recreateSchedule(ctx, c.d, c.observed, c.reason); err != nil {
			slog.Error("reconciler: recreating schedule",
				"program_id", c.d.res.ProgramID, "err", err)
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
//
// contentPath の決定は 3 分岐で、順序が意味を持つ:
//  1. 明示 override（overrides.contentPath）があればそれが最優先。
//     explicitContentPath 経由にすることで、テンプレート再生成（resolveContentPath
//     が先に buildContentPath を通す）には絶対に落ちない — テンプレートが壊れて
//     いても、明示指定がある限り再作成が失敗しないことが経路として担保される。
//  2. 無ければ observed の contentPath を引き継ぐ（従来どおり。base に書き戻す
//     経路は無いので、これがテンプレート再生成を避ける唯一の手段）。
//  3. observed も nil・空文字なら、初回作成と同じ生成経路にフォールバックする。
func (r *Reconciler) recreateSchedule(ctx context.Context, d desiredReservation, observed mirakc.Schedule, reason string) error {
	res, opts := d.res, d.opts

	var contentPath string
	if cp, ok := explicitContentPath(opts); ok {
		contentPath = cp
	} else if observed.Options.ContentPath != nil && *observed.Options.ContentPath != "" {
		// SanitizeContentPath を通すのは、mirakc 側を直接触られていた場合の保険
		// （安いので）。
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
			"program_id", res.ProgramID, "err", err)
		return fmt.Errorf("POST schedule after delete: %w", err)
	}

	slog.Info("reconciler: recreated schedule",
		"program_id", res.ProgramID,
		"priority", priority,
		"content_path", contentPath,
		"reason", reason,
	)
	return nil
}

// recordNeverScheduled は「番組終了後に schedule が観測されなかった」予約に
// ついて、never_scheduled_events 表に欠測の行を作る（issue #318）。欠測は試行
// ではなく観測の不在なので、recordings（観測された試行の脊椎）ではなく放送
// イベントを主キーにした専用表の「行の存在」で表す（CLAUDE.md 不変条件 10・12）。
//
// now は RunPass の作成ループが使ったものをそのまま受け取る —— 判定式だけで
// なく判定の瞬間まで揃える（別々に取ると、パス中に終了時刻を跨いだ番組が
// POST されたうえで同じパスで欠測行が作られる）。終了判定は programEnded
// （RunPass の作成ループと共有）で、schedule の非観測は今パスで observeSchedules
// した schedules をそのまま使う。境界がここと作成ループでずれると、同じ予約が
// 「作らない」と「まだ欠測にしない」の間に落ちて mirakc への POST を撃ち続ける
// ので、programEnded 以外の場所で終了判定を書き下さないこと（issue #134）。
//
// 冪等性（CLAUDE.md 不変条件 5: レベルトリガー）は 2 重に担保される:
//
//  1. CreateNeverScheduledEvent 自身が ON CONFLICT ... DO NOTHING で、既に
//     欠測行があれば何もしない
//  2. 前パスで欠測を記録した予約は、次パスの ListReservationsForSyncEvaluation
//     （listDesired の元クエリ）で既に除外されるので、reservations 自体が
//     この関数に渡ってこない
//
// 本物の record が後から来ても欠測行は消さない（録画は録画、欠測は欠測）。
// 「録れたのに orphaned のまま」（#59）は表示側（never_recorded）が本物の
// recordings 行の不在も条件にすることで防ぐ（reservations.sql 参照）。
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

		rows, err := q.CreateNeverScheduledEvent(ctx, sqlcgen.CreateNeverScheduledEventParams{
			Site:      r.site,
			NetworkID: snap.NetworkID,
			ServiceID: snap.ServiceID,
			EventID:   snap.EventID,
		})
		if err != nil {
			return fmt.Errorf("recording never-scheduled outcome for program %d: %w", res.ProgramID, err)
		}
		// :execrows なので実際に INSERT できたか（1）か、ON CONFLICT で
		// 何もしなかったか（0）が分かる。0 行なら（他パスとの競合で既に
		// 記録済み）ログを出さない —— 出すと実態と食い違う。
		if rows == 0 {
			continue
		}
		slog.Info("reconciler: recorded never-scheduled outcome (program ended without an observed schedule)",
			"program_id", res.ProgramID,
		)
	}
	return nil
}

// startDelayed は開始遅延検出器が検出した 1 件（ログ出力用の要約）。
type startDelayed struct {
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
//   - effective.skip の予約（listDesired が reservation.EvaluateSyncCandidates の Skipped
//     で絞っている）
//   - 既に never-scheduled と判定された予約（listDesired の元クエリ
//     ListReservationsForSyncEvaluation が、番組終了後に schedule が一度も
//     観測されなかったことを示す never-scheduled recordings 行の NOT EXISTS で
//     絞っている。旧 orphaned_at 列は issue #98 で廃止済み）
//
// ここではさらに時間窓で絞る: 「開始時刻 + StartDelayGrace < now() < 終了時刻」。
// 終了時刻を過ぎた予約は recordNeverScheduled の領分であり、ここで検出し続けると
// 終わった番組についてアラートが鳴り止まなくなる（recordNeverScheduled が拾うのを待つ）。
//
// 観測の有無は recordings.started_at で判定する。判定の宛先キーは予約 id ではなく
// 放送イベント (network_id, service_id, event_id) —— 予約行の導出キーは ruler の
// 導出削除・再実体化で変わる不安定な値で、recordings.reservation_id（issue #158 で
// 列自体を削除済み）は当時 ON DELETE SET NULL だった。予約 id で引くと、録画中に EPG フリッカーやルール
// 編集で予約行が作り直された瞬間に started 済み recordings 行が見つからなくなり、
// 検出窓の間毎パス開始遅延を誤検知する（CLAUDE.md 不変条件 9 の identity。
// #29 / #53 / #98 / #99 / #149 と同じ族、internal/db/queries/start_delay.sql 参照）。
// desiredReservation は listDesired が program_snapshots を JOIN 済みで得た結果
// なので、放送イベントキーは既に手元にある。
//
// 予約に対応する recordings 行そのものが無い場合（録画が一切観測されていない）も
// 「観測なし」として扱う —— ListStartedBroadcastEventKeys は渡したキーのうち
// started_at が埋まっているものだけを返すので、返らなかったキーがそのまま
// 「観測なし」の集合になる。
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
			// 番組終了後は recordNeverScheduled の領分。ここで拾うと終わった番組を
			// 遅延として報告し続けてアラートが鳴り止まなくなる。
			continue
		}
		candidates = append(candidates, d)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// event_id はイベント終了から 24 時間しか一意でないため、event_id が再利用
	// されるのは前イベント終了の 24 時間後以降でしかない。したがって「今回の
	// イベントの録画」の started_at は必ず「今回のイベント開始 - 24 時間」以降に
	// ある。候補の開始時刻ちょうどを下界にすると、24 時間を超える長尺番組では
	// その番組自身の started_at が下界より古くなり、録画中なのに毎パス開始遅延と
	// 誤検知する。recordings.started_at は mirakc がチューナーを開いた時刻そのままで、
	// 構造的に「予定 - 15 秒」になる（実測 -00:00:14.99、前番組延長・
	// running_status=2 ではさらに数秒〜十数秒早い。docs/recording/delegation.md
	// §2「PSI/SI 追従」参照）。下界を「候補の開始時刻 - 24 時間」まで緩めれば、
	// 再利用の除外を保ったまま、started_at が予定より早いことに影響されない。
	startedAfter := now.Add(-broadcastEventIDUniquenessWindow)
	for _, d := range candidates {
		if b := d.snap.StartAt.Add(-broadcastEventIDUniquenessWindow); b.Before(startedAfter) {
			startedAfter = b
		}
	}
	// 下界は候補全体で共有する 1 値なので、バッチに長尺番組が 1 本混じると
	// 他の候補（本来は直近 24 時間で足りる）の窓も一緒に広がる。候補ごとに
	// 下界を分ければ避けられるが、そうすると 3 本の parallel array を
	// generate_subscripts + 添字アクセスでタプル照合する下のクエリ形が崩れる
	// （internal/db/queries/start_delay.sql 参照）。widen 側の誤検知を消す方を
	// 優先し、この天井は許容する。

	networkIDs := make([]int32, len(candidates))
	serviceIDs := make([]int32, len(candidates))
	eventIDs := make([]int32, len(candidates))
	for i, d := range candidates {
		networkIDs[i] = d.snap.NetworkID
		serviceIDs[i] = d.snap.ServiceID
		eventIDs[i] = d.snap.EventID
	}

	started, err := sqlcgen.New(r.pool).ListStartedBroadcastEventKeys(ctx, sqlcgen.ListStartedBroadcastEventKeysParams{
		Site:         r.site,
		NetworkIds:   networkIDs,
		ServiceIds:   serviceIDs,
		EventIds:     eventIDs,
		StartedAfter: startedAfter,
	})
	if err != nil {
		return nil, fmt.Errorf("listing started broadcast event keys: %w", err)
	}
	type broadcastEventKey struct {
		networkID int32
		serviceID int32
		eventID   int32
	}
	startedSet := make(map[broadcastEventKey]struct{}, len(started))
	for _, k := range started {
		startedSet[broadcastEventKey{networkID: k.NetworkID, serviceID: k.ServiceID, eventID: k.EventID}] = struct{}{}
	}

	var delayed []startDelayed
	for _, d := range candidates {
		key := broadcastEventKey{networkID: d.snap.NetworkID, serviceID: d.snap.ServiceID, eventID: d.snap.EventID}
		if _, ok := startedSet[key]; ok {
			continue
		}
		delayed = append(delayed, startDelayed{
			programID: d.res.ProgramID,
			title:     d.snap.Title,
			elapsed:   now.Sub(d.snap.StartAt),
		})
	}
	return delayed, nil
}
