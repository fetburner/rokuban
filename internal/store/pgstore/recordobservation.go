package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

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

	// この no-op upsert が (site, record_id) の行ロックを取り、同じ record の
	// processRecord を直列化する。後発は先発の commit 後の recording_id を受け取る。
	existingRecordingID, err := q.AcquireRecordSync(ctx, sqlcgen.AcquireRecordSyncParams{
		Site: observation.Site, RecordID: observation.RecordID,
		ProgramID: observation.ProgramID, Status: observation.Status,
	})
	if err != nil {
		return store.ProcessResult{}, fmt.Errorf("acquiring record_sync: %w", mapDatabaseError(err))
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
		result.PreviousStatus = existing.Status
		result.Title = existing.Title
		if err := updateRecordingStatus(ctx, q, *recordingID, observation); err != nil {
			return store.ProcessResult{}, fmt.Errorf("updating recording status: %w", mapDatabaseError(err))
		}
	} else if observation.Managed {
		var id int64
		if observation.Status == db.RecordingStatusFailed {
			id, err = s.createOrGetFailedRecording(ctx, q, observation)
			if errors.Is(err, pgx.ErrNoRows) {
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
			if observation.Status == db.RecordingStatusFailed {
				if err := updateRecordingStatus(ctx, q, id, observation); err != nil {
					return store.ProcessResult{}, fmt.Errorf("updating failed recording status: %w", mapDatabaseError(err))
				}
			}
		}
	}

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
	if recordingID != nil && (observation.Status == db.RecordingStatusRecording || observation.Status == db.RecordingStatusFinished) {
		result.EnqueueIngest = true
		if observation.Status == db.RecordingStatusFinished {
			result.IngestPriority = 2
		}
	}
	result.RecordingID = recordingID

	if err := tx.Commit(ctx); err != nil {
		return store.ProcessResult{}, fmt.Errorf("committing record observation: %w", mapDatabaseError(err))
	}
	return result, nil
}

// ProcessFailedSchedule は予約のある失敗 schedule を再構成する。
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
func (s *Recordings) RecordFailedRecording(ctx context.Context, observation store.FailedRecordingObservation) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning failed recording transaction: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	res, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{
		Site: observation.Site, ProgramID: observation.ProgramID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("looking up reservation: %w", err)
	}
	source, err := deriveRecordingSource(ctx, q, observation.Site, observation.ProgramID, true)
	if err != nil {
		return 0, err
	}
	events, err := qualityEvents("recording.failed", observation.Reason)
	if err != nil {
		return 0, fmt.Errorf("marshalling failed recording quality event: %w", err)
	}
	recordingID, err := q.CreateFailedRecording(ctx, withFailedEventSnapshot(observation.Snapshot, sqlcgen.CreateFailedRecordingParams{
		RuleID: res.RuleID, Source: source, Site: observation.Site,
		NetworkID: observation.Snapshot.NetworkID, ServiceID: observation.Snapshot.ServiceID,
		EventID: observation.Snapshot.EventID, QualityEvents: events,
	}))
	if err != nil {
		return 0, fmt.Errorf("creating failed recording: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing failed recording transaction: %w", mapDatabaseError(err))
	}
	return recordingID, nil
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

// MarkSweepComplete は record 全量突き合わせの成功時刻を更新する。
func (s *Recordings) MarkSweepComplete(ctx context.Context, site string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning record sweep snapshot transaction: %w", err)
	}
	defer rollback(tx)
	if err := sqlcgen.New(tx).UpsertRecordSweepSnapshot(ctx, site); err != nil {
		return fmt.Errorf("upserting record sweep snapshot: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing record sweep snapshot: %w", mapDatabaseError(err))
	}
	return nil
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

func (s *Recordings) createRecording(ctx context.Context, q *sqlcgen.Queries, observation store.RecordObservation) (int64, error) {
	res, hasReservation, err := getReservation(ctx, q, observation.Site, observation.ProgramID)
	if err != nil {
		return 0, err
	}
	source, err := deriveRecordingSource(ctx, q, observation.Site, observation.ProgramID, hasReservation)
	if err != nil {
		return 0, err
	}
	if _, err := q.SupersedeFailedRecording(ctx, sqlcgen.SupersedeFailedRecordingParams{
		Site: observation.Site, NetworkID: observation.Snapshot.NetworkID,
		ServiceID: observation.Snapshot.ServiceID, EventID: observation.Snapshot.EventID,
		ProgramStartAt: observation.Snapshot.ProgramStartAt,
	}); err != nil {
		return 0, fmt.Errorf("superseding failed recording for program %d: %w", observation.ProgramID, err)
	}
	return q.CreateRecording(ctx, recordingParams(observation, res.RuleID, source))
}

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
		Status:            normalizeRecordingStatus(observation.RecordID, observation.Status),
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
		NewStatus: normalizeRecordingStatus(observation.RecordID, observation.Status),
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

func qualityEvents(event string, reason json.RawMessage) (json.RawMessage, error) {
	return json.Marshal([]db.QualityEvent{{At: time.Now(), Event: event, Reason: reason}})
}

// normalizeRecordingStatus は mirakc の値を recordings_status_check の値域へ写す。
// 未知値は failed に写し、生の値は record_sync に保持する
// （TestProcessRecord_StatusValues）。
func normalizeRecordingStatus(recordID, raw string) string {
	switch raw {
	case db.RecordingStatusRecording, db.RecordingStatusFinished, db.RecordingStatusCanceled, db.RecordingStatusFailed:
		return raw
	default:
		slog.Error("unknown mirakc recording status; storing as failed", "record_id", recordID, "raw_status", raw)
		return db.RecordingStatusFailed
	}
}

var (
	_ store.RecordObservations = (*Recordings)(nil)
	_ store.IngestTarget       = (*Recordings)(nil)
	_ store.RecordBacklog      = (*Recordings)(nil)
)
