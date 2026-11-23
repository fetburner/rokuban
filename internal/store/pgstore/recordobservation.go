package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// afterRecordAcquired は、ProcessRecord が (site, record_id) の record 同期行を確保した直後に呼ぶ。
// 本番では nil。直列化の契約テストが、確保の直後に同期点を差し込むために使う。
var afterRecordAcquired func()

// HasReservation は放送イベントの予約があるかを返す。
func (s *Recordings) HasReservation(ctx context.Context, site string, programID int64) (bool, error) {
	_, err := sqlcgen.New(s.pool).GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{
		Site: site, ProgramID: programID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("looking up reservation for program %d: %w", programID, err)
	}
	return true, nil
}

// ProcessRecord は record 同期行、録画行、品質イベントを原子的に反映する。
// 同じ (site, record_id) の観測は直列化される。
func (s *Recordings) ProcessRecord(ctx context.Context, observation store.RecordObservation) (store.ProcessResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.ProcessResult{}, fmt.Errorf("beginning record observation: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	// record_sync の (site, record_id) 行を先に確保して行ロックを取る。
	// これにより同一 record を並行処理しても、後発は先発の commit まで待たされ、
	// 待った後は recording_id が埋まった状態を見るので recordings の二重作成が起きない。
	// AcquireRecordSync は行がなければ recording_id = NULL で新規作成するだけで、
	// 既存行があってもその内容を書き換えない。
	existingRecordingID, err := q.AcquireRecordSync(ctx, sqlcgen.AcquireRecordSyncParams{
		Site: observation.Site, RecordID: observation.RecordID,
		ProgramID: observation.ProgramID, Status: observation.Status,
	})
	if err != nil {
		return store.ProcessResult{}, fmt.Errorf("acquiring record_sync: %w", mapDatabaseError(err))
	}
	if afterRecordAcquired != nil {
		afterRecordAcquired()
	}

	var result store.ProcessResult
	var recordingID *int64
	if existingRecordingID != nil {
		recordingID = existingRecordingID
		existing, err := q.GetRecordingByID(ctx, *recordingID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.ProcessResult{}, fmt.Errorf("recording %d: %w", *recordingID, store.ErrNotFound)
			}
			return store.ProcessResult{}, fmt.Errorf("loading recording %d: %w", *recordingID, err)
		}
		// 既存の録画の状態は webhook を finished への遷移だけに絞るために返す。
		result.PreviousStatus = existing.Status
		result.Title = existing.Title
		if err := updateRecordingStatus(ctx, q, *recordingID, observation); err != nil {
			return store.ProcessResult{}, fmt.Errorf("updating recording status: %w", mapDatabaseError(err))
		}
	} else if observation.Managed {
		var id int64
		if observation.Status == store.RecordingStatusFailed {
			// failed record は recordless failed（SSE / schedules sweep）が先に作った同一
			// active-event 行と競合し得る。既存行を再利用する専用クエリを使い、
			// watcher の復帰順序に依存しないようにする。
			id, err = s.createOrGetFailedRecording(ctx, q, observation)
			if errors.Is(err, pgx.ErrNoRows) {
				// 同一 active-event に生きている non-failed 行（本物の success record が
				// 既に枠を得た状態）がある。この failed record をその行へ帰属させず、
				// recordings は作らない（recordings_unique_active_event の枠は success が
				// 占有する）。record_sync には recording_id = NULL の観測として残すだけで、
				// ingest も起きない。
				result.FailedRecordingSkipped = true
				err = nil
			}
		} else {
			id, err = s.createRecording(ctx, q, observation)
		}
		if err != nil {
			return store.ProcessResult{}, fmt.Errorf("creating recording: %w", mapDatabaseError(err))
		}
		if !result.FailedRecordingSkipped {
			recordingID = &id
			result.Title = observation.Snapshot.Title
			if observation.Status == store.RecordingStatusFailed {
				// CreateOrGetFailedRecording は既存の active-event 行を再利用することがある。
				// その場合も records API の status / startTime を反映し、
				// recording 中の行を failed のまま残さない。
				if err := updateRecordingStatus(ctx, q, id, observation); err != nil {
					return store.ProcessResult{}, fmt.Errorf("updating failed recording status: %w", mapDatabaseError(err))
				}
			}
		}
	}

	// records API に残った failed record は recording.failed の SSE を失った後でも
	// 失敗理由を持つ。録画ごとの報告済み事実で重複を判定するので、同じ record を
	// sweep するたびに quality_events やメトリクスが増殖しない。
	if recordingID != nil && len(observation.FailedReason) > 0 {
		added, err := appendQualityEventIfMissing(ctx, q, *recordingID, "recording.failed", observation.FailedReason)
		if err != nil {
			return store.ProcessResult{}, fmt.Errorf("appending failed quality event: %w", mapDatabaseError(err))
		}
		result.FailedQualityEventAdded = added
	}

	if err := upsertRecordSync(ctx, q, observation, recordingID); err != nil {
		return store.ProcessResult{}, fmt.Errorf("upserting record_sync: %w", mapDatabaseError(err))
	}
	if recordingID != nil && (observation.Status == store.RecordingStatusRecording || observation.Status == store.RecordingStatusFinished) {
		result.EnqueueIngest = true
		if observation.Status == store.RecordingStatusFinished {
			result.IngestPriority = store.IngestPriorityAfterFinish
		}
	}
	result.RecordingID = recordingID

	if err := tx.Commit(ctx); err != nil {
		return store.ProcessResult{}, fmt.Errorf("committing record observation: %w", mapDatabaseError(err))
	}
	return result, nil
}

