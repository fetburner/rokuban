package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"golang.org/x/sync/errgroup"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/worker"
)

// DefaultSite はデフォルトの mirakc サイト名。定義は db.DefaultSite（唯一の出所）。
const DefaultSite = db.DefaultSite

// Config は Watcher の設定。
type Config struct {
	ReconcileInterval time.Duration
}

func defaultConfig() Config {
	return Config{
		ReconcileInterval: 5 * time.Minute,
	}
}

// Watcher は mirakc の SSE イベントを購読し、録画の状態変化を DB に反映する。
type Watcher struct {
	site     string
	mirakc   *mirakc.Client
	pool     *pgxpool.Pool
	river    *river.Client[pgx5.Tx]
	cfg      Config
	services []mirakc.Service
}

// New は Watcher を生成する。cfg が nil の場合はデフォルト設定を使う。
func New(site string, mc *mirakc.Client, pool *pgxpool.Pool, rc *river.Client[pgx5.Tx], cfg *Config) *Watcher {
	c := defaultConfig()
	if cfg != nil {
		if cfg.ReconcileInterval > 0 {
			c.ReconcileInterval = cfg.ReconcileInterval
		}
	}
	return &Watcher{
		site:   site,
		mirakc: mc,
		pool:   pool,
		river:  rc,
		cfg:    c,
	}
}

// Run は SSE 購読と定期 reconcile を開始し、ctx がキャンセルされるまでブロックする。
func (w *Watcher) Run(ctx context.Context) error {
	events := make(chan mirakc.Event, 64)
	eg, egCtx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		return w.mirakc.Subscribe(egCtx, events, nil)
	})

	eg.Go(func() error {
		return w.eventLoop(egCtx, events)
	})

	return eg.Wait()
}

