package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// Reservations は利用者の録画意図・上書きの書き込みと、予約の読み取りを PostgreSQL で実装する。
type Reservations struct {
	pool *pgxpool.Pool
}

// NewReservations は PostgreSQL の Reservations を作る。
func NewReservations(pool *pgxpool.Pool) *Reservations {
	return &Reservations{pool: pool}
}

// PutIntent は番組のスナップショットを確保し、利用者の録画意図を同じ tx で書く。
// 同じ番組への PutIntent・PatchOverrides とは、スナップショットの行ロックで直列化される。
func (s *Reservations) PutIntent(ctx context.Context, site string, programID int64, action string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning intent write for program %d at site %s: %w", programID, site, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if err := ensureProgramSnapshot(ctx, q, site, programID); err != nil {
		return err
	}
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: site, ProgramID: programID, Action: action,
	}); err != nil {
		return fmt.Errorf("upserting intent for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	if err := bumpRulerInputRevision(ctx, q, site); err != nil {
		return fmt.Errorf("bumping ruler input revision after intent write: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing intent for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	return nil
}

// DeleteIntent は利用者の録画意図を消す。意図が無ければすでに望む状態なので成功する。
// program_overrides には触れない（意図と上書きは別の軸。分離の核心）。
func (s *Reservations) DeleteIntent(ctx context.Context, site string, programID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning intent delete for program %d at site %s: %w", programID, site, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if _, err := q.DeleteProgramIntent(ctx, sqlcgen.DeleteProgramIntentParams{
		Site: site, ProgramID: programID,
	}); err != nil {
		return fmt.Errorf("deleting intent for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	if err := bumpRulerInputRevision(ctx, q, site); err != nil {
		return fmt.Errorf("bumping ruler input revision after intent delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing intent delete for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	return nil
}

// PatchOverrides は、番組のスナップショットをロックしてから既存の上書きを読み、
// パッチをマージして検証し、同じ tx で書く。
// program_intents（action）には触れない（分離の核心。ルール由来の予約に PATCH しても
// 手動予約の意図を巻き込む経路が構造的に無い）。
func (s *Reservations) PatchOverrides(ctx context.Context, site string, programID int64, patch reservation.OverridesPatch) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning overrides patch for program %d at site %s: %w", programID, site, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	// 既存の上書きを読むより先に、スナップショットの行ロックを取る。先に読むと、
	// 別フィールドの同時 PATCH が先行の commit 前の値からマージして互いの値を消す。
	if err := ensureProgramSnapshot(ctx, q, site, programID); err != nil {
		return err
	}

	existing, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: site, ProgramID: programID})
	var existingOptions reservation.Options
	if err == nil {
		if err := json.Unmarshal(existing.Overrides, &existingOptions); err != nil {
			return fmt.Errorf("unmarshalling existing overrides for program %d at site %s: %w", programID, site, err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("getting overrides for program %d at site %s: %w", programID, site, err)
	}
	merged, err := reservation.MergeOverrides(existingOptions, patch)
	if err != nil {
		return fmt.Errorf("merging overrides for program %d at site %s: %w", programID, site, err)
	}

	// このパッチを適用した後の実効値（既存の上書き + パッチ + ルールの base）で
	// keepOriginal / encodeProfiles の組み合わせを検証する。リクエスト単体では
	// 組み合わせが見えないので、必ずマージ後の実効値を見る。base はルール由来の予約が
	// あればそこから、無ければ nil（手動予約相当）として扱う。
	var baseJSON json.RawMessage
	base, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{
		Site: site, ProgramID: programID,
	})
	if err == nil {
		baseJSON = base.Base
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("getting reservation base for program %d at site %s: %w", programID, site, err)
	}

	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshalling merged overrides for program %d at site %s: %w", programID, site, err)
	}
	effective, err := reservation.EffectiveOptions(baseJSON, mergedJSON, nil)
	if err != nil {
		return fmt.Errorf("computing effective options for program %d at site %s: %w", programID, site, err)
	}
	if err := reservation.ValidateEffectiveKeepOriginal(effective); err != nil {
		return fmt.Errorf("validating effective options for program %d at site %s: %w", programID, site, err)
	}
	if err := persistOverrides(ctx, q, site, programID, mergedJSON); err != nil {
		return fmt.Errorf("persisting overrides for program %d at site %s: %w", programID, site, err)
	}
	if err := bumpRulerInputRevision(ctx, q, site); err != nil {
		return fmt.Errorf("bumping ruler input revision after overrides patch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing overrides for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	return nil
}

// DeleteOverrides は番組の上書きをすべて消す（「ルールに戻す」）。
// program_intents（action）には触れない（分離の核心）。行が無くても成功する。
func (s *Reservations) DeleteOverrides(ctx context.Context, site string, programID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning overrides delete for program %d at site %s: %w", programID, site, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if _, err := q.DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{
		Site: site, ProgramID: programID,
	}); err != nil {
		return fmt.Errorf("deleting overrides for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	if err := bumpRulerInputRevision(ctx, q, site); err != nil {
		return fmt.Errorf("bumping ruler input revision after overrides delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing overrides delete for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	return nil
}

// GetReservation は予約を、番組スナップショットと利用者の入力つきで 1 件返す。無ければ ErrNotFound。
func (s *Reservations) GetReservation(ctx context.Context, site string, programID int64) (store.ReservationDetails, error) {
	row, err := sqlcgen.New(s.pool).GetReservationFullBySiteAndProgramID(ctx, sqlcgen.GetReservationFullBySiteAndProgramIDParams{
		Site: site, ProgramID: programID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ReservationDetails{}, fmt.Errorf("reservation for program %d at site %s: %w", programID, site, store.ErrNotFound)
		}
		return store.ReservationDetails{}, fmt.Errorf("getting reservation for program %d at site %s: %w", programID, site, err)
	}
	return reservationDetails(row.Reservation, row.ProgramSnapshot, row.Overrides, row.IntentAction, row.NeverRecorded, row.Series), nil
}

// ListReservations は予約を詳細つきで、site と番組の開始時刻の順に返す。
func (s *Reservations) ListReservations(ctx context.Context) ([]store.ReservationDetails, error) {
	rows, err := sqlcgen.New(s.pool).ListReservationsFull(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing reservations: %w", err)
	}
	result := make([]store.ReservationDetails, 0, len(rows))
	for _, row := range rows {
		result = append(result, reservationDetails(row.Reservation, row.ProgramSnapshot, row.Overrides, row.IntentAction, row.NeverRecorded, row.Series))
	}
	return result, nil
}

// ListOverlaps は EPG 射影の番組の放送時間帯と重なる予約の候補を返す。射影に番組が無ければ ErrNotFound。
func (s *Reservations) ListOverlaps(ctx context.Context, site string, programID int64) ([]store.ReservationOverlap, error) {
	q := sqlcgen.New(s.pool)
	program, err := q.GetEpgProgram(ctx, sqlcgen.GetEpgProgramParams{Site: site, ProgramID: programID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("program %d at site %s: %w", programID, site, store.ErrNotFound)
		}
		return nil, fmt.Errorf("getting broadcast window for program %d at site %s: %w", programID, site, err)
	}
	rows, err := q.ListOverlappingReservations(ctx, sqlcgen.ListOverlappingReservationsParams{
		Site: site, TargetProgramID: programID,
		WindowStart: program.EpgProgram.StartAt, WindowEnd: program.EpgProgram.EndAt,
	})
	if err != nil {
		return nil, fmt.Errorf("listing reservations overlapping program %d at site %s: %w", programID, site, err)
	}
	result := make([]store.ReservationOverlap, 0, len(rows))
	for _, row := range rows {
		result = append(result, store.ReservationOverlap{
			ProgramID: row.Reservation.ProgramID, Title: row.ProgramSnapshot.Title,
			StartAt: row.ProgramSnapshot.StartAt, DurationMs: row.ProgramSnapshot.DurationMs,
			Base: row.Reservation.Base, Overrides: row.Overrides, IntentAction: row.IntentAction,
		})
	}
	return result, nil
}

// ensureProgramSnapshot は program_snapshots に (site, programID) の行があることを保証し、
// その行をロックする。program_intents / program_overrides はこの行への FK を持つので、
// それらの書き込みより先に呼ぶ。
//
// EPG 射影に番組があれば最新の事実で upsert する（射影にある間は更新）。射影から消えていても
// 既存のスナップショット行があればそのまま使う（凍結。放送済みで射影のローリングウィンドウから
// 外れた後も、既存の予約の上書きを編集できるようにするため）。どちらの場合も
// lockProgramSnapshot でロックを取るので、ロックの獲得は upsert の副作用に頼らない。
// 射影にも既存の行にも無ければ store.ErrNotFound を返す。GC が先に消した場合も、行は作り直さずに
// ErrNotFound を返す。
func ensureProgramSnapshot(ctx context.Context, q *sqlcgen.Queries, site string, programID int64) error {
	source, err := q.GetProgramSnapshotSource(ctx, sqlcgen.GetProgramSnapshotSourceParams{Site: site, ProgramID: programID})
	if err == nil {
		if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
			Site: site, ProgramID: programID, Title: source.Title, StartAt: source.StartAt,
			DurationMs: source.DurationMs, NetworkID: source.NetworkID, ServiceID: source.ServiceID,
			ChannelType: source.ChannelType, Channel: source.Channel, EventID: source.EventID, ServiceName: source.ServiceName,
		}); err != nil {
			return fmt.Errorf("upserting program snapshot for program %d at site %s: %w", programID, site, mapDatabaseError(err))
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("getting program snapshot source for program %d at site %s: %w", programID, site, err)
	}
	return lockProgramSnapshot(ctx, q, site, programID)
}

func reservationDetails(r sqlcgen.Reservation, snapshot sqlcgen.ProgramSnapshot, overrides json.RawMessage, intentAction *string, neverRecorded bool, series *string) store.ReservationDetails {
	var similarity *float32
	if r.DedupSimilarity.Valid {
		value := r.DedupSimilarity.Float32
		similarity = &value
	}
	return store.ReservationDetails{
		Reservation: store.ReservationRecord{
			Site: r.Site, ProgramID: r.ProgramID, RuleID: r.RuleID, Base: r.Base,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			DedupMatchRecordingID: r.DedupMatchRecordingID, DedupSimilarity: similarity,
		},
		Snapshot: store.ProgramSnapshot{
			Title: snapshot.Title, StartAt: snapshot.StartAt, DurationMs: snapshot.DurationMs,
			ChannelType: snapshot.ChannelType, ServiceName: snapshot.ServiceName,
		},
		Overrides: overrides, IntentAction: intentAction, NeverRecorded: neverRecorded, Series: series,
	}
}

// persistOverrides はマージ結果（marshal 済み）を書き戻す。結果が空（{}）なら行そのものを
// 消す（不変条件 10: 空の上書き = 行が無い）。空でなければ upsert する。
func persistOverrides(ctx context.Context, q *sqlcgen.Queries, site string, programID int64, finalJSON json.RawMessage) error {
	if isEmptyOverridesJSON(finalJSON) {
		if _, err := q.DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{Site: site, ProgramID: programID}); err != nil {
			return fmt.Errorf("deleting emptied overrides: %w", mapDatabaseError(err))
		}
		return nil
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: site, ProgramID: programID, Overrides: finalJSON,
	}); err != nil {
		return fmt.Errorf("upserting overrides: %w", mapDatabaseError(err))
	}
	return nil
}

// isEmptyOverridesJSON は jsonb の上書きが実質空（{}）かどうかを判定する。
// reservation.Options の全フィールドが omitempty なので、json.Marshal の結果が {} であることと
// 「型付き構造体に何も設定されていない」は同値になる（新しいフィールドを足しても自動的に正しく動く）。
func isEmptyOverridesJSON(raw json.RawMessage) bool {
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return false
	}
	return len(values) == 0
}

var _ store.Reservations = (*Reservations)(nil)
