package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// GetProgramOverlaps は指定番組の放送時間帯と重なる既存予約の件数と内訳を返す
// (GET /api/programs/{programId}/overlaps)。
//
// チューナー射影は使わず、自分の予約だけを数えた事実を返す。半開区間で重なりを
// 判定し、勝敗や容量超過の判定は行わない。
func (h *Server) GetProgramOverlaps(ctx context.Context, req GetProgramOverlapsRequestObject) (GetProgramOverlapsResponseObject, error) {
	if !h.knownSite(req.Site) {
		return GetProgramOverlaps404JSONResponse{Error: "unknown site"}, nil
	}
	rows, err := h.reservationStore.ListOverlaps(ctx, req.Site, req.ProgramId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return GetProgramOverlaps404JSONResponse{Error: "program not found in EPG projection"}, nil
		}
		return nil, fmt.Errorf("listing overlapping reservations: %w", err)
	}

	overlaps := make([]OverlappingReservation, 0, len(rows))
	for _, row := range rows {
		effective, err := reservation.EffectiveOptions(row.Base, row.Overrides, row.IntentAction)
		if err != nil {
			return nil, fmt.Errorf("resolving effective options for program %d: %w", row.ProgramID, err)
		}
		if effective.IsSkipped() {
			continue
		}
		overlaps = append(overlaps, OverlappingReservation{
			ProgramId: row.ProgramID, Title: row.Title, StartAt: row.StartAt, DurationMs: row.DurationMs,
		})
	}

	return GetProgramOverlaps200JSONResponse{Count: len(overlaps), Reservations: overlaps}, nil
}