// ProcessFailedSchedule は schedules API に残っている recordless failed を録画に再構成する。
// SSE の recording.failed と同じ active-event に収束させ、周期 sweep では同じ event + reason を
// 一度だけ quality_events に追記する。
func (s *Recordings) ProcessFailedSchedule(ctx context.Context, observation store.FailedScheduleObservation) (store.ProcessResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.ProcessResult{}, fmt.Errorf("beginning failed schedule transaction: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	res, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{
		Site: observation.Site, ProgramID: observation.ProgramID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ProcessResult{}, nil
	}
	if err != nil {
		return store.ProcessResult{}, fmt.Errorf("looking up reservation: %w", err)
	}
	source, err := deriveRecordingSource(ctx, q, observation.Site, observation.ProgramID, true)
	if err != nil {
		return store.ProcessResult{}, err
	}
	// 既存の active-event 行があれば再利用する。ErrNoRows は、同一 active-event に
	// 生きている non-failed 行（本物の record が既に枠を得た状態）があることを表す。
	// 成功した行へ recording.failed を追記しない。

	recordingID, err := q.CreateOrGetFailedRecording(ctx, withFailedSnapshot(observation.Snapshot, sqlcgen.CreateOrGetFailedRecordingParams{
		RuleID: res.RuleID, Source: source, Site: observation.Site,
		NetworkID: observation.Snapshot.NetworkID, ServiceID: observation.Snapshot.ServiceID,
		EventID: observation.Snapshot.EventID,
	}))
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return store.ProcessResult{}, fmt.Errorf("committing skipped failed schedule: %w", mapDatabaseError(err))
		}
		return store.ProcessResult{FailedRecordingSkipped: true}, nil
	}
	if err != nil {
		return store.ProcessResult{}, fmt.Errorf("creating failed recording from schedule: %w", mapDatabaseError(err))
	}
	added, err := appendQualityEventIfMissing(ctx, q, recordingID, "recording.failed", observation.Reason)
	if err != nil {
		return store.ProcessResult{}, fmt.Errorf("appending failed schedule quality event: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return store.ProcessResult{}, fmt.Errorf("committing failed schedule transaction: %w", mapDatabaseError(err))
	}
	if !added {
		return store.ProcessResult{}, nil
	}
	return store.ProcessResult{
		RecordingID:             &recordingID,
		Title:                   observation.Snapshot.Title,
		FailedQualityEventAdded: added,
	}, nil
}

