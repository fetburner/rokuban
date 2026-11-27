package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

var (
	_ store.RecordObservations = (*Recordings)(nil)
	_ store.IngestTarget       = (*Recordings)(nil)
	_ store.RecordBacklog      = (*Recordings)(nil)
)

// Recordings は録画の観測・失敗の記録・取り込み先・未取り込みの滞留量・ごみ箱と完全削除を MySQL で実装する。
// 各書き込みは 1 つの tx で行い、録画の行に触れる前に行ロックを取る。
type Recordings struct {
	db *sql.DB
	// afterRecordAcquired は、ProcessRecord が (site, record_id) の record 同期行を確保した直後に呼ぶ。
	// 本番では nil。直列化の契約テストが、確保の直後に同期点を差し込むために使う。
	afterRecordAcquired func()
}

// NewRecordings は MySQL を使う Recordings を作る。
func NewRecordings(db *sql.DB) *Recordings {
	return &Recordings{db: db}
}

// HasReservation は放送イベントの予約が現在存在するかを返す。
func (s *Recordings) HasReservation(ctx context.Context, site string, programID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM reservations WHERE site = ? AND program_id = ?`, site, programID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("looking up reservation for program %d: %w", programID, err)
	}
	return true, nil
}

// ProcessRecord は record 同期行、録画行、品質イベントを原子的に反映する。
// 同じ (site, record_id) の観測は、record 同期行の確保で直列化される。
func (s *Recordings) ProcessRecord(ctx context.Context, observation store.RecordObservation) (store.ProcessResult, error) {
	var result store.ProcessResult
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		// record 同期行を先に確保する。ON DUPLICATE KEY UPDATE は既存行に行ロックを取るので、
		// 後発は先発の commit まで待たされ、待った後は recording_id が埋まった状態を見る。
		if err := acquireRecordSync(ctx, tx, observation, now); err != nil {
			return fmt.Errorf("acquiring record_sync: %w", err)
		}
		existing, err := recordSyncRecordingID(ctx, tx, observation.Site, observation.RecordID)
		if err != nil {
			return err
		}
		if s.afterRecordAcquired != nil {
			s.afterRecordAcquired()
		}
		recordingID, attributed, err := attributeRecord(ctx, tx, now, observation, existing)
		if err != nil {
			return err
		}
		result = attributed

		// records API に残った failed record は、録画ごとの報告済みの事実で重複を判定する。
		if recordingID != nil && len(observation.FailedReason) > 0 {
			added, err := appendQualityEventIfMissing(ctx, tx, now, *recordingID, "recording.failed", observation.FailedReason)
			if err != nil {
				return fmt.Errorf("appending failed quality event: %w", err)
			}
			result.FailedQualityEventAdded = added
		}

		if err := upsertRecordSync(ctx, tx, now, observation, recordingID); err != nil {
			return fmt.Errorf("upserting record_sync: %w", err)
		}
		if recordingID != nil && (observation.Status == store.RecordingStatusRecording || observation.Status == store.RecordingStatusFinished) {
			result.EnqueueIngest = true
			if observation.Status == store.RecordingStatusFinished {
				result.IngestPriority = store.IngestPriorityAfterFinish
			}
		}
		result.RecordingID = recordingID
		return nil
	})
	if err != nil {
		return store.ProcessResult{}, err
	}
	return result, nil
}

// attributeRecord は観測を録画に帰属させ、録画の ID と、録画の作成・既存の状態に関する結果を返す。
// existing は record 同期行に既にある録画である。帰属させないときは録画の ID は nil になる。
func attributeRecord(ctx context.Context, tx *sql.Tx, now time.Time, observation store.RecordObservation, existing *int64) (*int64, store.ProcessResult, error) {
	var result store.ProcessResult
	failed := observation.Status == store.RecordingStatusFailed
	switch {
	case existing != nil:
		// 既存の録画の状態は webhook を finished への遷移だけに絞るために返す。
		if err := lockRecordingState(ctx, tx, *existing); err != nil {
			return nil, result, err
		}
		var status, title string
		if err := tx.QueryRowContext(ctx, `SELECT status, title FROM recordings WHERE id = ?`, *existing).Scan(&status, &title); err != nil {
			return nil, result, fmt.Errorf("loading recording %d: %w", *existing, err)
		}
		result.PreviousStatus = status
		result.Title = title
		if err := updateRecordingStatus(ctx, tx, now, *existing, observation); err != nil {
			return nil, result, fmt.Errorf("updating recording status: %w", err)
		}
		return existing, result, nil
	case !observation.Managed:
		return nil, result, nil
	case failed:
		// records API の failed は、recordless failed の SSE か sweep が先に作った同じ active-event の
		// 行を再利用する。生きている成功の行があれば帰属させず、録画も作らない。
		id, reusable, err := createOrGetFailedRecording(ctx, tx, now, failedDraft(observation.Site, observation.ProgramID, observation.Snapshot, "[]", false))
		if err != nil {
			return nil, result, fmt.Errorf("creating recording: %w", err)
		}
		if !reusable {
			result.FailedRecordingSkipped = true
			return nil, result, nil
		}
		if err := updateRecordingStatus(ctx, tx, now, id, observation); err != nil {
			return nil, result, fmt.Errorf("updating failed recording status: %w", err)
		}
		result.Title = observation.Snapshot.Title
		return &id, result, nil
	default:
		id, err := createRecording(ctx, tx, now, recordDraftFromObservation(observation))
		if err != nil {
			return nil, result, fmt.Errorf("creating recording: %w", err)
		}
		result.Title = observation.Snapshot.Title
		return &id, result, nil
	}
}

// ProcessFailedSchedule は schedules API に残っている recordless failed を録画に再構成する。
// SSE の recording.failed と同じ active-event に収束させ、周期 sweep では同じ event + reason を
// 一度だけ quality_events に追記する。
func (s *Recordings) ProcessFailedSchedule(ctx context.Context, observation store.FailedScheduleObservation) (store.ProcessResult, error) {
	var result store.ProcessResult
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		_, hasReservation, err := getReservation(ctx, tx, observation.Site, observation.ProgramID)
		if err != nil || !hasReservation {
			return err
		}
		id, reusable, err := createOrGetFailedRecording(ctx, tx, now, failedDraft(observation.Site, observation.ProgramID, observation.Snapshot, "[]", false))
		if err != nil {
			return fmt.Errorf("creating failed recording from schedule: %w", err)
		}
		if !reusable {
			result = store.ProcessResult{FailedRecordingSkipped: true}
			return nil
		}
		added, err := appendQualityEventIfMissing(ctx, tx, now, id, "recording.failed", observation.Reason)
		if err != nil {
			return fmt.Errorf("appending failed schedule quality event: %w", err)
		}
		if added {
			result = store.ProcessResult{
				RecordingID:             &id,
				Title:                   observation.Snapshot.Title,
				FailedQualityEventAdded: true,
			}
		}
		return nil
	})
	if err != nil {
		return store.ProcessResult{}, err
	}
	return result, nil
}

// RecordFailedRecording は recording.failed の観測を記録し、録画 ID を返す。
// 予約が無いときは何も記録せず recorded=false を返す。
func (s *Recordings) RecordFailedRecording(ctx context.Context, observation store.FailedRecordingObservation) (int64, bool, error) {
	var recordingID int64
	recorded := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		_, hasReservation, err := getReservation(ctx, tx, observation.Site, observation.ProgramID)
		if err != nil || !hasReservation {
			return err
		}
		events, err := qualityEventsJSON(now, "recording.failed", observation.Reason)
		if err != nil {
			return fmt.Errorf("marshalling failed recording quality event: %w", err)
		}
		recordingID, err = createFailedRecording(ctx, tx, now, failedDraft(observation.Site, observation.ProgramID, observation.Snapshot, events, true))
		if err != nil {
			return fmt.Errorf("creating failed recording: %w", err)
		}
		recorded = true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return recordingID, recorded, nil
}

// AppendRecordBrokenEvent は record に紐づく録画があれば破損イベントを追記する。
func (s *Recordings) AppendRecordBrokenEvent(ctx context.Context, site, recordID, reason string) (bool, error) {
	appended := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var linked sql.Null[int64]
		err := tx.QueryRowContext(ctx, `SELECT recording_id FROM record_sync WHERE site = ? AND record_id = ?`, site, recordID).Scan(&linked)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !linked.Valid) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("looking up record_sync: %w", err)
		}
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		reasonJSON, err := json.Marshal(map[string]string{"reason": reason})
		if err != nil {
			return fmt.Errorf("marshalling broken reason: %w", err)
		}
		events, err := qualityEventsJSON(now, "recording.record-broken", reasonJSON)
		if err != nil {
			return fmt.Errorf("marshalling record-broken quality event: %w", err)
		}
		if err := lockRecordingState(ctx, tx, linked.V); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE recordings SET quality_events = JSON_MERGE_PRESERVE(quality_events, ?), updated_at = ?
WHERE id = ?`, events, now, linked.V); err != nil {
			return fmt.Errorf("appending record-broken quality event: %w", err)
		}
		appended = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return appended, nil
}

