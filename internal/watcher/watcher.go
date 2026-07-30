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
	"github.com/fetburner/rokuban/internal/webhook"
)

// DefaultSite はデフォルトの mirakc サイト名。定義は db.DefaultSite（唯一の出所）。
const DefaultSite = db.DefaultSite

// IngestArgsFunc は ingest ジョブの引数（River の JobArgs）を組み立てる関数。
//
// 具体型は internal/worker.IngestJobArgs だが、internal/watcher はそれを直接
// import できない。record_sweep ジョブ（internal/worker.RecordSweepWorker）が
// このパッケージの Watcher.Sweep を呼ぶため、逆方向に internal/watcher →
// internal/worker の import を残すと循環インポートになる（M2-18）。
// そのため呼び出し元（cmd/rokuban と RecordSweepWorker）が具体型を注入する。
type IngestArgsFunc func(site, recordID string) river.JobArgs

// Watcher は mirakc の SSE イベントを購読し、録画の状態変化を DB に反映する。
type Watcher struct {
	site          string
	mirakc        *mirakc.Client
	pool          *pgxpool.Pool
	river         *river.Client[pgx5.Tx]
	newIngestArgs IngestArgsFunc
	webhook       *webhook.Client
	services      []mirakc.Service
}

// New は Watcher を生成する。newIngestArgs は ingest ジョブの引数を組み立てる関数で、
// 呼び出し元が internal/worker.NewIngestArgs を渡す想定（IngestArgsFunc のコメント参照）。
// webhook は任意（nil 可）。録画 finished / failed の通知に使う（M3-11）。
func New(site string, mc *mirakc.Client, pool *pgxpool.Pool, rc *river.Client[pgx5.Tx], newIngestArgs IngestArgsFunc, wh *webhook.Client) *Watcher {
	return &Watcher{
		site:          site,
		mirakc:        mc,
		pool:          pool,
		river:         rc,
		newIngestArgs: newIngestArgs,
		webhook:       wh,
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

// Sweep は `GET /api/recording/records` で全 record を取得し、DB（record_sync /
// recordings）と突き合わせる。3 段構えの (c)（docs/recording.md §3.3）にあたる
// レベルトリガーの真実で、SSE のヒント（(a)(b)）を取りこぼしても収束させる。
//
// M2-18 で record_sweep ジョブ（internal/worker.RecordSweepWorker）から呼ばれる形に
// 公開メソッドとして切り出した。processRecord は M2-16 で record_sync の
// (site, record_id) 行ロックにより冪等化されているため、SSE 由来の handleEvent と
// このメソッドが同一 record を並行処理しても recordings は重複しない
// （docs/recording.md §3.3「record 処理は並行実行しても壊れない」）。
func (w *Watcher) Sweep(ctx context.Context) error {
	slog.Info("watcher sweep started")

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
			slog.Error("sweep: processing record", "record_id", record.ID, "err", err)
		}
	}
	slog.Info("watcher sweep complete", "records", len(records))
	metrics.SweepLastPass.SetToCurrentTime()
	return nil
}

