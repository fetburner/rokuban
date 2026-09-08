package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/schedulesync"
)

// presyncQueryTimeout は presync collector が scrape を占有する上限。
// Prometheus の scrape timeout（既定 10 秒）より短くする。
const presyncQueryTimeout = 5 * time.Second

// reasonMissing / reasonOptions / reasonOptionsDeferred は presync が数える
// 不一致の理由。
const (
	reasonMissing         = "missing"
	reasonOptions         = "options"
	reasonOptionsDeferred = "options_deferred"
)

// PresyncCollector は 1 サイトの desired reservation と schedule_sync の
// observed を scrape ごとに突き合わせる DB-backed collector。
//
// reconciler のプロセス内ゲージではなく DB を読むので、reconciler が River の
// ScaledJob で起動して終了する構成でも常駐プロセスの /metrics から同じ値を
// 観測できる。snapshot marker は schedule_sync の全量 upsert / stale 削除が
// 同一トランザクションでコミットされたときだけ更新される。
type PresyncCollector struct {
	pool *pgxpool.Pool
	site string

	pending  *prometheus.Desc
	earliest *prometheus.Desc
	snapshot *prometheus.Desc
	errors   prometheus.Counter
}

// NewPresyncCollector はサイト単位の presync collector を作る。
func NewPresyncCollector(pool *pgxpool.Pool, site string) *PresyncCollector {
	labels := prometheus.Labels{"site": site}
	return &PresyncCollector{
		pool: pool,
		site: site,
		pending: prometheus.NewDesc(
			"rokuban_presync_pending",
			"Desired reservations not observed with the expected schedule state, by reason.",
			[]string{"reason"}, labels,
		),
		earliest: prometheus.NewDesc(
			"rokuban_presync_pending_earliest_start_timestamp_seconds",
			"Earliest program start time among the pending desired reservations for this reason.",
			[]string{"reason"}, labels,
		),
		snapshot: prometheus.NewDesc(
			"rokuban_schedule_snapshot_last_success_timestamp_seconds",
			"Unix time of the last committed full schedule snapshot for this site. Zero means no snapshot has completed.",
			nil, labels,
		),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "rokuban_presync_scrape_errors_total",
			Help:        "Failures while querying the presync state during a scrape.",
			ConstLabels: labels,
		}),
	}
}

// Describe は prometheus.Collector を満たす。
func (c *PresyncCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.earliest
	ch <- c.snapshot
	c.errors.Describe(ch)
}

// Collect は desired/observed の現在値を DB から再読する。
//
// DB の取得に失敗した場合は pending や snapshot を 0 として出さない。0 は
// 「未同期なし」または「一度も snapshot が成功していない」と読める値であり、
// 障害を隠すためである。失敗時は専用のエラーカウンタだけを出す。
func (c *PresyncCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), presyncQueryTimeout)
	defer cancel()

	result, err := c.read(ctx)
	if err != nil {
		slog.Error("metrics: querying presync state", "site", c.site, "err", err)
		c.errors.Inc()
		c.errors.Collect(ch)
		return
	}

	for _, reason := range []string{reasonMissing, reasonOptions, reasonOptionsDeferred} {
		ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue,
			float64(result.pending[reason]), reason)
		// pending が 0 の reason は系列を出さない。0 を出すと
		// `earliest - time() < lead` が常に真になり、健全な状態で鳴る。
		if at, ok := result.earliest[reason]; ok {
			ch <- prometheus.MustNewConstMetric(c.earliest, prometheus.GaugeValue,
				float64(at.UnixNano())/float64(time.Second), reason)
		}
	}
	ch <- prometheus.MustNewConstMetric(
		c.snapshot, prometheus.GaugeValue, result.snapshotAt,
	)
	c.errors.Collect(ch)
}

type presyncResult struct {
	pending    map[string]int
	earliest   map[string]time.Time
	snapshotAt float64
}

// add は reason ごとの pending 件数と、その reason の中で最も開始が早い
// 予約の start_at を更新する。
func (r *presyncResult) add(reason string, startAt time.Time) {
	r.pending[reason]++
	if cur, ok := r.earliest[reason]; !ok || startAt.Before(cur) {
		r.earliest[reason] = startAt
	}
}

func (c *PresyncCollector) read(ctx context.Context) (presyncResult, error) {
	q := sqlcgen.New(c.pool)

	result := presyncResult{
		pending:  make(map[string]int, 3),
		earliest: make(map[string]time.Time, 3),
	}
	snapshotAt, err := q.GetScheduleSyncSnapshot(ctx, c.site)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return presyncResult{}, fmt.Errorf("reading schedule snapshot marker: %w", err)
		}
	} else {
		result.snapshotAt = float64(snapshotAt.UnixNano()) / float64(time.Second)
	}

	observedRows, err := q.ListScheduleSyncsBySite(ctx, c.site)
	if err != nil {
		return presyncResult{}, fmt.Errorf("listing schedule observations: %w", err)
	}
	type observedSchedule struct {
		state   string
		options mirakc.Options
		tags    []string
	}
	observed := make(map[int64]observedSchedule, len(observedRows))
	for _, row := range observedRows {
		var options mirakc.Options
		if err := json.Unmarshal(row.Options, &options); err != nil {
			return presyncResult{}, fmt.Errorf("unmarshalling observed options for program %d: %w", row.ProgramID, err)
		}
		observed[row.ProgramID] = observedSchedule{
			state:   row.State,
			options: options,
			tags:    row.Tags,
		}
	}

	rows, err := q.ListReservationsForSyncEvaluation(ctx, c.site)
	if err != nil {
		return presyncResult{}, fmt.Errorf("listing desired reservations: %w", err)
	}
	now := time.Now()
	for _, candidate := range reservation.EvaluateSyncCandidates(rows) {
		if candidate.Err != nil {
			return presyncResult{}, candidate.Err
		}
		if candidate.Skipped || schedulesync.ProgramEnded(
			candidate.Snapshot.StartAt,
			candidate.Snapshot.DurationMs,
			now,
		) {
			continue
		}

		programID := candidate.Reservation.ProgramID
		observedSchedule, ok := observed[programID]
		if !ok {
			result.add(reasonMissing, candidate.Snapshot.StartAt)
			continue
		}

		diff, owned := schedulesync.CompareOptions(
			programID,
			candidate.Options,
			schedulesync.DefaultPriority,
			observedSchedule.options,
			observedSchedule.tags,
		)
		if owned && diff.Any() {
			reason := reasonOptions
			if !schedulesync.IsRecreateAllowed(observedSchedule.state) {
				reason = reasonOptionsDeferred
			}
			result.add(reason, candidate.Snapshot.StartAt)
		}
	}

	return result, nil
}
