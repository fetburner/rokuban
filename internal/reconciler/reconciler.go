package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

	for _, res := range reservations {
		if _, exists := observedByProgram[res.ProgramID]; exists {
			continue
		}
		if err := r.createSchedule(ctx, res); err != nil {
			slog.Error("reconciler: creating schedule", "reservation_id", res.ID, "program_id", res.ProgramID, "err", err)
			continue
		}
		created++
		metrics.ReconcileSchedules.WithLabelValues("created").Inc()
	}

	desiredPrograms := make(map[int64]struct{}, len(reservations))
	for _, res := range reservations {
		desiredPrograms[res.ProgramID] = struct{}{}
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

	slog.Info("reconciler: pass complete",
		"desired", len(reservations),
		"observed", len(schedules),
		"created", created,
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

func (r *Reconciler) listDesired(ctx context.Context) ([]sqlcgen.Reservation, error) {
	reservations, err := sqlcgen.New(r.pool).ListActiveReservationsBySite(ctx, r.site)
	if err != nil {
		return nil, err
	}

	var desired []sqlcgen.Reservation
	for _, res := range reservations {
		var opts db.ReservationOptions
		if len(res.Overrides) > 0 {
			if err := json.Unmarshal(res.Overrides, &opts); err != nil {
				slog.Error("reconciler: unmarshalling overrides", "reservation_id", res.ID, "err", err)
				continue
			}
		}
		if opts.Skip != nil && *opts.Skip {
			continue
		}
		desired = append(desired, res)
	}
	return desired, nil
}

func (r *Reconciler) createSchedule(ctx context.Context, res sqlcgen.Reservation) error {
	var opts db.ReservationOptions
	if len(res.Overrides) > 0 {
		_ = json.Unmarshal(res.Overrides, &opts)
	}

	priority := r.cfg.DefaultPriority
	if opts.Priority != nil {
		priority = *opts.Priority
	}

	serviceID := int((res.ProgramID / 100000) % 100000)
	contentPath := generateContentPath(res.Title, res.ProgramStartAt, serviceID)
	if opts.ContentPath != nil && *opts.ContentPath != "" {
		contentPath = sanitizeContentPath(*opts.ContentPath)
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

func (r *Reconciler) markOrphaned(ctx context.Context, reservations []sqlcgen.Reservation, schedules []mirakc.Schedule) error {
	scheduledPrograms := make(map[int64]struct{}, len(schedules))
	for _, s := range schedules {
		scheduledPrograms[s.Program.ID] = struct{}{}
	}

	q := sqlcgen.New(r.pool)
	for _, res := range reservations {
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
