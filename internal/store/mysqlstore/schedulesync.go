package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// scheduleSyncChunkRows は 1 文に詰める観測行の数。1 行 7 個のプレースホルダで、MySQL の上限（65535）を超えない。
const scheduleSyncChunkRows = 1000

// ScheduleSync は reconciler の schedule 同期を MySQL で実装する。
type ScheduleSync struct {
	db *sql.DB
}

// NewScheduleSync は MySQL を使う schedule 同期 store を作る。
func NewScheduleSync(db *sql.DB) *ScheduleSync {
	return &ScheduleSync{db: db}
}

var _ store.ScheduleSync = (*ScheduleSync)(nil)
var _ store.ScheduleSyncReader = (*ScheduleSync)(nil)

// ReplaceObserved は site の観測を observed で全部入れ替え、snapshot marker を同じ tx で進める。
// 全量 snapshot なので古い行を時刻で選ぶ必要が無く、site の行を消してから入れ直す。
// 時刻は tx の開始時に 1 回読み、観測行と marker に同じ値を書く。NOW() は文ごとの時刻なので使わない。
func (s *ScheduleSync) ReplaceObserved(ctx context.Context, site string, observed []store.ObservedSchedule) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		at, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM schedule_sync WHERE site = ?`, site); err != nil {
			return fmt.Errorf("deleting schedule observations for site %s: %w", site, err)
		}
		for start := 0; start < len(observed); start += scheduleSyncChunkRows {
			end := min(start+scheduleSyncChunkRows, len(observed))
			if err := insertScheduleObservations(ctx, tx, site, at, observed[start:end]); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO schedule_sync_snapshots (site, snapshot_at) VALUES (?, ?)
AS incoming ON DUPLICATE KEY UPDATE snapshot_at = incoming.snapshot_at`, site, at); err != nil {
			return fmt.Errorf("updating schedule snapshot marker for site %s: %w", site, err)
		}
		return nil
	})
}

const scheduleSyncTuple = "(?, ?, ?, ?, ?, ?, ?)"

// insertScheduleObservations は 1 文で rows を挿入する。空の rows では呼ばない。
// options が不正な JSON なら MySQL が文ごと拒否し、呼び出し元の tx が巻き戻す。
func insertScheduleObservations(ctx context.Context, tx *sql.Tx, site string, at time.Time, rows []store.ObservedSchedule) error {
	args := make([]any, 0, len(rows)*7)
	for _, row := range rows {
		tags := row.Tags
		if tags == nil {
			tags = []string{}
		}
		tagsJSON, err := json.Marshal(tags)
		if err != nil {
			return fmt.Errorf("encoding schedule tags for program %d: %w", row.ProgramID, err)
		}
		// FailedReason が無いときは NULL を書く。
		var failed any
		if len(row.FailedReason) > 0 {
			failed = string(row.FailedReason)
		}
		args = append(args, site, row.ProgramID, row.State, string(row.Options), string(tagsJSON), failed, at)
	}
	query := `INSERT INTO schedule_sync (site, program_id, state, options, tags, failed_reason, observed_at) VALUES ` +
		strings.Repeat(scheduleSyncTuple+", ", len(rows)-1) + scheduleSyncTuple
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("inserting schedule observations for site %s: %w", site, err)
	}
	return nil
}

// ListForSyncEvaluation は同期対象の候補行を返す。予約・番組・欠測の述語は reservations.go の定数を使う。
func (s *ScheduleSync) ListForSyncEvaluation(ctx context.Context, site string) ([]reservation.SyncEvaluationRow, error) {
	return queryRows(ctx, s.db, "schedule sync evaluation rows", `
SELECT r.program_id, r.base, s.title, s.start_at, s.duration_ms, s.channel_type, s.channel,
       s.network_id, s.service_id, s.event_id, i.action, o.overrides
`+reservationJoins+`
WHERE r.site = ? AND NOT `+reservationMissingEvent+`
ORDER BY s.start_at, r.program_id`,
		scanScheduleSyncEvaluation, site)
}

