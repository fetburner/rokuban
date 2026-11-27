package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// Reservations は MySQL で予約の読み取り、意図・上書きの書き込み、ruler パスを実装する。
type Reservations struct {
	db *sql.DB
}

// NewReservations は MySQL を使う予約 store を作る。
func NewReservations(db *sql.DB) *Reservations {
	return &Reservations{db: db}
}

const programIDTable = `JSON_TABLE(?, '$[*]' COLUMNS (program_id BIGINT PATH '$'))`

// upsertProgramSnapshots は射影がある番組のスナップショットを追従させる。
func upsertProgramSnapshots(ctx context.Context, tx *sql.Tx, site string, ids []int64, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
 INSERT INTO program_snapshots (
   site, program_id, title, start_at, duration_ms, network_id, service_id,
   channel_type, channel, event_id, service_name, updated_at
 )
 SELECT incoming.*
 FROM (
   SELECT p.site, p.program_id, p.name AS title, p.start_at, p.duration_ms, p.network_id,
          p.service_id, s.channel_type, s.channel, p.event_id, s.name AS service_name,
          CAST(? AS DATETIME(6)) AS updated_at
   FROM epg_programs p
   JOIN epg_services s USING (site, network_id, service_id)
   WHERE p.site = ? AND p.program_id IN (SELECT ids.program_id FROM `+programIDTable+` AS ids)
 ) AS incoming
 ORDER BY incoming.program_id
 ON DUPLICATE KEY UPDATE
   updated_at = IF(
     NOT (program_snapshots.title <=> incoming.title
      AND program_snapshots.start_at <=> incoming.start_at
      AND program_snapshots.duration_ms <=> incoming.duration_ms
      AND program_snapshots.network_id <=> incoming.network_id
      AND program_snapshots.service_id <=> incoming.service_id
      AND program_snapshots.channel_type <=> incoming.channel_type
      AND program_snapshots.channel <=> incoming.channel
      AND program_snapshots.event_id <=> incoming.event_id
      AND program_snapshots.service_name <=> incoming.service_name),
     incoming.updated_at, program_snapshots.updated_at),
   title = incoming.title, start_at = incoming.start_at, duration_ms = incoming.duration_ms,
   network_id = incoming.network_id, service_id = incoming.service_id, channel_type = incoming.channel_type,
   channel = incoming.channel, event_id = incoming.event_id, service_name = incoming.service_name
 `, now, site, programIDsJSON(ids)); err != nil {
		return fmt.Errorf("upserting program snapshots for %q: %w", site, err)
	}
	return nil
}

func ensureProgramSnapshot(ctx context.Context, tx *sql.Tx, site string, programID int64, now time.Time) error {
	if err := upsertProgramSnapshots(ctx, tx, site, []int64{programID}, now); err != nil {
		return err
	}
	var locked int64
	err := tx.QueryRowContext(ctx, `
 SELECT program_id FROM program_snapshots WHERE site = ? AND program_id = ? FOR UPDATE
 `, site, programID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		err = store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("locking program snapshot %d at %q: %w", programID, site, err)
	}
	return nil
}

// PutIntent はスナップショットをロックし、同じ番組の意図・上書きの操作と直列化する。
func (s *Reservations) PutIntent(ctx context.Context, site string, programID int64, action string) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := ensureProgramSnapshot(ctx, tx, site, programID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
  INSERT INTO program_intents (site, program_id, action, created_at, updated_at)
  VALUES (?, ?, ?, ?, ?) AS incoming
  ON DUPLICATE KEY UPDATE action = incoming.action, updated_at = incoming.updated_at
  `, site, programID, action, now, now); err != nil {
			return fmt.Errorf("upserting program intent: %w", err)
		}
		return bumpRulerInputRevision(ctx, tx, site)
	})
}

// DeleteIntent は番組の利用者意図を削除し、入力版を進める。
func (s *Reservations) DeleteIntent(ctx context.Context, site string, programID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM program_intents WHERE site = ? AND program_id = ?`, site, programID); err != nil {
			return fmt.Errorf("deleting program intent: %w", err)
		}
		return bumpRulerInputRevision(ctx, tx, site)
	})
}

// PatchOverrides はスナップショットをロックしてから上書きをマージ・検証し、入力版とともに確定する。
func (s *Reservations) PatchOverrides(ctx context.Context, site string, programID int64, patch reservation.OverridesPatch) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := ensureProgramSnapshot(ctx, tx, site, programID, now); err != nil {
			return err
		}
		// 既存の上書きと base は、ロックの後に方言のまま読む。判断は store.PlanOverridesPatch が行う。
		var base, overrides []byte
		if err := tx.QueryRowContext(ctx, `
  SELECT r.base, o.overrides
  FROM program_snapshots s
  LEFT JOIN reservations r USING (site, program_id)
  LEFT JOIN program_overrides o USING (site, program_id)
  WHERE s.site = ? AND s.program_id = ?
  `, site, programID).Scan(&base, &overrides); err != nil {
			return fmt.Errorf("reading reservation base and overrides: %w", err)
		}
		payload, empty, err := store.PlanOverridesPatch(base, overrides, patch)
		if err != nil {
			return fmt.Errorf("planning program overrides: %w", err)
		}
		if empty {
			// 不変条件 10: 空の上書き = 行が無い。
			if err := deleteProgramOverrides(ctx, tx, site, programID); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `
  INSERT INTO program_overrides (site, program_id, overrides, created_at, updated_at)
  VALUES (?, ?, CAST(? AS JSON), ?, ?) AS incoming
  ON DUPLICATE KEY UPDATE overrides = incoming.overrides, updated_at = incoming.updated_at
  `, site, programID, string(payload), now, now); err != nil {
			return fmt.Errorf("upserting program overrides: %w", err)
		}
		return bumpRulerInputRevision(ctx, tx, site)
	})
}

// DeleteOverrides は番組の上書きを削除し、入力版を進める。
func (s *Reservations) DeleteOverrides(ctx context.Context, site string, programID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := deleteProgramOverrides(ctx, tx, site, programID); err != nil {
			return err
		}
		return bumpRulerInputRevision(ctx, tx, site)
	})
}

func deleteProgramOverrides(ctx context.Context, tx *sql.Tx, site string, programID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM program_overrides WHERE site = ? AND program_id = ?`, site, programID); err != nil {
		return fmt.Errorf("deleting program overrides: %w", err)
	}
	return nil
}