func (w *Watcher) processRecord(ctx context.Context, record mirakc.Record) error {
	// tag は programId しか運ばない（#53）。「自分が予約した record か」は
	// IsOurs（新旧タグ形式のいずれか）で判定し、reservation の特定は record 自身が
	// 持つ Program.ID（tag のパースを経由しない、常に正確な値）で行う。
	ours := mirakc.IsOurs(record.Tags)

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
	// prevStatus は webhook を「finished への遷移」だけに絞るために取る。
	// record_sweep が同じ finished record を何度も processRecord しても再通知しない。
	var prevStatus string
	var title string

	if existingRecordingID != nil {
		recordingID = existingRecordingID
		existing, getErr := q.GetRecordingByID(ctx, *recordingID)
		if getErr != nil {
			return fmt.Errorf("loading recording %d: %w", *recordingID, getErr)
		}
		prevStatus = existing.Status
		title = existing.Title
		if err := w.updateRecordingStatus(ctx, q, *recordingID, record); err != nil {
			return fmt.Errorf("updating recording status: %w", err)
		}
	} else if ours {
		id, createErr := w.createRecording(ctx, q, record)
		if createErr != nil {
			return fmt.Errorf("creating recording: %w", createErr)
		}
		recordingID = &id
		title = derefStr(record.Program.Name)
	}

	if err := w.upsertRecordSync(ctx, q, record, recordingID); err != nil {
		return fmt.Errorf("upserting record_sync: %w", err)
	}

	if record.Recording.Status == "finished" && recordingID != nil {
		if _, err := w.river.InsertTx(ctx, tx, w.newIngestArgs(w.site, record.ID), nil); err != nil {
			return fmt.Errorf("enqueuing ingest job: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	// DB が永続化した後に通知。失敗しても本処理は成功扱い（M3-11）。
	if recordingID != nil && record.Recording.Status == "finished" && prevStatus != "finished" {
		w.notify(ctx, webhook.Event{
			Type:        webhook.EventRecordingFinished,
			RecordingID: *recordingID,
			Site:        w.site,
			Title:       title,
			Status:      "finished",
		})
	}

	return nil
}

func (w *Watcher) createRecording(ctx context.Context, q *sqlcgen.Queries, record mirakc.Record) (int64, error) {
	var resID *int64
	var ruleID *int64

	res, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{
		Site:      w.site,
		ProgramID: record.Program.ID,
	})
	if err != nil && !errors.Is(err, pgx5.ErrNoRows) {
		return 0, fmt.Errorf("looking up reservation for program %d: %w", record.Program.ID, err)
	}
	if err == nil {
		resID = &res.ID
		ruleID = res.RuleID
	}

	source, err := w.deriveRecordingSource(ctx, q, record.Program.ID, resID != nil)
	if err != nil {
		return 0, err
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

// deriveRecordingSource は recordings.source を決める（issue #26）。
//
// reservations.source は「ユーザーが手動で予約したか」（不可逆な歴史的事実）と
// 「いまルールが base を供給しているか」（毎パス変わる導出状態）という 2 つの
// 独立した事実を 1 列に載せていたため、手動予約にルールが一度でもマッチすると
// 二度と 'manual' に戻らない不可逆な歪みがあった（同列は 00012 で削除済み）。
//
// 録画時点の program_intents に action='record' の行があるかどうかだけを見る。
// intent は放送終了まで生きているので録画時点では必ず参照でき、この行の有無が
// 「ユーザーが録れと言ったか」の唯一の真実である。program_overrides（priority 等の
// 上書き）は M2-4 で intent と分離されているため、「ルール由来の予約に上書きを
// 足しただけ」では intent 行が存在せず、正しく 'rule' のままになる
// （docs/recording.md §4.4「manual 行にルールがマッチしても昇格は要らない」）。
//
// hasReservation は予約行が引けたかどうか。**意図が無いときの既定値**を分ける
// ために必要になる。予約行が無い record（tag は付いているが予約が既に削除
// されている等）を 'rule' と記録するのは誤りで、`source = 'rule'` かつ
// `rule_id IS NULL` という矛盾した組になってしまう。帰属できるルールが無いなら
// 「人間が手で起こした録画」として 'manual' に倒す（issue #26 以前の実装が
// `source := "manual"` を既定にしていたのと同じ判断）。
func (w *Watcher) deriveRecordingSource(ctx context.Context, q *sqlcgen.Queries, programID int64, hasReservation bool) (string, error) {
	intent, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: w.site, ProgramID: programID})
	switch {
	case err == nil && intent.Action == db.IntentRecord:
		// ユーザーが「録れ」と言った。ルールもマッチしていても変わらない。
		return db.SourceManual, nil
	case err != nil && !errors.Is(err, pgx5.ErrNoRows):
		return "", fmt.Errorf("looking up program intent for program %d: %w", programID, err)
	}
	if !hasReservation {
		return db.SourceManual, nil
	}
	return db.SourceRule, nil
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

	// handleRecordingFailed は予約が無ければ上で早期 return しているので、
	// ここに到達した時点で予約は必ず存在する。
	source, err := w.deriveRecordingSource(ctx, q, data.ProgramID, true)
	if err != nil {
		return err
	}

	title := derefStr(schedule.Program.Name)
	networkID := int32(schedule.Program.NetworkID)
	serviceID := int32(schedule.Program.ServiceID)
	eventID := int32(schedule.Program.EventID)

	if err := q.CreateFailedRecording(ctx, sqlcgen.CreateFailedRecordingParams{
		ReservationID:     &res.ID,
		RuleID:            res.RuleID,
		Source:            source,
		Site:              w.site,
		NetworkID:         networkID,
		ServiceID:         serviceID,
		EventID:           eventID,
		ServiceName:       service.Name,
		ChannelType:       service.Channel.Type,
		Channel:           service.Channel.Channel,
		Title:             title,
		Description:       schedule.Program.Description,
		Extended:          marshalJSONOrNull(schedule.Program.Extended),
		Genres:            marshalJSONOrNull(schedule.Program.Genres),
		IsFree:            schedule.Program.IsFree,
		ProgramStartAt:    millisToTime(schedule.Program.StartAt),
		ProgramDurationMs: derefInt64(schedule.Program.Duration),
		QualityEvents:     qeJSON,
	}); err != nil {
		return err
	}

	// CreateFailedRecording は :exec（ON CONFLICT 更新もあり）なので id は別途引く。
	// 引けなくても本処理は成功済み。webhook だけ諦める。
	var recordingID int64
	if err := w.pool.QueryRow(ctx, `
		SELECT id FROM recordings
		WHERE site = $1 AND network_id = $2 AND service_id = $3 AND event_id = $4
		  AND deleted_at IS NULL
	`, w.site, networkID, serviceID, eventID).Scan(&recordingID); err != nil {
		slog.Warn("webhook: looking up failed recording id",
			"program_id", data.ProgramID, "err", err)
		return nil
	}
	w.notify(ctx, webhook.Event{
		Type:        webhook.EventRecordingFailed,
		RecordingID: recordingID,
		Site:        w.site,
		Title:       title,
		Status:      "failed",
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