// DeleteStaleRecordSyncs は全量観測に無い record 同期行を削除する。
// liveRecordIDs が空なら、その site の行を全て消す。
func (s *Recordings) DeleteStaleRecordSyncs(ctx context.Context, site string, liveRecordIDs []string) (int64, error) {
	var deleted int64
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		query := `DELETE FROM record_sync WHERE site = ?`
		args := []any{site}
		if len(liveRecordIDs) > 0 {
			query += ` AND record_id NOT IN (` + placeholders(len(liveRecordIDs)) + `)`
			for _, id := range liveRecordIDs {
				args = append(args, id)
			}
		}
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("deleting stale record_syncs: %w", err)
		}
		deleted, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// GetRecordSyncIngestTarget は取り込み先と進捗の分母に使う観測値を返す。
func (s *Recordings) GetRecordSyncIngestTarget(ctx context.Context, site, recordID string) (store.RecordIngestTarget, error) {
	var recordingID, contentLength sql.Null[int64]
	err := s.db.QueryRowContext(ctx, `SELECT recording_id, content_length FROM record_sync WHERE site = ? AND record_id = ?`, site, recordID).Scan(&recordingID, &contentLength)
	if errors.Is(err, sql.ErrNoRows) {
		return store.RecordIngestTarget{}, fmt.Errorf("record_sync (%s, %s): %w", site, recordID, store.ErrNotFound)
	}
	if err != nil {
		return store.RecordIngestTarget{}, fmt.Errorf("querying record_sync ingest target: %w", err)
	}
	return store.RecordIngestTarget{RecordingID: nullPtr(recordingID), ContentLength: nullPtr(contentLength)}, nil
}

