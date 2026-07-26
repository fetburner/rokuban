package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	ReconcileInterval time.Duration
	MaxDeletesPerPass int
	DefaultPriority   int
}

func defaultConfig() Config {
	return Config{
		ReconcileInterval: 30 * time.Second,
		MaxDeletesPerPass: 10,
		DefaultPriority:   10,
	}
}

// Reconciler は予約の desired state と mirakc の observed state を定期的に突き合わせる。
type Reconciler struct {
	site   string
	mirakc *mirakc.Client
	pool   *pgxpool.Pool
	cfg    Config
}

// New は Reconciler を生成する。cfg が nil の場合はデフォルト設定を使う。
func New(site string, mc *mirakc.Client, pool *pgxpool.Pool, cfg *Config) *Reconciler {
	c := defaultConfig()
	if cfg != nil {
		if cfg.ReconcileInterval > 0 {
			c.ReconcileInterval = cfg.ReconcileInterval
		}
		if cfg.MaxDeletesPerPass > 0 {
			c.MaxDeletesPerPass = cfg.MaxDeletesPerPass
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

// Run は reconcile ループを開始し、ctx がキャンセルされるまでブロックする。
func (r *Reconciler) Run(ctx context.Context) error {
	if err := r.reconcile(ctx); err != nil {
		slog.Error("initial reconcile failed", "err", err)
	}

	ticker := time.NewTicker(r.cfg.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := r.reconcile(ctx); err != nil {
				slog.Error("reconcile failed", "err", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context) error {
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
	// 作成/削除できずに残った量が知りたいので、実行した件数ではなく検出した件数。
	metrics.ReconcileLastPass.SetToCurrentTime()
	metrics.ReconcilePendingDiff.WithLabelValues("create").Set(float64(missing))
	metrics.ReconcilePendingDiff.WithLabelValues("delete").Set(float64(len(toDelete)))

	slog.Info("reconciler: pass complete",
		"desired", len(reservations),
		"observed", len(schedules),
		"missing", missing,
		"created", created,
		"stale", len(toDelete),
		"deleted", deleted,
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
// オプションは base（ruler の導出結果）と program_intents（ユーザー意図）の
// 合成なので、行だけを持ち回さず解決結果と組で扱う。
type desiredReservation struct {
	res  sqlcgen.Reservation
	opts db.ReservationOptions
}

func (r *Reconciler) listDesired(ctx context.Context) ([]desiredReservation, error) {
	reservations, err := sqlcgen.New(r.pool).ListActiveReservationsBySite(ctx, r.site)
	if err != nil {
		return nil, err
	}

	var desired []desiredReservation
	for _, row := range reservations {
		opts, err := db.EffectiveOptions(row.Reservation.Base, row.IntentOverrides, row.IntentAction)
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

func (r *Reconciler) createSchedule(ctx context.Context, d desiredReservation) error {
	res, opts := d.res, d.opts

	// service_id は予約行にスナップショットされた値のみを使う。mirakc の programId
	// 内部構造（Mirakurun 互換の ID 合成規則）を割り算して推測することはしない
	// （不変条件: mirakc 固有の概念を永続テーブルの外で復元しない）。
	// NULL は移行前の行で、番組が EPG プロジェクションから既に消えていて
	// 00009_reservation_channel.sql の backfill でも埋められなかった残骸。
	// 誤った推測で schedule を作るより、同期対象から外してアラートする方が安全。
	if res.ServiceID == nil {
		return fmt.Errorf("reservation %d (program %d) has no service_id snapshot; "+
			"likely a pre-migration row whose program has already fallen out of the EPG projection",
			res.ID, res.ProgramID)
	}

	priority := r.cfg.DefaultPriority
	if opts.Priority != nil {
		priority = *opts.Priority
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
		return fmt.Errorf("building content path for reservation %d (program %d): %w",
			res.ID, res.ProgramID, err)
	}
	if opts.ContentPath != nil && *opts.ContentPath != "" {
		contentPath = contentpath.SanitizeContentPath(*opts.ContentPath)
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
