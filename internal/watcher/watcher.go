package watcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/webhook"
)

// DefaultSite はデフォルトの mirakc サイト名。定義は db.DefaultSite（唯一の出所）。
const DefaultSite = db.DefaultSite

// Watcher は mirakc の SSE イベントを購読し、録画の状態変化を DB に反映する。
type Watcher struct {
	site     string
	mirakc   *mirakc.Client
	records  store.RecordObservations
	markers  store.LoopMarkers
	enqueuer jobq.Enqueuer
	webhook  *webhook.Client
	services []mirakc.Service
}

// New は Watcher を生成する。enqueuer は ingest 投入に使う。webhook は任意（nil 可）で、
// 録画 finished / failed の通知に使う（M3-11）。
func New(site string, mc *mirakc.Client, records store.RecordObservations, markers store.LoopMarkers, enqueuer jobq.Enqueuer, wh *webhook.Client) *Watcher {
	return &Watcher{
		site:     site,
		mirakc:   mc,
		records:  records,
		markers:  markers,
		enqueuer: enqueuer,
		webhook:  wh,
	}
}

// Run は SSE 購読を開始し、ctx がキャンセルされるまでブロックする。
//
// M2-18 で定期の全量突き合わせ（3 段構えの (c)、docs/recording.md §3.3）を
// record_sweep ジョブ（internal/worker.RecordSweepWorker）に切り出したため、
// Watcher は SSE 購読と handleEvent だけの常駐になった。真実（レベルトリガー）は
// ジョブ側にあり、Watcher はヒント源（(a) record-saved の即時反映 / (b) 接続時の
// 全 record 再送）に徹する。
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
	for {
		select {
		case ev := <-events:
			w.handleEvent(ctx, ev)
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

// beforeSweepProcessRecords は Sweep が stale 削除の直後、snapshot の
// processRecord の直前に呼ぶテスト用フック。本番では nil。
var beforeSweepProcessRecords func()

// Sweep は mirakc の schedules / records API を取得し、DB（record_sync /
// recordings）と突き合わせる。3 段構えの (c)（docs/recording.md §3.3）にあたる
// レベルトリガーの真実で、SSE のヒント（(a)(b)）を取りこぼしても収束させる。
//
// 成功 marker を進める条件は、ListSchedules の成功、全 failed schedule の処理成功、
// ListRecords の成功、全 record の処理成功である。recordless な失敗は schedules API
// からしか復元できないため、ListSchedules の失敗も成功に数えない。ListServices による
// キャッシュ更新の失敗は条件に含めず、前回のキャッシュを使う。サービスが見つからない
// failed schedule は processFailedSchedule の失敗として報告される。
//
// 失敗があっても残りの処理を続け、最後に errors.Join で返す。決定的に失敗する record
// があれば marker は進まないが、取りこぼしを成功扱いするよりアラートで検知できる方を
// 選ぶ。record ID を含む既存ログから原因を特定できる。部分失敗後の継続と marker 非更新は
// TestSweep_PartialRecordFailureDoesNotAdvanceMarker、
// TestSweep_ListSchedulesFailureDoesNotAdvanceMarker、
// TestSweep_FailedScheduleProcessingDoesNotAdvanceMarker が検証する。
//
// M2-18 で record_sweep ジョブ（internal/worker.RecordSweepWorker）から呼ばれる形に
// 公開メソッドとして切り出した。processRecord は M2-16 で record_sync の
// (site, record_id) 行ロックにより冪等化されているため、SSE 由来の handleEvent と
// このメソッドが同一 record を並行処理しても recordings は重複しない
// （docs/recording.md §3.3「record 処理は並行実行しても問題が起きない」）。
func (w *Watcher) Sweep(ctx context.Context) error {
	slog.Info("watcher sweep started")
	var errs []error

	if services, err := w.mirakc.ListServices(ctx); err != nil {
		slog.Error("refreshing service cache", "err", err)
	} else {
		w.services = services
	}

	// recordless な recording.failed は records API には出ないが、失敗中の
	// schedule に failedReason が残っている間は schedules API から再構成できる。
	// schedules の取得に失敗しても records の sweep は継続する。mirakc の版差や
	// 一時的な API 障害で、すでに保存済みの record まで取りこぼさないため。
	if schedules, err := w.mirakc.ListSchedules(ctx); err != nil {
		slog.Error("listing schedules for failed recordings", "err", err)
		errs = append(errs, fmt.Errorf("listing schedules: %w", err))
	} else {
		for _, schedule := range schedules {
			if err := w.processFailedSchedule(ctx, schedule); err != nil {
				slog.Error("sweep: processing failed schedule",
					"program_id", schedule.Program.ID, "err", err)
				errs = append(errs, fmt.Errorf("processing failed schedule for program_id %d: %w", schedule.Program.ID, err))
			}
		}
	}

	records, err := w.mirakc.ListRecords(ctx)
	if err != nil {
		return fmt.Errorf("listing records: %w", err)
	}
	recordIDs := make([]string, 0, len(records))
	for _, record := range records {
		recordIDs = append(recordIDs, record.ID)
	}
	// stale 削除は snapshot の processRecord より前に実行する。後に置くと、
	// ListRecords 後に SSE が作った行をこの削除が巻き込む。
	stale, err := w.records.DeleteStaleRecordSyncs(ctx, w.site, recordIDs)
	if err != nil {
		return fmt.Errorf("deleting stale record_syncs: %w", err)
	}
	if beforeSweepProcessRecords != nil {
		beforeSweepProcessRecords()
	}
	for _, record := range records {
		if err := w.processRecord(ctx, record); err != nil {
			slog.Error("sweep: processing record", "record_id", record.ID, "err", err)
			errs = append(errs, fmt.Errorf("processing record %q: %w", record.ID, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("sweep incomplete: %w", errors.Join(errs...))
	}
	if err := w.markers.MarkRecordSweep(ctx, w.site); err != nil {
		return fmt.Errorf("marking record sweep success: %w", err)
	}
	slog.Info("watcher sweep complete", "records", len(records), "stale", stale)
	metrics.SweepLastPass.SetToCurrentTime()
	return nil
}

func (w *Watcher) processRecord(ctx context.Context, record mirakc.Record) error {
	observation, err := recordObservation(w.site, record)
	if err != nil {
		return err
	}
	result, err := w.records.ProcessRecord(ctx, observation)
	if err != nil {
		return fmt.Errorf("processing record observation: %w", err)
	}
	if result.FailedQualityEventAdded && record.Recording.FailedReason != nil {
		metrics.RecordingsFailed.WithLabelValues(failureReason(*record.Recording.FailedReason)).Inc()
	}
	if result.EnqueueIngest {
		var opts []jobq.EnqueueOpt
		if result.IngestPriority != 0 {
			opts = append(opts, jobq.Priority(result.IngestPriority))
		}
		if _, err := w.enqueuer.Enqueue(ctx, jobs.IngestJobArgs{Site: w.site, RecordID: record.ID}, opts...); err != nil {
			slog.Error("enqueuing ingest job after record commit", "site", w.site, "record_id", record.ID, "err", err)
		}
	}

	// DB が永続化した後に通知。失敗しても本処理は成功扱い（M3-11）。
	if result.RecordingID != nil && record.Recording.Status == db.RecordingStatusFinished && result.PreviousStatus != db.RecordingStatusFinished {
		w.notify(ctx, webhook.Event{
			Type:        webhook.EventRecordingFinished,
			RecordingID: *result.RecordingID,
			Site:        w.site,
			Title:       result.Title,
			Status:      db.RecordingStatusFinished,
		})
	}

	return nil
}

// processFailedSchedule は schedules API に残っている recordless failed を
// recordings に再構成する。SSE の recording.failed と同じ active-event に収束させ、
// 周期 sweep では同じ event + reason を一度だけ quality_events に追記する。
func (w *Watcher) processFailedSchedule(ctx context.Context, schedule mirakc.Schedule) error {
	if schedule.State != db.RecordingStatusFailed || schedule.FailedReason == nil {
		return nil
	}

	// schedules API には mirakc 全体の schedule が返る。Rokuban が観測すべき
	// スケジュールかは、recording.failed の SSE 経路と同じく reservation の存在で
	// 境界を引く。古い mirakc が tags を返さない場合も回収できる。
	// 予約の無い番組はサービス詳細の問い合わせも行わない。store は書き込み tx 内で予約を再確認する。
	hasReservation, err := w.records.HasReservation(ctx, w.site, schedule.Program.ID)
	if err != nil {
		return fmt.Errorf("checking reservation for failed schedule: %w", err)
	}
	if !hasReservation {
		return nil
	}

	service, err := w.findService(ctx, schedule.Program.NetworkID, schedule.Program.ServiceID)
	if err != nil {
		return fmt.Errorf("finding service: %w", err)
	}
	reasonJSON, err := json.Marshal(schedule.FailedReason)
	if err != nil {
		return fmt.Errorf("marshalling failed schedule reason: %w", err)
	}
	snapshot := snapshotFromSchedule(schedule, *service)
	result, err := w.records.ProcessFailedSchedule(ctx, store.FailedScheduleObservation{
		Site: w.site, ProgramID: schedule.Program.ID,
		Snapshot: snapshot.recordingSnapshot(schedule.Program.NetworkID, schedule.Program.ServiceID, schedule.Program.EventID),
		Reason:   reasonJSON,
	})
	if err != nil {
		return fmt.Errorf("processing failed schedule: %w", err)
	}
	if result.FailedQualityEventAdded {
		metrics.RecordingsFailed.WithLabelValues(failureReason(*schedule.FailedReason)).Inc()
		if result.RecordingID != nil {
			w.notify(ctx, webhook.Event{
				Type: webhook.EventRecordingFailed, RecordingID: *result.RecordingID,
				Site: w.site, Title: result.Title, Status: db.RecordingStatusFailed,
			})
		}
	}
	return nil
}

func (w *Watcher) handleRecordingFailed(ctx context.Context, data mirakc.RecordingFailedData) error {
	// 観測した時点で数える。予約の照会や mirakc への問い合わせより後に置くと、
	// それらが失敗したときに取りこぼす（物事がうまくいっていないときこそ数えたい）。
	metrics.RecordingsFailed.WithLabelValues(failureReason(data.Reason)).Inc()

	hasReservation, err := w.records.HasReservation(ctx, w.site, data.ProgramID)
	if err != nil {
		return fmt.Errorf("looking up reservation: %w", err)
	}
	if !hasReservation {
		slog.Debug("no reservation for failed recording", "program_id", data.ProgramID)
		return nil
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
	snapshot := snapshotFromSchedule(*schedule, *service)
	recordingID, recorded, err := w.records.RecordFailedRecording(ctx, store.FailedRecordingObservation{
		Site: w.site, ProgramID: data.ProgramID,
		Snapshot: snapshot.recordingSnapshot(schedule.Program.NetworkID, schedule.Program.ServiceID, schedule.Program.EventID),
		Reason:   reasonJSON,
	})
	if err != nil {
		return fmt.Errorf("recording failed event: %w", err)
	}
	if !recorded {
		// 予約が照会の後に消えていた。
		return nil
	}
	w.notify(ctx, webhook.Event{
		Type: webhook.EventRecordingFailed, RecordingID: recordingID,
		Site: w.site, Title: snapshot.title, Status: db.RecordingStatusFailed,
	})
	return nil
}

// notify は webhook を送る。失敗はログのみ（本処理を止めない。M3-11）。
func (w *Watcher) notify(ctx context.Context, ev webhook.Event) {
	if w.webhook == nil {
		return
	}
	if err := w.webhook.Notify(ctx, ev); err != nil {
		slog.Error("webhook notify failed",
			"type", ev.Type, "recording_id", ev.RecordingID, "site", ev.Site, "err", err)
	}
}

func (w *Watcher) handleRecordBroken(ctx context.Context, data mirakc.RecordBrokenData) error {
	metrics.RecordsBroken.WithLabelValues(brokenReason(data.Reason)).Inc()
	appended, err := w.records.AppendRecordBrokenEvent(ctx, w.site, data.RecordID, data.Reason)
	if err != nil {
		return fmt.Errorf("appending record-broken event: %w", err)
	}
	if !appended {
		slog.Debug("record-broken event has no linked recording", "record_id", data.RecordID)
	}
	return nil
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