// GetUningestedRecordBacklog は対象 site の未取り込み record の件数と合計サイズを返す。
// 原本の asset 行が状態を問わず 1 つも無い終了済みの record を数える。
func (s *Recordings) GetUningestedRecordBacklog(ctx context.Context, site string) (store.RecordBacklogSnapshot, error) {
	var snapshot store.RecordBacklogSnapshot
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), CAST(COALESCE(SUM(rs.content_length), 0) AS SIGNED)
FROM record_sync rs
WHERE rs.site = ?
  AND rs.status = 'finished'
  AND rs.recording_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM media_assets a
      WHERE a.recording_id = rs.recording_id AND a.kind = 'original'
  )`, site).Scan(&snapshot.Records, &snapshot.Bytes)
	if err != nil {
		return store.RecordBacklogSnapshot{}, fmt.Errorf("querying record backlog for site %q: %w", site, err)
	}
	return snapshot, nil
}

// recordingDraft は録画の行に書く値である。由来（ルールと予約）は書き込みの直前に同じ tx で導く。
type recordingDraft struct {
	site      string
	programID int64
	snapshot  store.RecordingSnapshot
	status    string
	startedAt *time.Time
	endedAt   *time.Time
	// events は quality_events の JSON 配列である。時刻は呼び出し側が tx の時計で付ける。
	events string
	// reported は failure_reported の値である。
	reported bool
}

// recordDraftFromObservation は mirakc の record の観測から録画の行の値を作る。status は丸めて書く。
func recordDraftFromObservation(observation store.RecordObservation) recordingDraft {
	return recordingDraft{
		site: observation.Site, programID: observation.ProgramID, snapshot: observation.Snapshot,
		status:    store.NormalizeRecordingStatus(observation.RecordID, observation.Status),
		startedAt: observation.StartedAt, endedAt: observation.EndedAt, events: "[]",
	}
}

// failedDraft は失敗の録画の行の値を作る。失敗の録画は開始・終了の時刻を持たない。
func failedDraft(site string, programID int64, snapshot store.RecordingSnapshot, events string, reported bool) recordingDraft {
	return recordingDraft{
		site: site, programID: programID, snapshot: snapshot,
		status: store.RecordingStatusFailed, events: events, reported: reported,
	}
}

// insertRecordingSQL は録画の行を作る文である。ON DUPLICATE KEY UPDATE の句は呼び出し側が末尾に足す。
// 列の順序は recordingDraft.args と一致させる（25 列）。
var insertRecordingSQL = `INSERT INTO recordings (
    rule_id, source, site, network_id, service_id, event_id, service_name,
    channel_type, channel, title, description, extended, genres, is_free,
    program_start_at, program_duration_ms, status, started_at, ended_at,
    series_key, genre_lv1, quality_events, failure_reported, created_at, updated_at
) VALUES (` + placeholders(25) + `)`

// args は insertRecordingSQL の値を列の順に返す。series_key は題名から、genre_lv1 は genres から、アプリが書く。
func (d recordingDraft) args(now time.Time, ruleID *int64, source string) []any {
	snap := d.snapshot
	return []any{
		ruleID, source, d.site, snap.NetworkID, snap.ServiceID, snap.EventID, snap.ServiceName,
		snap.ChannelType, snap.Channel, snap.Title, snap.Description, nullableJSON(snap.Extended), nullableJSON(snap.Genres), snap.IsFree,
		snap.ProgramStartAt, snap.ProgramDurationMs, d.status, d.startedAt, d.endedAt,
		seriesKeyValue(snap.Title), epgGenreLevels(snap.Genres), d.events, d.reported, now, now,
	}
}

// recordingOrigin は予約と意図から、録画に付けるルールと由来を同じ tx で導く。
func recordingOrigin(ctx context.Context, tx *sql.Tx, site string, programID int64) (*int64, string, error) {
	ruleID, hasReservation, err := getReservation(ctx, tx, site, programID)
	if err != nil {
		return nil, "", err
	}
	source, err := deriveRecordingSource(ctx, tx, site, programID, hasReservation)
	if err != nil {
		return nil, "", err
	}
	return ruleID, source, nil
}

// getReservation は放送イベントの予約のルールを返す。予約が無いときは hasReservation=false を返す。
func getReservation(ctx context.Context, tx *sql.Tx, site string, programID int64) (*int64, bool, error) {
	var ruleID sql.Null[int64]
	err := tx.QueryRowContext(ctx, `SELECT rule_id FROM reservations WHERE site = ? AND program_id = ?`, site, programID).Scan(&ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("looking up reservation for program %d: %w", programID, err)
	}
	return nullPtr(ruleID), true, nil
}

// deriveRecordingSource は同じ tx で読んだ意図と予約の有無から録画の由来を決める。
func deriveRecordingSource(ctx context.Context, tx *sql.Tx, site string, programID int64, hasReservation bool) (string, error) {
	var action string
	err := tx.QueryRowContext(ctx, `SELECT action FROM program_intents WHERE site = ? AND program_id = ?`, site, programID).Scan(&action)
	if errors.Is(err, sql.ErrNoRows) {
		return reservation.DeriveRecordingSource(nil, hasReservation), nil
	}
	if err != nil {
		return "", fmt.Errorf("looking up program intent for program %d: %w", programID, err)
	}
	return reservation.DeriveRecordingSource(&action, hasReservation), nil
}

// createRecording は成功した record の録画行を作り、ID を返す。
func createRecording(ctx context.Context, tx *sql.Tx, now time.Time, d recordingDraft) (int64, error) {
	ruleID, source, err := recordingOrigin(ctx, tx, d.site, d.programID)
	if err != nil {
		return 0, err
	}
	// 「本物の record が推論に必ず勝つ」: 同一 active-event に status='failed' の生きた行が
	// あれば、INSERT より前に superseded にして枠を明け渡させる。Postgres の SupersedeFailedRecording と同じ。
	if _, err := tx.ExecContext(ctx, `