// RecordFailedRecording は recording.failed の観測を記録し、録画 ID を返す。
// 予約が無いときは何も記録せず recorded=false を返す。
func (s *Recordings) RecordFailedRecording(ctx context.Context, observation store.FailedRecordingObservation) (recordingID int64, recorded bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("beginning failed recording transaction: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	res, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{
		Site: observation.Site, ProgramID: observation.ProgramID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("looking up reservation: %w", err)
	}
	source, err := deriveRecordingSource(ctx, q, observation.Site, observation.ProgramID, true)
	if err != nil {
		return 0, false, err
	}
	events, err := qualityEvents("recording.failed", observation.Reason)
	if err != nil {
		return 0, false, fmt.Errorf("marshalling failed recording quality event: %w", err)
	}
	recordingID, err = q.CreateFailedRecording(ctx, withFailedEventSnapshot(observation.Snapshot, sqlcgen.CreateFailedRecordingParams{
		RuleID: res.RuleID, Source: source, Site: observation.Site,
		NetworkID: observation.Snapshot.NetworkID, ServiceID: observation.Snapshot.ServiceID,
		EventID: observation.Snapshot.EventID, QualityEvents: events,
	}))
	if err != nil {
		return 0, false, fmt.Errorf("creating failed recording: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("committing failed recording transaction: %w", mapDatabaseError(err))
	}
	return recordingID, true, nil
}

// AppendRecordBrokenEvent は record に紐づく録画があれば破損イベントを追記する。
func (s *Recordings) AppendRecordBrokenEvent(ctx context.Context, site, recordID, reason string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning record-broken transaction: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	linkedRecordingID, err := q.GetRecordSyncRecordingID(ctx, sqlcgen.GetRecordSyncRecordingIDParams{Site: site, RecordID: recordID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && linkedRecordingID == nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("looking up record_sync: %w", err)
	}
	reasonJSON, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return false, fmt.Errorf("marshalling broken reason: %w", err)
	}
	events, err := qualityEvents("recording.record-broken", reasonJSON)
	if err != nil {
		return false, fmt.Errorf("marshalling record-broken quality event: %w", err)
	}
	if err := q.AppendQualityEvents(ctx, sqlcgen.AppendQualityEventsParams{ID: *linkedRecordingID, Events: events}); err != nil {
		return false, fmt.Errorf("appending record-broken quality event: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing record-broken transaction: %w", mapDatabaseError(err))
	}
	return true, nil
}

// DeleteStaleRecordSyncs は全量観測に無い record 同期行を削除する。
func (s *Recordings) DeleteStaleRecordSyncs(ctx context.Context, site string, liveRecordIDs []string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning stale record_sync deletion: %w", err)
	}
	defer rollback(tx)
	if liveRecordIDs == nil {
		liveRecordIDs = []string{}
	}
	deleted, err := sqlcgen.New(tx).DeleteStaleRecordSyncs(ctx, sqlcgen.DeleteStaleRecordSyncsParams{
		Site: site, RecordIds: liveRecordIDs,
	})
	if err != nil {
		return 0, fmt.Errorf("deleting stale record_syncs: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing stale record_sync deletion: %w", mapDatabaseError(err))
	}
	return deleted, nil
}

// GetRecordSyncIngestTarget は取り込み先と進捗の分母に使う観測値を返す。
func (s *Recordings) GetRecordSyncIngestTarget(ctx context.Context, site, recordID string) (store.RecordIngestTarget, error) {
	row, err := sqlcgen.New(s.pool).GetRecordSyncIngestTarget(ctx, sqlcgen.GetRecordSyncIngestTargetParams{
		Site: site, RecordID: recordID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.RecordIngestTarget{}, fmt.Errorf("record_sync (%s, %s): %w", site, recordID, store.ErrNotFound)
		}
		return store.RecordIngestTarget{}, fmt.Errorf("querying record_sync ingest target: %w", err)
	}
	return store.RecordIngestTarget{RecordingID: row.RecordingID, ContentLength: row.ContentLength}, nil
}

// GetUningestedRecordBacklog は対象 site の未取り込み record の件数と合計サイズを返す。
func (s *Recordings) GetUningestedRecordBacklog(ctx context.Context, site string) (store.RecordBacklogSnapshot, error) {
	row, err := sqlcgen.New(s.pool).GetUningestedRecordBacklog(ctx, site)
	if err != nil {
		return store.RecordBacklogSnapshot{}, fmt.Errorf("querying record backlog for site %q: %w", site, err)
	}
	return store.RecordBacklogSnapshot{Records: row.Records, Bytes: row.Bytes}, nil
}