func (w *Watcher) eventLoop(ctx context.Context, events <-chan mirakc.Event) error {
	if err := w.reconcile(ctx); err != nil {
		slog.Error("initial reconcile failed", "err", err)
	}

	ticker := time.NewTicker(w.cfg.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case ev := <-events:
			w.handleEvent(ctx, ev)
		case <-ticker.C:
			if err := w.reconcile(ctx); err != nil {
				slog.Error("reconcile failed", "err", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (w *Watcher) handleEvent(ctx context.Context, ev mirakc.Event) {
	switch ev.Type {
	case "recording.record-saved":
		var data mirakc.RecordSavedData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			slog.Error("unmarshalling record-saved event", "err", err)
			return
		}
		record, err := w.mirakc.GetRecord(ctx, data.RecordID)
		if err != nil {
			slog.Error("fetching record", "record_id", data.RecordID, "err", err)
			return
		}
		if err := w.processRecord(ctx, *record); err != nil {
			slog.Error("processing record-saved", "record_id", data.RecordID, "err", err)
		}

	case "recording.failed":
		var data mirakc.RecordingFailedData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			slog.Error("unmarshalling recording.failed event", "err", err)
			return
		}
		if err := w.handleRecordingFailed(ctx, data); err != nil {
			slog.Error("handling recording.failed", "program_id", data.ProgramID, "err", err)
		}

	case "recording.record-broken":
		var data mirakc.RecordBrokenData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			slog.Error("unmarshalling record-broken event", "err", err)
			return
		}
		if err := w.handleRecordBroken(ctx, data); err != nil {
			slog.Error("handling record-broken", "record_id", data.RecordID, "err", err)
		}
	}
}

func (w *Watcher) reconcile(ctx context.Context) error {
	slog.Info("watcher reconcile started")

	if services, err := w.mirakc.ListServices(ctx); err != nil {
		slog.Error("refreshing service cache", "err", err)
	} else {
		w.services = services
	}

	records, err := w.mirakc.ListRecords(ctx)
	if err != nil {
		return fmt.Errorf("listing records: %w", err)
	}
	for _, record := range records {
		if err := w.processRecord(ctx, record); err != nil {
			slog.Error("reconcile: processing record", "record_id", record.ID, "err", err)
		}
	}
	slog.Info("watcher reconcile complete", "records", len(records))
	return nil
}

func (w *Watcher) processRecord(ctx context.Context, record mirakc.Record) error {
	reservationID, hasTag := mirakc.FindReservationID(record.Tags)

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)

	// record_sync の (site, record_id) 行を先に確保して行ロックを取る（M2-16）。
	// これにより同一 record を並行処理しても、後発は先発のコミットまで待たされ、
	// 待った後は recording_id が埋まった状態を見るので recordings の二重作成が起きない。
	// AcquireRecordSync は行がなければ recording_id = NULL で新規作成するだけで、
	// 既存行があってもその内容を書き換えない。
	existingRecordingID, err := q.AcquireRecordSync(ctx, sqlcgen.AcquireRecordSyncParams{
		Site:      w.site,
		RecordID:  record.ID,
		ProgramID: record.Program.ID,
		Status:    record.Recording.Status,
	})
	if err != nil {
		return fmt.Errorf("acquiring record_sync: %w", err)
	}

	var recordingID *int64

	if existingRecordingID != nil {
		recordingID = existingRecordingID
		if err := w.updateRecordingStatus(ctx, q, *recordingID, record); err != nil {
			return fmt.Errorf("updating recording status: %w", err)
		}
	} else if hasTag {
		id, createErr := w.createRecording(ctx, q, reservationID, record)
		if createErr != nil {
			return fmt.Errorf("creating recording: %w", createErr)
		}
		recordingID = &id
	}

	if err := w.upsertRecordSync(ctx, q, record, recordingID); err != nil {
		return fmt.Errorf("upserting record_sync: %w", err)
	}

	if record.Recording.Status == "finished" && recordingID != nil {
		if _, err := w.river.InsertTx(ctx, tx, worker.IngestJobArgs{
			Site:     w.site,
			RecordID: record.ID,
		}, nil); err != nil {
			return fmt.Errorf("enqueuing ingest job: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

func (w *Watcher) createRecording(ctx context.Context, q *sqlcgen.Queries, reservationID int64, record mirakc.Record) (int64, error) {
	var resID *int64
	var ruleID *int64
	source := "manual"

	res, err := q.GetReservation(ctx, reservationID)
	if err != nil && !errors.Is(err, pgx5.ErrNoRows) {
		return 0, fmt.Errorf("looking up reservation %d: %w", reservationID, err)
	}
	if err == nil {
		resID = &res.ID
		ruleID = res.RuleID
		source = res.Source
	}

	id, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		ReservationID:     resID,
		RuleID:            ruleID,
		Source:            source,
		Site:              w.site,
		NetworkID:         int32(record.Program.NetworkID),
		ServiceID:         int32(record.Program.ServiceID),
		EventID:           int32(record.Program.EventID),
		ServiceName:       record.Service.Name,
		ChannelType:       record.Service.Channel.Type,
		Channel:           record.Service.Channel.Channel,
		Title:             derefStr(record.Program.Name),
		Description:       record.Program.Description,
		Extended:          marshalJSONOrNull(record.Program.Extended),
		Genres:            marshalJSONOrNull(record.Program.Genres),
		IsFree:            record.Program.IsFree,
		ProgramStartAt:    millisToTime(record.Program.StartAt),
		ProgramDurationMs: derefInt64(record.Program.Duration),
		Status:            record.Recording.Status,
		StartedAt:         millisToTimeNonNil(record.Recording.StartTime),
		EndedAt:           millisToTimePtr(record.Recording.EndTime),
	})
	return id, err
}

func (w *Watcher) updateRecordingStatus(ctx context.Context, q *sqlcgen.Queries, recordingID int64, record mirakc.Record) error {
	return q.UpdateRecordingStatus(ctx, sqlcgen.UpdateRecordingStatusParams{
		ID:        recordingID,
		NewStatus: record.Recording.Status,
		StartedAt: millisToTimeNonNil(record.Recording.StartTime),
		EndedAt:   millisToTimePtr(record.Recording.EndTime),
	})
}

func (w *Watcher) upsertRecordSync(ctx context.Context, q *sqlcgen.Queries, record mirakc.Record, recordingID *int64) error {
	tags := record.Tags
	if tags == nil {
		tags = []string{}
	}

	var failedReasonJSON json.RawMessage
	if record.Recording.FailedReason != nil {
		data, err := json.Marshal(record.Recording.FailedReason)
		if err != nil {
			return fmt.Errorf("marshalling failed_reason: %w", err)
		}
		failedReasonJSON = data
	}

	return q.UpsertRecordSync(ctx, sqlcgen.UpsertRecordSyncParams{
		Site:          w.site,
		RecordID:      record.ID,
		RecordingID:   recordingID,
		ProgramID:     record.Program.ID,
		Status:        record.Recording.Status,
		ContentPath:   contentPathPtr(record.Content.Path),
		ContentLength: contentLengthPtr(record.Content.Length),
		Tags:          tags,
		FailedReason:  failedReasonJSON,
	})
}

func (w *Watcher) handleRecordingFailed(ctx context.Context, data mirakc.RecordingFailedData) error {
	// 観測した時点で数える。予約の照会や mirakc への問い合わせより後に置くと、
	// それらが失敗したときに取りこぼす（物事がうまくいっていないときこそ数えたい）。
	metrics.RecordingsFailed.WithLabelValues(failureReason(data.Reason)).Inc()

	q := sqlcgen.New(w.pool)

	res, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{
		Site:      w.site,
		ProgramID: data.ProgramID,
	})
	if errors.Is(err, pgx5.ErrNoRows) {
		slog.Debug("no reservation for failed recording", "program_id", data.ProgramID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("looking up reservation: %w", err)
	}

	schedule, err := w.mirakc.GetSchedule(ctx, data.ProgramID)
	if err != nil {
		return fmt.Errorf("getting schedule: %w", err)
	}

	service, err := w.findService(ctx, schedule.Program.NetworkID, schedule.Program.ServiceID)
	if err != nil {
		return fmt.Errorf("finding service: %w", err)
	}

	reasonJSON, err := json.Marshal(data.Reason)
	if err != nil {
		return fmt.Errorf("marshalling failed reason: %w", err)
	}
	qe := db.QualityEvent{
		At:     time.Now(),
		Event:  "recording.failed",
		Reason: reasonJSON,
	}
	qeJSON, err := json.Marshal([]db.QualityEvent{qe})
	if err != nil {
		return fmt.Errorf("marshalling quality events: %w", err)
	}

	return q.CreateFailedRecording(ctx, sqlcgen.CreateFailedRecordingParams{
		ReservationID:     &res.ID,
		RuleID:            res.RuleID,
		Source:            res.Source,
		Site:              w.site,
		NetworkID:         int32(schedule.Program.NetworkID),
		ServiceID:         int32(schedule.Program.ServiceID),
		EventID:           int32(schedule.Program.EventID),
		ServiceName:       service.Name,
		ChannelType:       service.Channel.Type,
		Channel:           service.Channel.Channel,
		Title:             derefStr(schedule.Program.Name),
		Description:       schedule.Program.Description,
		Extended:          marshalJSONOrNull(schedule.Program.Extended),
		Genres:            marshalJSONOrNull(schedule.Program.Genres),
		IsFree:            schedule.Program.IsFree,
		ProgramStartAt:    millisToTime(schedule.Program.StartAt),
		ProgramDurationMs: derefInt64(schedule.Program.Duration),
		QualityEvents:     qeJSON,
	})
}

func (w *Watcher) handleRecordBroken(ctx context.Context, data mirakc.RecordBrokenData) error {
	metrics.RecordsBroken.WithLabelValues(brokenReason(data.Reason)).Inc()

	q := sqlcgen.New(w.pool)

	recordingID, err := q.GetRecordSyncRecordingID(ctx, sqlcgen.GetRecordSyncRecordingIDParams{
		Site:     w.site,
		RecordID: data.RecordID,
	})
	if err != nil {
		if errors.Is(err, pgx5.ErrNoRows) {
			slog.Debug("no record_sync entry for record-broken", "record_id", data.RecordID)
			return nil
		}
		return fmt.Errorf("looking up record_sync: %w", err)
	}
	if recordingID == nil {
		slog.Debug("untracked record for record-broken", "record_id", data.RecordID)
		return nil
	}

	reasonJSON, err := json.Marshal(map[string]string{"reason": data.Reason})
	if err != nil {
		return fmt.Errorf("marshalling broken reason: %w", err)
	}
	qe := db.QualityEvent{
		At:     time.Now(),
		Event:  "recording.record-broken",
		Reason: reasonJSON,
	}
	qeJSON, err := json.Marshal([]db.QualityEvent{qe})
	if err != nil {
		return fmt.Errorf("marshalling quality events: %w", err)
	}

	return q.AppendQualityEvents(ctx, sqlcgen.AppendQualityEventsParams{
		ID:     *recordingID,
		Events: qeJSON,
	})
}

func (w *Watcher) findService(ctx context.Context, networkID, serviceID int) (*mirakc.Service, error) {
	if len(w.services) == 0 {
		services, err := w.mirakc.ListServices(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing services: %w", err)
		}
		w.services = services
	}
	for i, s := range w.services {
		if s.NetworkID == networkID && s.ServiceID == serviceID {
			return &w.services[i], nil
		}
	}
	return nil, fmt.Errorf("service not found: network=%d service=%d", networkID, serviceID)
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt64(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

func millisToTime(m *mirakc.Milliseconds) time.Time {
	if m == nil {
		return time.Time{}
	}
	return m.Time()
}

func millisToTimeNonNil(m mirakc.Milliseconds) *time.Time {
	t := m.Time()
	if t.IsZero() {
		return nil
	}
	return &t
}

func millisToTimePtr(m *mirakc.Milliseconds) *time.Time {
	if m == nil {
		return nil
	}
	t := m.Time()
	return &t
}

func marshalJSONOrNull(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil || string(data) == "null" {
		return nil
	}
	return data
}

func contentLengthPtr(v *uint64) *int64 {
	if v == nil {
		return nil
	}
	n := int64(*v)
	return &n
}

func contentPathPtr(path string) *string {
	if path == "" {
		return nil
	}
	return &path
}

// failureReason は recording.failed のラベル値を返す。
// mirakc の FailedReason.type は値域が有界なのでラベルに使える。
func failureReason(reason mirakc.FailedReason) string {
	if reason.Type == "" {
		return "unknown"
	}
	return reason.Type
}

// brokenReason は record-broken のラベル値を返す。
func brokenReason(reason string) string {
	if reason == "" {
		return "unknown"
	}
	return reason
}