UPDATE recordings SET superseded_at = ?, updated_at = ?
WHERE site = ? AND network_id = ? AND service_id = ? AND event_id = ?
  AND program_start_at = ? AND deleted_at IS NULL AND superseded_at IS NULL AND status = 'failed'`,
		now, now, d.site, d.snapshot.NetworkID, d.snapshot.ServiceID, d.snapshot.EventID, d.snapshot.ProgramStartAt); err != nil {
		return 0, fmt.Errorf("superseding failed recording for program %d: %w", d.programID, err)
	}
	res, err := tx.ExecContext(ctx, insertRecordingSQL, d.args(now, ruleID, source)...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// createOrGetFailedRecording は failed の録画行を作るか、同じ active-event の生きた failed 行を返す。
// 生きた失敗でない行（本物の成功が枠を得たもの）があれば reusable=false を返し、何も変えない。
// ON DUPLICATE KEY UPDATE が狙うのは active-event のキーだけである（recordings の PK は AUTO_INCREMENT で衝突しない）。
func createOrGetFailedRecording(ctx context.Context, tx *sql.Tx, now time.Time, d recordingDraft) (int64, bool, error) {
	ruleID, source, err := recordingOrigin(ctx, tx, d.site, d.programID)
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, insertRecordingSQL+` AS incoming
ON DUPLICATE KEY UPDATE updated_at = recordings.updated_at`, d.args(now, ruleID, source)...); err != nil {
		return 0, false, err
	}
	id, status, err := liveRecordingAt(ctx, tx, d.site, d.snapshot)
	if err != nil {
		return 0, false, err
	}
	return id, status == store.RecordingStatusFailed, nil
}

// createFailedRecording は failed の録画行を作るか、同じ active-event の生きた行へ品質イベントを追記する。
// 生きた行の状態は問わない（Postgres の CreateFailedRecording と同じ）。
func createFailedRecording(ctx context.Context, tx *sql.Tx, now time.Time, d recordingDraft) (int64, error) {
	ruleID, source, err := recordingOrigin(ctx, tx, d.site, d.programID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, insertRecordingSQL+` AS incoming