// createRecording は成功した record の録画行を作る。
func (s *Recordings) createRecording(ctx context.Context, q *sqlcgen.Queries, observation store.RecordObservation) (int64, error) {
	res, hasReservation, err := getReservation(ctx, q, observation.Site, observation.ProgramID)
	if err != nil {
		return 0, err
	}
	source, err := deriveRecordingSource(ctx, q, observation.Site, observation.ProgramID, hasReservation)
	if err != nil {
		return 0, err
	}
	// 「本物の record が推論に必ず勝つ」（issue #98 の決定、issue #129 症状 2）:
	// 同一 active-event（program_start_at を含む）に status='failed' の行が
	// 生きたまま残っていれば、続く CreateRecording の INSERT より前に superseded にして
	// 枠を明け渡させる。SupersedeFailedRecording / CreateRecording の doc コメント参照
	// （1 つのクエリに詰め込まず 2 つの文に分けている理由も同所）。
	if _, err := q.SupersedeFailedRecording(ctx, sqlcgen.SupersedeFailedRecordingParams{
		Site: observation.Site, NetworkID: observation.Snapshot.NetworkID,
		ServiceID: observation.Snapshot.ServiceID, EventID: observation.Snapshot.EventID,
		ProgramStartAt: observation.Snapshot.ProgramStartAt,
	}); err != nil {
		return 0, fmt.Errorf("superseding failed recording for program %d: %w", observation.ProgramID, err)
	}
	return q.CreateRecording(ctx, recordingParams(observation, res.RuleID, source))
}

// createOrGetFailedRecording は records API に残った failed record のための録画行を作る。
// 同じ active-event の行が recordless failed や SSE 経路で先に作られていても、その行を返す。
// 成功 record 用の createRecording と違い、failed 行を supersede しない
// （本物の成功 record が来たときだけ supersede する）。
func (s *Recordings) createOrGetFailedRecording(ctx context.Context, q *sqlcgen.Queries, observation store.RecordObservation) (int64, error) {
	res, hasReservation, err := getReservation(ctx, q, observation.Site, observation.ProgramID)
	if err != nil {
		return 0, err
	}
	source, err := deriveRecordingSource(ctx, q, observation.Site, observation.ProgramID, hasReservation)
	if err != nil {
		return 0, err
	}
	return q.CreateOrGetFailedRecording(ctx, withFailedSnapshot(observation.Snapshot, sqlcgen.CreateOrGetFailedRecordingParams{
		RuleID: res.RuleID, Source: source, Site: observation.Site,
		NetworkID: observation.Snapshot.NetworkID, ServiceID: observation.Snapshot.ServiceID,
		EventID: observation.Snapshot.EventID,
	}))
}

// getReservation は放送イベントの予約を返す。予約が無いときは hasReservation=false を返す。
func getReservation(ctx context.Context, q *sqlcgen.Queries, site string, programID int64) (sqlcgen.GetReservationBySiteAndProgramIDRow, bool, error) {
	res, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{Site: site, ProgramID: programID})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.GetReservationBySiteAndProgramIDRow{}, false, nil
	}
	if err != nil {
		return sqlcgen.GetReservationBySiteAndProgramIDRow{}, false, fmt.Errorf("looking up reservation for program %d: %w", programID, err)
	}
	return res, true, nil
}

// deriveRecordingSource は同じ tx で読んだ意図と予約の有無から録画の由来を決める。
func deriveRecordingSource(ctx context.Context, q *sqlcgen.Queries, site string, programID int64, hasReservation bool) (string, error) {
	intent, err := q.GetProgramIntent(ctx, sqlcgen.GetProgramIntentParams{Site: site, ProgramID: programID})
	if errors.Is(err, pgx.ErrNoRows) {
		return reservation.DeriveRecordingSource(nil, hasReservation), nil
	}
	if err != nil {
		return "", fmt.Errorf("looking up program intent for program %d: %w", programID, err)
	}
	return reservation.DeriveRecordingSource(&intent.Action, hasReservation), nil
}

