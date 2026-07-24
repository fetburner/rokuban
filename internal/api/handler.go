package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

var version = "dev"

const defaultSite = "default"

type Server struct {
	pool *pgxpool.Pool
}

func NewServer(pool *pgxpool.Pool) *Server {
	return &Server{pool: pool}
}

func (h *Server) Healthz(_ context.Context, _ HealthzRequestObject) (HealthzResponseObject, error) {
	return Healthz200JSONResponse{Status: "ok"}, nil
}

func (h *Server) GetVersion(_ context.Context, _ GetVersionRequestObject) (GetVersionResponseObject, error) {
	return GetVersion200JSONResponse{Version: version}, nil
}

func (h *Server) ListReservations(ctx context.Context, _ ListReservationsRequestObject) (ListReservationsResponseObject, error) {
	q := sqlcgen.New(h.pool)
	rows, err := q.ListReservationsBySite(ctx, defaultSite)
	if err != nil {
		return nil, err
	}

	result := make([]Reservation, 0, len(rows))
	for _, r := range rows {
		result = append(result, reservationFromRow(r))
	}
	return ListReservations200JSONResponse(result), nil
}

func (h *Server) CreateReservation(ctx context.Context, req CreateReservationRequestObject) (CreateReservationResponseObject, error) {
	overrides := db.ReservationOptions{}
	if req.Body.Priority != nil {
		overrides.Priority = req.Body.Priority
	}
	overridesJSON, err := json.Marshal(overrides)
	if err != nil {
		return nil, err
	}

	q := sqlcgen.New(h.pool)
	row, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:              defaultSite,
		ProgramID:         req.Body.ProgramId,
		Overrides:         overridesJSON,
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

	res := reservationFromRow(row)
	return CreateReservation201JSONResponse(res), nil
}

func (h *Server) GetReservation(ctx context.Context, req GetReservationRequestObject) (GetReservationResponseObject, error) {
	q := sqlcgen.New(h.pool)
	row, err := q.GetReservationFull(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetReservation404JSONResponse{Error: "reservation not found"}, nil
		}
		return nil, err
	}
	res := reservationFromRow(row)
	return GetReservation200JSONResponse(res), nil
}

func (h *Server) DeleteReservation(ctx context.Context, req DeleteReservationRequestObject) (DeleteReservationResponseObject, error) {
	q := sqlcgen.New(h.pool)
	n, err := q.DeleteReservation(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return DeleteReservation404JSONResponse{Error: "reservation not found"}, nil
	}
	return DeleteReservation204Response{}, nil
}

func reservationFromRow(r sqlcgen.Reservation) Reservation {
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
	if len(r.Overrides) > 0 && string(r.Overrides) != "{}" {
		var m map[string]interface{}
		if json.Unmarshal(r.Overrides, &m) == nil && len(m) > 0 {
			res.Overrides = &m
		}
	}
	return res
}