ON DUPLICATE KEY UPDATE
    quality_events = JSON_MERGE_PRESERVE(recordings.quality_events, incoming.quality_events),
    failure_reported = TRUE,
    updated_at = incoming.updated_at`, d.args(now, ruleID, source)...); err != nil {
		return 0, err
	}
	id, _, err := liveRecordingAt(ctx, tx, d.site, d.snapshot)
	return id, err
}

// liveRecordingAt は同じ放送イベントの生きた録画の ID と状態を、書いた後に同じ tx で読み直す。
func liveRecordingAt(ctx context.Context, tx *sql.Tx, site string, snapshot store.RecordingSnapshot) (int64, string, error) {
	var id int64
	var status string
	err := tx.QueryRowContext(ctx, `
SELECT id, status FROM recordings
WHERE site = ? AND network_id = ? AND service_id = ? AND event_id = ? AND active_program_start_at = ?`,
		site, snapshot.NetworkID, snapshot.ServiceID, snapshot.EventID, snapshot.ProgramStartAt).Scan(&id, &status)
	if err != nil {
		return 0, "", fmt.Errorf("reading live recording for event %d: %w", snapshot.EventID, err)
	}
	return id, status, nil
}

// updateRecordingStatus は録画の状態を進める。finished・failed・canceled からは降格させない。
func updateRecordingStatus(ctx context.Context, tx *sql.Tx, now time.Time, recordingID int64, observation store.RecordObservation) error {
	_, err := tx.ExecContext(ctx, `
