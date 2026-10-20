package api

import (
	"context"
	"errors"

	"github.com/fetburner/rokuban/internal/store"
)

// GetProgramReservation は指定番組の予約を (site, programId) を宛先に返す
// (GET /api/sites/{site}/programs/{programId}/reservation、issue #99)。
//
// 書き込み側（program_intents / program_overrides）は (site, programId) を宛先にしている
// （issue #29）。reservations もこの複合キーを主キーに持つので、予約行が再実体化されても
// このエンドポイントの URL は変わらない（不変条件 9「導出器が作るキーを宛先にしない」）。
func (h *Server) GetProgramReservation(ctx context.Context, req GetProgramReservationRequestObject) (GetProgramReservationResponseObject, error) {
	if !h.knownSite(req.Site) {
		return GetProgramReservation404JSONResponse{Error: "unknown site"}, nil
	}

	row, err := h.reservationStore.GetReservation(ctx, req.Site, req.ProgramId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return GetProgramReservation404JSONResponse{Error: "no reservation for this program"}, nil
		}
		return nil, err
	}
	res, err := reservationFromRow(row)
	if err != nil {
		return nil, err
	}
	return GetProgramReservation200JSONResponse(res), nil
}
