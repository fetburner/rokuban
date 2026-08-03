package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// GetProgramReservation は指定番組の予約を (site, programId) を宛先に返す
// (GET /api/sites/{site}/programs/{programId}/reservation、issue #99)。
//
// GetReservation（GET /api/reservations/{id}）は reservations.id という ruler の
// 導出削除・再実体化で変わりうる不安定な値を宛先にしている。書き込み側
// （program_intents / program_overrides）は M3-1（issue #29）で既に
// (site, programId) に寄せていたが、読み取りは #53 が mirakc の tag に適用した
// のと同じ論法をまだ適用していなかった。このハンドラでその読み取り側を埋める:
// UNIQUE (site, program_id) がキーとして成立するので、予約行が再実体化されて
// id が変わってもこのエンドポイントの URL は変わらない
// （CLAUDE.md 不変条件 9「導出器が作るキーを宛先にしない」）。
func (h *Server) GetProgramReservation(ctx context.Context, req GetProgramReservationRequestObject) (GetProgramReservationResponseObject, error) {
	if req.Site != h.site {
		return GetProgramReservation404JSONResponse{Error: "unknown site"}, nil
	}

	q := sqlcgen.New(h.pool)
	row, err := q.GetReservationFullBySiteAndProgramID(ctx, sqlcgen.GetReservationFullBySiteAndProgramIDParams{
		Site:      req.Site,
		ProgramID: req.ProgramId,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetProgramReservation404JSONResponse{Error: "no reservation for this program"}, nil
		}
		return nil, err
	}
	res, err := reservationFromRow(row.Reservation, row.ProgramSnapshot, row.Overrides, row.IntentAction, row.NeverRecorded)
	if err != nil {
		return nil, err
	}
	return GetProgramReservation200JSONResponse(res), nil
}
