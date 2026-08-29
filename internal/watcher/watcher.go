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
	"github.com/fetburner/rokuban/internal/ptr"
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
	// IsOurs（rokuban のタグ）で判定し、reservation の特定は record 自身が
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
		title = ptr.Deref(record.Program.Name)
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

	source, err := db.DeriveRecordingSource(ctx, q, w.site, record.Program.ID, resID != nil)
	if err != nil {
		return 0, err
	}

	networkID := int32(record.Program.NetworkID)
	serviceID := int32(record.Program.ServiceID)
	eventID := int32(record.Program.EventID)

	// 「本物の record が推論に必ず勝つ」（issue #98 の決定、issue #129 症状 2）:
	// 同一 active-event に status='failed' の行が生きたまま残っていれば、続く
	// CreateRecording の INSERT より前に superseded にして枠を明け渡させる。
	// SupersedeFailedRecording / CreateRecording の doc コメント参照（1 つの
	// クエリに詰め込まず 2 つの文に分けている理由も同所）。
	if _, err := q.SupersedeFailedRecording(ctx, sqlcgen.SupersedeFailedRecordingParams{
		Site:      w.site,
		NetworkID: networkID,
		ServiceID: serviceID,
		EventID:   eventID,
	}); err != nil {
		return 0, fmt.Errorf("superseding failed recording for program %d: %w", record.Program.ID, err)
	}

	id, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		RuleID:            ruleID,
		Source:            source,
		Site:              w.site,
		NetworkID:         networkID,
		ServiceID:         serviceID,
		EventID:           eventID,
		ServiceName:       record.Service.Name,
		ChannelType:       record.Service.Channel.Type,
		Channel:           record.Service.Channel.Channel,
		Title:             ptr.Deref(record.Program.Name),
		Description:       record.Program.Description,
		Extended:          marshalJSONOrNull(record.Program.Extended),
		Genres:            marshalJSONOrNull(record.Program.Genres),
		IsFree:            record.Program.IsFree,
		ProgramStartAt:    millisToTime(record.Program.StartAt),
		ProgramDurationMs: ptr.Deref(record.Program.Duration),
		Status:            normalizeRecordingStatus(record.ID, record.Recording.Status),
		StartedAt:         millisToTimeNonNil(record.Recording.StartTime),
		EndedAt:           millisToTimePtr(record.Recording.EndTime),
	})
	return id, err
}

// knownRecordingStatuses は CHECK 制約 recordings_status_check が許す値集合と
// 一致する。db パッケージの
// 定数を直接 switch に書かず集合として持つのは normalizeRecordingStatus の
// メンテナ向けに「ここが CHECK と同期している唯一の場所」を明示するため。
var knownRecordingStatuses = map[string]bool{
	db.RecordingStatusRecording: true,
	db.RecordingStatusFinished:  true,
	db.RecordingStatusCanceled:  true,
	db.RecordingStatusFailed:    true,
}

// normalizeRecordingStatus は mirakc の RecordInfo.Status（internal/mirakc/types.go。
// 値域を制限する型を持たない素の string）を recordings.status の閉じた値域に
// 正規化する（issue #130）。
//
// mirakc の RecordingStatus は 4 バリアントの網羅的 enum で、ワイヤに出る値は
// recording/finished/canceled/failed の 4 つで閉じていることをソースで確認済み
// （issue #130）。だが RecordInfo.Status 自体は素の string なので、mirakc が
// 将来 5 つ目の値を追加すると Rokuban はコンパイル時に気付けない。未知の値を
// そのまま CreateRecording / UpdateRecordingStatus に渡すと recordings_status_check
// 違反で processRecord のトランザクション全体がロールバックし、record_sync にも
// 観測が残らないまま同じ record を毎パス再試行し続ける
// （実際に canceled だけがこの壊れ方をしていたのが #130 本体）。
//
// 未知の値は 'failed' に丸める。'canceled' を 'failed' に丸めると「録画一覧が
// 嘘をつく」という #130 が避けた問題と混同しないこと —— あちらは**分かっている
// 2つの異なる事実**（取消と失敗）を同じ値に潰すから嘘になる。ここは逆に
// **何が起きたか分からない**という状態そのものが事実であり、その生の値は
// record_sync.status（CHECK 無し。docs/schema/record-sync.md「mirakc の
// recordingStatus そのまま」）に手を加えず残る。recordings.status 側は「正常に
// 完了しなかった」という粗い事実だけを表現する。5 値目を CHECK / openapi.yaml の
// enum に足さない選択（issue #130 の「決めること」）に伴う代償として、この粗さは
// 許容する。
//
// 未知の値を観測したら必ず slog.Error でログに残す。次に mirakc が値を足したら、
// このログを見た人間が本関数と recordings_status_check / openapi.yaml の enum を
// 更新すること。
func normalizeRecordingStatus(recordID, raw string) string {
	if knownRecordingStatuses[raw] {
		return raw
	}
	slog.Error("unknown mirakc recording status; storing as failed",
		"record_id", recordID, "raw_status", raw)
	return db.RecordingStatusFailed
}

func (w *Watcher) updateRecordingStatus(ctx context.Context, q *sqlcgen.Queries, recordingID int64, record mirakc.Record) error {
	return q.UpdateRecordingStatus(ctx, sqlcgen.UpdateRecordingStatusParams{
		ID:        recordingID,
		NewStatus: normalizeRecordingStatus(record.ID, record.Recording.Status),
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
	source, err := db.DeriveRecordingSource(ctx, q, w.site, data.ProgramID, true)
	if err != nil {
		return err
	}

	title := ptr.Deref(schedule.Program.Name)
	networkID := int32(schedule.Program.NetworkID)
	serviceID := int32(schedule.Program.ServiceID)
	eventID := int32(schedule.Program.EventID)

	if err := q.CreateFailedRecording(ctx, sqlcgen.CreateFailedRecordingParams{
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
		ProgramDurationMs: ptr.Deref(schedule.Program.Duration),
		QualityEvents:     qeJSON,
	}); err != nil {
		return err
	}

	// CreateFailedRecording は :exec（ON CONFLICT 更新もあり）なので id は別途引く。
	// 引けなくても本処理は成功済み。webhook だけ諦める。
	//
	// superseded_at IS NULL も条件に入れる（issue #129 症状 2）。CreateRecording が
	// 同一 active-event の failed 行を superseded にした後も、その行は deleted_at が
	// NULL のまま履歴として残るため、deleted_at だけで絞ると superseded 済みの
	// 過去の failed 行と、いま CreateFailedRecording が更新した「生きている」行の
	// 2 行がヒットしうる（ORDER BY が無いと QueryRow はどちらを返すか不定）。
	// ON CONFLICT の対象（生きている行）と同じ述語に揃えることで一意に定まる。
	var recordingID int64
	if err := w.pool.QueryRow(ctx, `
		SELECT id FROM recordings
		WHERE site = $1 AND network_id = $2 AND service_id = $3 AND event_id = $4
		  AND deleted_at IS NULL AND superseded_at IS NULL
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
