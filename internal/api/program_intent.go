package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// PutProgramIntent はユーザー意図（record/skip）を (site, programId) を自身の
// キーとして書く (PUT /api/sites/{site}/programs/{programId}/intent、issue #29)。
//
// PutProgramIntent は reservations に触れない。導出の書き手は ruler で、例外はルール削除 API の
// 同期削除 1 本（docs/schema/reservations.md §3「書き込み所有権」）。commit 後に ruler_pass の
// ヒントを入れて実体化を早めるが、reservations は作らない
// （#29 の決定: 作成の即時反映は UI の楽観更新で満たす）。
func (h *Server) PutProgramIntent(ctx context.Context, req PutProgramIntentRequestObject) (PutProgramIntentResponseObject, error) {
	if !h.knownSite(req.Site) {
		return PutProgramIntent400JSONResponse{Error: "unknown site"}, nil
	}
	if req.Body == nil {
		return PutProgramIntent400JSONResponse{Error: "request body is required"}, nil
	}

	var action string
	switch req.Body.Action {
	case Record:
		action = reservation.IntentRecord
	case Skip:
		action = reservation.IntentSkip
	default:
		return PutProgramIntent400JSONResponse{Error: fmt.Sprintf("invalid action %q", req.Body.Action)}, nil
	}

	if err := h.reservationStore.PutIntent(ctx, req.Site, req.ProgramId, action); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PutProgramIntent400JSONResponse{Error: "program not found in EPG projection"}, nil
		}
		return nil, fmt.Errorf("putting program intent: %w", err)
	}
	h.enqueueRulerPassHint(ctx, req.Site)
	return PutProgramIntent204Response{}, nil
}

// DeleteProgramIntent は意図を消して「意見なし」に戻す
// (DELETE /api/sites/{site}/programs/{programId}/intent、issue #29)。
//
// 取消（「録るな」の明示的な主張）ではない。取消は PUT .../intent {action: skip}。
// program_overrides には触れない（別軸）。行が無くても冪等に 204（DeleteRecording と同じ規律）。
func (h *Server) DeleteProgramIntent(ctx context.Context, req DeleteProgramIntentRequestObject) (DeleteProgramIntentResponseObject, error) {
	if !h.knownSite(req.Site) {
		return DeleteProgramIntent400JSONResponse{Error: "unknown site"}, nil
	}
	if err := h.reservationStore.DeleteIntent(ctx, req.Site, req.ProgramId); err != nil {
		return nil, fmt.Errorf("deleting program intent: %w", err)
	}
	h.enqueueRulerPassHint(ctx, req.Site)
	return DeleteProgramIntent204Response{}, nil
}