UPDATE recordings SET
    status = CASE WHEN status IN ('finished', 'failed', 'canceled') THEN status ELSE ? END,
    started_at = COALESCE(started_at, ?),
    ended_at = CASE WHEN ? IS NOT NULL THEN ? ELSE ended_at END,
    updated_at = ?
WHERE id = ?`,
		store.NormalizeRecordingStatus(observation.RecordID, observation.Status),
		observation.StartedAt, observation.EndedAt, observation.EndedAt, now, recordingID)
	return err
}

// acquireRecordSync は (site, record_id) の record 同期行を確保する。既存行の内容には触れず、行ロックだけを取る。
func acquireRecordSync(ctx context.Context, tx *sql.Tx, observation store.RecordObservation, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO record_sync (site, record_id, recording_id, program_id, status, tags, observed_at)
VALUES (?, ?, NULL, ?, ?, '[]', ?)
ON DUPLICATE KEY UPDATE observed_at = record_sync.observed_at`,
		observation.Site, observation.RecordID, observation.ProgramID, observation.Status, now)
	return err
}

// recordSyncRecordingID は確保した record 同期行の recording_id を返す。未設定なら nil。
func recordSyncRecordingID(ctx context.Context, tx *sql.Tx, site, recordID string) (*int64, error) {
	var id sql.Null[int64]
	if err := tx.QueryRowContext(ctx, `SELECT recording_id FROM record_sync WHERE site = ? AND record_id = ?`, site, recordID).Scan(&id); err != nil {
		return nil, fmt.Errorf("reading record_sync recording: %w", err)
	}
	return nullPtr(id), nil
}

// upsertRecordSync は record 同期行の観測を書く。recording_id は既に入っていれば変えない。
func upsertRecordSync(ctx context.Context, tx *sql.Tx, now time.Time, observation store.RecordObservation, recordingID *int64) error {
	tags := observation.Tags
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("marshalling record tags: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO record_sync (
    site, record_id, recording_id, program_id, status, content_path, content_length, tags, failed_reason, observed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) AS incoming
ON DUPLICATE KEY UPDATE
    recording_id = COALESCE(record_sync.recording_id, incoming.recording_id),
    program_id = incoming.program_id,
    status = incoming.status,
    content_path = incoming.content_path,
    content_length = incoming.content_length,
    tags = incoming.tags,
    failed_reason = incoming.failed_reason,
    observed_at = incoming.observed_at`,
		observation.Site, observation.RecordID, recordingID, observation.ProgramID, observation.Status,
		observation.ContentPath, observation.ContentLength, string(tagsJSON), nullableJSON(observation.FailedReason), now)
	return err
}

// appendQualityEventIfMissing は録画の failure_reported が偽のときだけ品質イベントを追記する。
// 追記した（行が変わった）ときに true を返す。
func appendQualityEventIfMissing(ctx context.Context, tx *sql.Tx, now time.Time, recordingID int64, event string, reason json.RawMessage) (bool, error) {
	events, err := qualityEventsJSON(now, event, reason)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `
UPDATE recordings SET quality_events = JSON_MERGE_PRESERVE(quality_events, ?), failure_reported = TRUE, updated_at = ?
WHERE id = ? AND failure_reported = FALSE`, events, now, recordingID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// qualityEventsJSON は品質イベントを、tx の時計で付けた時刻つきの JSON 配列にする。
func qualityEventsJSON(at time.Time, event string, reason json.RawMessage) (string, error) {
	b, err := json.Marshal([]struct {
		Event  string          `json:"event"`
		Reason json.RawMessage `json:"reason"`
		At     time.Time       `json:"at"`
	}{{Event: event, Reason: reason, At: at}})
	if err != nil {
		return "", err
	}
	return string(b), nil
}