func scanScheduleSyncEvaluation(rows *sql.Rows) (reservation.SyncEvaluationRow, error) {
	var row reservation.SyncEvaluationRow
	var base, overrides []byte
	r, p := &row.Reservation, &row.ProgramSnapshot
	if err := rows.Scan(&r.ProgramID, &base, &p.Title, &p.StartAt, &p.DurationMs, &p.ChannelType, &p.Channel,
		&p.NetworkID, &p.ServiceID, &p.EventID, &row.IntentAction, &overrides); err != nil {
		return reservation.SyncEvaluationRow{}, err
	}
	r.Base, row.Overrides = base, overrides
	return row, nil
}

// broadcastEventKeyWire は JSON_TABLE に渡す放送イベントキーの形。
type broadcastEventKeyWire struct {
	NetworkID int32 `json:"network_id"`
	ServiceID int32 `json:"service_id"`
	EventID   int32 `json:"event_id"`
}

// ListStartedBroadcastEventKeys は開始済み録画がある放送イベントキーを返す。
// キーは 1 つの JSON 引数で渡す。キーの件数分のプレースホルダを作ると MySQL の上限（65535）に掛かる。
// 結合は recordings の一意索引（site から始まる）が使えるよう、site を結合条件にも書く。
func (s *ScheduleSync) ListStartedBroadcastEventKeys(ctx context.Context, site string, keys []store.BroadcastEventKey, startedAfter time.Time) ([]store.BroadcastEventKey, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	wire := make([]broadcastEventKeyWire, len(keys))
	for i, key := range keys {
		wire[i] = broadcastEventKeyWire{NetworkID: key.NetworkID, ServiceID: key.ServiceID, EventID: key.EventID}
	}
	keysJSON, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encoding broadcast event keys: %w", err)
	}
	return queryRows(ctx, s.db, "started broadcast event keys", `
SELECT DISTINCT rec.network_id, rec.service_id, rec.event_id
FROM JSON_TABLE(?, '$[*]' COLUMNS (
         network_id INT PATH '$.network_id',
         service_id INT PATH '$.service_id',
         event_id INT PATH '$.event_id'
     )) AS k
JOIN recordings rec
  ON rec.site = ? AND rec.network_id = k.network_id AND rec.service_id = k.service_id AND rec.event_id = k.event_id
WHERE rec.started_at IS NOT NULL AND rec.started_at >= ?`,
		func(rows *sql.Rows) (store.BroadcastEventKey, error) {
			var key store.BroadcastEventKey
			err := rows.Scan(&key.NetworkID, &key.ServiceID, &key.EventID)
			return key, err
		}, string(keysJSON), site, startedAfter)
}

// SnapshotAt は最後に確定した全量 snapshot の時刻を返す。marker が無ければ found は false である。
func (s *ScheduleSync) SnapshotAt(ctx context.Context, site string) (time.Time, bool, error) {
	var at time.Time
	err := s.db.QueryRowContext(ctx, `SELECT snapshot_at FROM schedule_sync_snapshots WHERE site = ?`, site).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("getting schedule snapshot marker: %w", err)
	}
	return at, true, nil
}

// ListObserved は 1 site の schedule 観測を program ID 順で返す。
func (s *ScheduleSync) ListObserved(ctx context.Context, site string) ([]store.ObservedSchedule, error) {
	return queryRows(ctx, s.db, "schedule observations", `
SELECT program_id, state, options, tags, failed_reason FROM schedule_sync WHERE site = ? ORDER BY program_id`,
		func(rows *sql.Rows) (store.ObservedSchedule, error) {
			var o store.ObservedSchedule
			var options, tags, failed []byte
			if err := rows.Scan(&o.ProgramID, &o.State, &options, &tags, &failed); err != nil {
				return store.ObservedSchedule{}, err
			}
			if err := json.Unmarshal(tags, &o.Tags); err != nil {
				return store.ObservedSchedule{}, fmt.Errorf("decoding schedule tags: %w", err)
			}
			o.Options, o.FailedReason = options, failed
			return o, nil
		}, site)
}
