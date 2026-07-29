package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// GetProgramOverlaps は指定番組の放送時間帯と重なる既存予約の件数と内訳を返す
// (GET /api/programs/{programId}/overlaps)。
//
// issue #21 の「案 C」: チューナー射影（tuner_sync、docs/data.md §6.5）は使わず、
// 自分の予約だけを数えた**事実**を返す。件数が 0 より大きくても「録画できない」
// とは限らず（同一物理チャンネルは 1 本のチューナーで賄える）、0 でも他サイト・
// 見えない消費者（並走 EPGStation・ライブ視聴・EPG 収集）は考慮されていない。
// 勝敗・容量超過の判定は M2-10 の領分であり、ここでは一切行わない。
//
// 重なりの判定は半開区間（`a.start < b.end AND b.start < a.end`）で行う。番組表は
// 連続しているため、閉区間で判定すると前後の番組がすべて重なりとして数えられて
// しまう。
func (h *Server) GetProgramOverlaps(ctx context.Context, req GetProgramOverlapsRequestObject) (GetProgramOverlapsResponseObject, error) {
	q := sqlcgen.New(h.pool)

	// 番組の放送時間は EPG プロジェクションから引く。射影に無ければ判定材料が
	// 無いので 404（CreateReservation の GetProgramChannelIdentity と同じ姿勢）。
	program, err := q.GetEpgProgram(ctx, sqlcgen.GetEpgProgramParams{
		Site:      defaultSite,
		ProgramID: req.ProgramId,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetProgramOverlaps404JSONResponse{Error: "program not found in EPG projection"}, nil
		}
		return nil, fmt.Errorf("getting program broadcast window: %w", err)
	}

	rows, err := q.ListOverlappingReservations(ctx, sqlcgen.ListOverlappingReservationsParams{
		Site:            defaultSite,
		TargetProgramID: req.ProgramId,
		WindowStart:     program.StartAt,
		WindowEnd:       program.EndAt,
	})
	if err != nil {
		return nil, fmt.Errorf("listing overlapping reservations: %w", err)
	}

	overlaps := make([]OverlappingReservation, 0, len(rows))
	for _, row := range rows {
		// state <> 'orphaned' と自分自身の除外は SQL 側で済ませてあるが、
		// effective.skip（program_overrides / program_intents.action='skip' の
		// 反映）は jsonb のマージが要るため Go 側で db.EffectiveOptions を通す
		// （不透明な overrides を SQL で読まない、という既存の規律に合わせる）。
		eff, err := db.EffectiveOptions(row.Reservation.Base, row.Overrides, row.IntentAction)
		if err != nil {
			return nil, fmt.Errorf("resolving effective options for reservation %d: %w", row.Reservation.ID, err)
		}
		if eff.IsSkipped() {
			continue
		}
		overlaps = append(overlaps, OverlappingReservation{
			Id:         row.Reservation.ID,
			ProgramId:  row.Reservation.ProgramID,
			Title:      row.Reservation.Title,
			StartAt:    row.Reservation.ProgramStartAt,
			DurationMs: row.Reservation.ProgramDurationMs,
		})
	}

	return GetProgramOverlaps200JSONResponse{
		Count:        len(overlaps),
		Reservations: overlaps,
	}, nil
}
