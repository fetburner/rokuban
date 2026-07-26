package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

var version = "dev"

// defaultSite は M1 の単一サイト構成でのサイト名。定義は db.DefaultSite（唯一の出所）。
const defaultSite = db.DefaultSite

// Server は予約 API のハンドラ実装。oapi-codegen の StrictServerInterface を満たす。
type Server struct {
	pool *pgxpool.Pool
}

// NewServer は Server を生成する。
func NewServer(pool *pgxpool.Pool) *Server {
	return &Server{pool: pool}
}

// Healthz はヘルスチェックエンドポイント。
func (h *Server) Healthz(_ context.Context, _ HealthzRequestObject) (HealthzResponseObject, error) {
	return Healthz200JSONResponse{Status: "ok"}, nil
}

// GetVersion はサーバーバージョンを返す。
func (h *Server) GetVersion(_ context.Context, _ GetVersionRequestObject) (GetVersionResponseObject, error) {
	return GetVersion200JSONResponse{Version: version}, nil
}

// ListReservations は予約一覧を返す。
func (h *Server) ListReservations(ctx context.Context, _ ListReservationsRequestObject) (ListReservationsResponseObject, error) {
	q := sqlcgen.New(h.pool)
	rows, err := q.ListReservationsBySite(ctx, defaultSite)
	if err != nil {
		return nil, err
	}

	result := make([]Reservation, 0, len(rows))
	for _, r := range rows {
		result = append(result, reservationFromRow(r.Reservation, r.IntentOverrides))
	}
	return ListReservations200JSONResponse(result), nil
}

// CreateReservation は手動予約を作成する。
//
// ユーザー意図（program_intents）と導出行（reservations）を同一トランザクションで書く。
// 意図だけ書いて ruler のパスを待つ形にはしない（作成が UI に即座に反映されないため）。
func (h *Server) CreateReservation(ctx context.Context, req CreateReservationRequestObject) (CreateReservationResponseObject, error) {
	overrides := db.ReservationOptions{}
	if req.Body.Priority != nil {
		overrides.Priority = req.Body.Priority
	}
	overridesJSON, err := json.Marshal(overrides)
	if err != nil {
		return nil, err
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	// 意図が先。導出行は ruler が作り直せるが、意図は誰も再生成できない。
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site:              defaultSite,
		ProgramID:         req.Body.ProgramId,
		Action:            db.IntentRecord,
		Overrides:         overridesJSON,
		ProgramStartAt:    req.Body.StartAt,
		ProgramDurationMs: req.Body.DurationMs,
	}); err != nil {
		return nil, fmt.Errorf("upserting program intent: %w", err)
	}

	row, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:              defaultSite,
		ProgramID:         req.Body.ProgramId,
		Title:             req.Body.Title,
		ProgramStartAt:    req.Body.StartAt,
		ProgramDurationMs: req.Body.DurationMs,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return CreateReservation409JSONResponse{Error: "reservation already exists for this program"}, nil
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	res := reservationFromRow(row, overridesJSON)
	return CreateReservation201JSONResponse(res), nil
}

// GetReservation は指定 ID の予約を返す。
func (h *Server) GetReservation(ctx context.Context, req GetReservationRequestObject) (GetReservationResponseObject, error) {
	q := sqlcgen.New(h.pool)
	row, err := q.GetReservationFull(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetReservation404JSONResponse{Error: "reservation not found"}, nil
		}
		return nil, err
	}
	res := reservationFromRow(row.Reservation, row.IntentOverrides)
	return GetReservation200JSONResponse(res), nil
}

// DeleteReservation は予約を取消す。
//
// 取消は**無条件に intent{skip} を書いて導出行を落とす**。行を消すだけでは
// 「消された行」と「最初から無かった行」が ruler から区別できず、次の全量パスが
// 復活させてしまう（docs/recording.md §4.4）。意図が別表に残るので、
// 再生成者がいるかで分岐する必要はない。
func (h *Server) DeleteReservation(ctx context.Context, req DeleteReservationRequestObject) (DeleteReservationResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	row, err := q.GetReservationFull(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeleteReservation404JSONResponse{Error: "reservation not found"}, nil
		}
		return nil, err
	}

	if _, err := q.SkipProgram(ctx, sqlcgen.SkipProgramParams{
		Site:              row.Reservation.Site,
		ProgramID:         row.Reservation.ProgramID,
		ProgramStartAt:    row.Reservation.ProgramStartAt,
		ProgramDurationMs: row.Reservation.ProgramDurationMs,
	}); err != nil {
		return nil, fmt.Errorf("recording skip intent: %w", err)
	}
	if _, err := q.DeleteReservation(ctx, req.Id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return DeleteReservation204Response{}, nil
}

// reservationFromRow は予約行とユーザー意図（program_intents）から API 表現を組む。
// overrides は予約行ではなく意図側にあるので、両方を受け取る。
func reservationFromRow(r sqlcgen.Reservation, intentOverrides []byte) Reservation {
	res := Reservation{
		Id:         r.ID,
		ProgramId:  r.ProgramID,
		Source:     ReservationSource(r.Source),
		State:      ReservationState(r.State),
		Title:      r.Title,
		StartAt:    r.ProgramStartAt,
		DurationMs: r.ProgramDurationMs,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
	}
	if r.RuleID != nil {
		res.RuleId = r.RuleID
	}
	if len(intentOverrides) > 0 && string(intentOverrides) != "{}" {
		var m map[string]interface{}
		if json.Unmarshal(intentOverrides, &m) == nil && len(m) > 0 {
			res.Overrides = &m
		}
	}
	return res
}
