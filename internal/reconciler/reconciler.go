// Package reconciler は予約（desired）と mirakc の schedules（observed）の差分を
// POST/DELETE で消す 1 パス評価ロジック。
//
// reconciler はシングルトンではなく River のジョブ（internal/worker の
// ReconcilePassWorker）として実行される。周期的・冪等・パスを跨ぐ状態を持たない
// （サーキットブレーカーの閾値判定も含め毎パス DB と mirakc から読み直す）という
// 性質が ruler / epg_sync と同じで、排他は advisory lock ではなくジョブロック +
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

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
)

// Config は Reconciler の設定。
type Config struct {
	MaxDeletesPerPass int

	// MaxRecreatesPerPass は 1 パスで行う予約オプション差分反映の再作成
	// （DELETE→POST）の上限。ルールの priority を編集するとマッチしている
	// 全予約が再作成対象になり得るため（N=200 なら 1 パスで 400 回の mirakc
	// 呼び出し）、上限を設けてレベルトリガーで数パスに分けて収束させる。
	//
	// MaxDeletesPerPass（大量削除サーキットブレーカー）とは別物: ブレーカーは
	// 超えたら「何もしない」回路遮断だが、こちらは単なるレート制限で上限まで
	// はやって残りを次パスに送る（docs/recording.md §3.2）。
	MaxRecreatesPerPass int

	DefaultPriority int
}