const reservationJoins = `
 FROM reservations r
 JOIN program_snapshots s USING (site, program_id)
 LEFT JOIN program_intents i USING (site, program_id)
 LEFT JOIN program_overrides o USING (site, program_id)
`

const reservationMissingEvent = `EXISTS (
 SELECT 1 FROM never_scheduled_events n
 WHERE n.site = s.site AND n.network_id = s.network_id AND n.service_id = s.service_id
   AND n.event_id = s.event_id AND n.program_start_at = s.start_at
)`

const reservationDetailsQuery = `
 SELECT r.site, r.program_id, r.rule_id, r.base, r.created_at, r.updated_at,
        r.dedup_match_recording_id, r.dedup_similarity, s.title, s.start_at, s.duration_ms,
        s.channel_type, s.service_name, o.overrides, i.action,
        (` + reservationMissingEvent + ` AND NOT EXISTS (
          SELECT 1 FROM recordings rec
          WHERE rec.site = s.site AND rec.network_id = s.network_id
            AND rec.service_id = s.service_id AND rec.event_id = s.event_id
        )),
        (SELECT eps.value FROM epg_program_series eps WHERE eps.site = r.site AND eps.program_id = r.program_id)
` + reservationJoins

func scanReservationDetails(scanner interface{ Scan(...any) error }) (store.ReservationDetails, error) {
	var detail store.ReservationDetails
	var base, overrides []byte
	r, s := &detail.Reservation, &detail.Snapshot
	err := scanner.Scan(&r.Site, &r.ProgramID, &r.RuleID, &base, &r.CreatedAt, &r.UpdatedAt, &r.DedupMatchRecordingID,
		&r.DedupSimilarity, &s.Title, &s.StartAt, &s.DurationMs, &s.ChannelType, &s.ServiceName,
		&overrides, &detail.IntentAction, &detail.NeverRecorded, &detail.Series)
	r.Base, detail.Overrides = base, overrides
	return detail, err
}

// GetReservation は番組 ID で予約の詳細を返す。行が無ければ ErrNotFound を返す。
func (s *Reservations) GetReservation(ctx context.Context, site string, programID int64) (store.ReservationDetails, error) {
	detail, err := scanReservationDetails(s.db.QueryRowContext(ctx, reservationDetailsQuery+`
 WHERE r.site = ? AND r.program_id = ?
 `, site, programID))
	if errors.Is(err, sql.ErrNoRows) {
		err = store.ErrNotFound
	}
	if err != nil {
		return store.ReservationDetails{}, fmt.Errorf("reading reservation %d at %q: %w", programID, site, err)
	}
	return detail, nil
}

// ListReservations は予約の詳細をサイト・番組開始時刻順で返す。
func (s *Reservations) ListReservations(ctx context.Context) ([]store.ReservationDetails, error) {
	return queryRows(ctx, s.db, "reservations", reservationDetailsQuery+` ORDER BY r.site, s.start_at`,
		func(rows *sql.Rows) (store.ReservationDetails, error) { return scanReservationDetails(rows) })
}

// ListOverlaps は射影上の番組の放送時間帯と重なる同一サイトの予約を返す。
func (s *Reservations) ListOverlaps(ctx context.Context, site string, programID int64) ([]store.ReservationOverlap, error) {
	var start, end time.Time
	err := s.db.QueryRowContext(ctx, `SELECT start_at, end_at FROM epg_programs WHERE site = ? AND program_id = ?`, site, programID).Scan(&start, &end)
	if errors.Is(err, sql.ErrNoRows) {
		err = store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading overlap window for program %d at %q: %w", programID, site, err)
	}

	return queryRows(ctx, s.db, "overlapping reservations", `
 SELECT r.program_id, s.title, s.start_at, s.duration_ms, r.base, o.overrides, i.action
 `+reservationJoins+`
 WHERE r.site = ? AND r.program_id <> ? AND NOT `+reservationMissingEvent+`
   AND s.start_at < ? AND TIMESTAMPADD(MICROSECOND, s.duration_ms * 1000, s.start_at) > ?
 ORDER BY s.start_at
 `, func(rows *sql.Rows) (store.ReservationOverlap, error) {
		var r store.ReservationOverlap
		var base, overrides []byte
		err := rows.Scan(&r.ProgramID, &r.Title, &r.StartAt, &r.DurationMs, &base, &overrides, &r.IntentAction)
		r.Base, r.Overrides = base, overrides
		return r, err
	}, site, programID, end, start)
}

var _ store.Reservations = (*Reservations)(nil)
