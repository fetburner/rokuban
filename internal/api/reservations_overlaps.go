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
// issue #21 の「案 C」: チューナー射影（tuner_sync）は使わず、自分の予約だけを数えた**事実**を返す。
// 件数が 0 より大きくても「録画できない」とは限らず（同一物理チャンネルは 1 本のチューナーで
// 賄える）、0 でも他サイト・見えない消費者（並走 EPGStation・ライブ視聴・EPG 収集）は考慮されていない。
// 勝敗・容量超過の判定は容量判定の領分であり、ここでは行わない。
//
// 重なりの判定は半開区間（`a.start < b.end AND b.start < a.end`）で行う。番組表は連続しているため、
// 閉区間で判定すると前後の番組がすべて重なりとして数えられてしまう。
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
		// 欠測除外（issue #98）と自分自身の除外は store が済ませてある。effective.skip
		// （program_overrides / program_intents.action='skip' の反映）は jsonb のマージが要るので、
		// Go 側で reservation.EffectiveOptions を通す（不透明な overrides を SQL で読まない規律）。
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