func recordingParams(observation store.RecordObservation, ruleID *int64, source string) sqlcgen.CreateRecordingParams {
	return sqlcgen.CreateRecordingParams{
		RuleID: ruleID, Source: source, Site: observation.Site,
		NetworkID: observation.Snapshot.NetworkID, ServiceID: observation.Snapshot.ServiceID,
		EventID: observation.Snapshot.EventID, ServiceName: observation.Snapshot.ServiceName,
		ChannelType: observation.Snapshot.ChannelType, Channel: observation.Snapshot.Channel,
		Title: observation.Snapshot.Title, Description: observation.Snapshot.Description,
		Extended: observation.Snapshot.Extended, Genres: observation.Snapshot.Genres,
		IsFree: observation.Snapshot.IsFree, ProgramStartAt: observation.Snapshot.ProgramStartAt,
		ProgramDurationMs: observation.Snapshot.ProgramDurationMs,
		Status:            store.NormalizeRecordingStatus(observation.RecordID, observation.Status),
		StartedAt:         observation.StartedAt, EndedAt: observation.EndedAt,
	}
}

func withFailedSnapshot(snapshot store.RecordingSnapshot, params sqlcgen.CreateOrGetFailedRecordingParams) sqlcgen.CreateOrGetFailedRecordingParams {
	params.ServiceName = snapshot.ServiceName
	params.ChannelType = snapshot.ChannelType
	params.Channel = snapshot.Channel
	params.Title = snapshot.Title
	params.Description = snapshot.Description
	params.Extended = snapshot.Extended
	params.Genres = snapshot.Genres
	params.IsFree = snapshot.IsFree
	params.ProgramStartAt = snapshot.ProgramStartAt
	params.ProgramDurationMs = snapshot.ProgramDurationMs
	return params
}

func withFailedEventSnapshot(snapshot store.RecordingSnapshot, params sqlcgen.CreateFailedRecordingParams) sqlcgen.CreateFailedRecordingParams {
	params.ServiceName = snapshot.ServiceName
	params.ChannelType = snapshot.ChannelType
	params.Channel = snapshot.Channel
	params.Title = snapshot.Title
	params.Description = snapshot.Description
	params.Extended = snapshot.Extended
	params.Genres = snapshot.Genres
	params.IsFree = snapshot.IsFree
	params.ProgramStartAt = snapshot.ProgramStartAt
	params.ProgramDurationMs = snapshot.ProgramDurationMs
	return params
}

func updateRecordingStatus(ctx context.Context, q *sqlcgen.Queries, recordingID int64, observation store.RecordObservation) error {
	return q.UpdateRecordingStatus(ctx, sqlcgen.UpdateRecordingStatusParams{
		ID:        recordingID,
		NewStatus: store.NormalizeRecordingStatus(observation.RecordID, observation.Status),
		StartedAt: observation.StartedAt, EndedAt: observation.EndedAt,
	})
}

func upsertRecordSync(ctx context.Context, q *sqlcgen.Queries, observation store.RecordObservation, recordingID *int64) error {
	tags := observation.Tags
	if tags == nil {
		tags = []string{}
	}
	return q.UpsertRecordSync(ctx, sqlcgen.UpsertRecordSyncParams{
		Site: observation.Site, RecordID: observation.RecordID, RecordingID: recordingID,
		ProgramID: observation.ProgramID, Status: observation.Status,
		ContentPath: observation.ContentPath, ContentLength: observation.ContentLength,
		Tags: tags, FailedReason: observation.FailedReason,
	})
}

func appendQualityEventIfMissing(ctx context.Context, q *sqlcgen.Queries, recordingID int64, event string, reason json.RawMessage) (bool, error) {
	events, err := qualityEvents(event, reason)
	if err != nil {
		return false, err
	}
	rows, err := q.AppendQualityEventsIfMissing(ctx, sqlcgen.AppendQualityEventsIfMissingParams{ID: recordingID, Events: events})
	return rows > 0, err
}

// qualityEvents は品質イベントを JSON 配列にする。`at` は書かない。
// 品質イベントの書き込みクエリが DB の時計（now()）で補う。
func qualityEvents(event string, reason json.RawMessage) (json.RawMessage, error) {
	return json.Marshal([]struct {
		Event  string          `json:"event"`
		Reason json.RawMessage `json:"reason"`
	}{{Event: event, Reason: reason}})
}

var (
	_ store.RecordObservations = (*Recordings)(nil)
	_ store.IngestTarget       = (*Recordings)(nil)
	_ store.RecordBacklog      = (*Recordings)(nil)
)