func defaultConfig() Config {
	return Config{
		MaxDeletesPerPass:   10,
		MaxRecreatesPerPass: 20,
		DefaultPriority:     10,
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
		if cfg.MaxDeletesPerPass > 0 {
			c.MaxDeletesPerPass = cfg.MaxDeletesPerPass
		}
		if cfg.MaxRecreatesPerPass > 0 {
			c.MaxRecreatesPerPass = cfg.MaxRecreatesPerPass
		}
		if cfg.DefaultPriority > 0 {
			c.DefaultPriority = cfg.DefaultPriority
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
// サーキットブレーカー（MaxDeletesPerPass の判定）はこの呼び出し内で完結し、
// 前回呼び出しの状態を一切引き継がない。
func (r *Reconciler) RunPass(ctx context.Context) error {
	slog.Debug("reconciler: starting pass")

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

	for _, d := range reservations {
		if _, exists := observedByProgram[d.res.ProgramID]; exists {
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

	var toDelete []int64
	for _, s := range schedules {
		if _, ours := mirakc.FindReservationID(s.Tags); !ours {
			continue
		}
		if _, desired := desiredPrograms[s.Program.ID]; desired {
			continue
		}
		toDelete = append(toDelete, s.Program.ID)
	}

	if len(toDelete) > r.cfg.MaxDeletesPerPass {
		metrics.ReconcileCircuitBreakerTrips.Inc()
		slog.Error("reconciler: circuit breaker tripped — too many deletes in one pass",
			"pending_deletes", len(toDelete),
			"threshold", r.cfg.MaxDeletesPerPass,
		)
	} else {
		for _, programID := range toDelete {
			if err := r.mirakc.DeleteSchedule(ctx, programID); err != nil {
				slog.Error("reconciler: deleting schedule", "program_id", programID, "err", err)
				continue
			}
			deleted++
			metrics.ReconcileSchedules.WithLabelValues("deleted").Inc()
		}
	}

	recreated, updateDiff, stateGuarded, limitCarriedOver := r.recreateChanged(ctx, reservations, observedByProgram)

	q := sqlcgen.New(r.pool)
	if err := q.DeleteStaleScheduleSyncs(ctx, sqlcgen.DeleteStaleScheduleSyncsParams{
		Site:       r.site,
		ObservedAt: sweepTime,
	}); err != nil {
		slog.Error("reconciler: cleaning stale schedule_syncs", "err", err)
	}

	if err := r.markOrphaned(ctx, reservations, schedules); err != nil {
		slog.Error("reconciler: marking orphaned", "err", err)
	}

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

	slog.Info("reconciler: pass complete",
		"desired", len(reservations),
		"observed", len(schedules),
		"missing", missing,
		"created", created,
		"stale", len(toDelete),
		"deleted", deleted,
		"update_diff", updateDiff,
		"recreated", recreated,
	)
	return nil
}

func (r *Reconciler) observeSchedules(ctx context.Context, schedules []mirakc.Schedule) error {
	q := sqlcgen.New(r.pool)
	for _, s := range schedules {
		resID, hasTag := mirakc.FindReservationID(s.Tags)
		var reservationID *int64
		if hasTag && resID > 0 {
			_, err := q.GetReservation(ctx, resID)
			if err == nil {
				reservationID = &resID
			}
		}

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
			Site:          r.site,
			ProgramID:     s.Program.ID,
			ReservationID: reservationID,
			State:         s.State,
			Options:       optionsJSON,
			Tags:          tags,
			FailedReason:  failedReasonJSON,
		}); err != nil {
			return fmt.Errorf("upserting schedule_sync for program %d: %w", s.Program.ID, err)
		}
	}
	return nil
}

// desiredReservation は予約行と、そこから解決済みの実効オプション。
// オプションは base（ruler の導出結果）と program_overrides（ユーザーの上書き）
// ・program_intents（ユーザーの record/skip 意図）の合成なので、行だけを
// 持ち回さず解決結果と組で扱う。
type desiredReservation struct {
	res  sqlcgen.Reservation
	opts db.ReservationOptions
}

// listDesired は mirakc への同期対象を返す。
//
// state ではなく effective.skip で絞る（docs/schema.md §3「state を『mirakc への
// 同期対象か』のフィルタに使ってはならない」、docs/recording.md §4.3）。
// state で除外してよいのは orphaned だけ（番組終了後に schedule が観測され
// なかった行で、番組が終わっているので schedule を作る意味がない）。
// ListSyncableReservationsBySite が state <> 'orphaned' で絞るのはこのため —
// 旧 ListActiveReservationsBySite は state = 'active' でしか絞っておらず、
// detached（「実質 manual として動く」はず）の予約に schedule が作られない
// バグの原因だった: 手動予約 → たまたまルールがマッチ（active, rule_id 付き）
// → そのルールを編集して外す（detached）→ 同期対象から外れる、という経路で
// ユーザーの手動予約が黙って録画されなくなっていた（M2-4 で修正）。
func (r *Reconciler) listDesired(ctx context.Context) ([]desiredReservation, error) {
	reservations, err := sqlcgen.New(r.pool).ListSyncableReservationsBySite(ctx, r.site)
	if err != nil {
		return nil, err
	}

	var desired []desiredReservation
	for _, row := range reservations {
		opts, err := db.EffectiveOptions(row.Reservation.Base, row.Overrides, row.IntentAction)
		if err != nil {
			// 壊れた jsonb で mirakc に既定値の schedule を作ってしまわないよう、
			// この予約は同期対象から外してアラートする（握りつぶさない）。
			slog.Error("reconciler: resolving effective options",
				"reservation_id", row.Reservation.ID, "err", err)
			continue
		}
		if opts.Skip != nil && *opts.Skip {
			continue
		}
		desired = append(desired, desiredReservation{res: row.Reservation, opts: opts})
	}
	return desired, nil
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
// service_id は予約行にスナップショットされた値のみを使う。mirakc の programId
// 内部構造（Mirakurun 互換の ID 合成規則）を割り算して推測することはしない
// （不変条件: mirakc 固有の概念を永続テーブルの外で復元しない）。
// NULL は移行前の行で、番組が EPG プロジェクションから既に消えていて
// 00009_reservation_channel.sql の backfill でも埋められなかった残骸。
// 誤った推測で schedule を作るより、同期対象から外してアラートする方が安全。
func resolveContentPath(res sqlcgen.Reservation, opts db.ReservationOptions) (string, error) {
	if res.ServiceID == nil {
		return "", fmt.Errorf("reservation %d (program %d) has no service_id snapshot; "+
			"likely a pre-migration row whose program has already fallen out of the EPG projection",
			res.ID, res.ProgramID)
	}

	// contentPath は filenameTemplate（ruler が base に載せたテンプレート、または
	// ユーザーの明示的な上書き）があればそれを展開し、なければ従来の固定形式
	// （buildContentPath 参照）。ContentPath（フルパスの直接指定）が別途あれば
	// そちらが最終的に勝つ — 展開結果よりユーザーの明示指定を優先する
	// （db.ReservationOptions のドキュメントコメント参照）。
	template := ""
	if opts.FilenameTemplate != nil {
		template = *opts.FilenameTemplate
	}
	contentPath, err := buildContentPath(res, template)
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

	contentPath, err := resolveContentPath(res, opts)
	if err != nil {
		return err
	}

	input := mirakc.ScheduleInput{
		ProgramID: res.ProgramID,
		Options: mirakc.Options{
			ContentPath: &contentPath,
			Priority:    priority,
		},
		Tags: []string{mirakc.ReservationTag(res.ID)},
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
		tagID, hasTag := mirakc.FindReservationID(s.Tags)
		if !hasTag {
			// 自分が作った schedule だけ触る。tag のない schedule は外部産で、
			// 既存の delete ループの ours 判定と揃えてある。
			continue
		}

		wantPriority := effectivePriority(r.cfg.DefaultPriority, d.opts)
		priorityMismatch := s.Options.Priority != wantPriority
		// tag の不一致（reservation id はあるが別の予約を指している = 古い
		// programId の使い回し等で紐付けが食い違っている）も再作成の契機にする。
		// tags は ingest が record と予約を突き合わせるのに使うため、古い tag が
		// 残ると録画が別の予約に紐付く。
		tagMismatch := tagID != d.res.ID
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

	// MaxRecreatesPerPass はサーキットブレーカー（MaxDeletesPerPass）とは別物の
	// 単なるレート制限なので、超えた分は諦めずに次パスへ持ち越すだけ。
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
		cp, err := resolveContentPath(res, opts)
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
		Tags: []string{mirakc.ReservationTag(res.ID)},
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

func (r *Reconciler) markOrphaned(ctx context.Context, reservations []desiredReservation, schedules []mirakc.Schedule) error {
	scheduledPrograms := make(map[int64]struct{}, len(schedules))
	for _, s := range schedules {
		scheduledPrograms[s.Program.ID] = struct{}{}
	}

	q := sqlcgen.New(r.pool)
	for _, d := range reservations {
		res := d.res
		endTime := res.ProgramStartAt.Add(time.Duration(res.ProgramDurationMs) * time.Millisecond)
		if !endTime.Before(time.Now()) {
			continue
		}
		if _, scheduled := scheduledPrograms[res.ProgramID]; scheduled {
			continue
		}
		if err := q.MarkReservationOrphaned(ctx, res.ID); err != nil {
			return fmt.Errorf("marking reservation %d orphaned: %w", res.ID, err)
		}
		slog.Info("reconciler: marked reservation orphaned (program ended)",
			"reservation_id", res.ID,
			"program_id", res.ProgramID,
		)
	}
	return nil
}
