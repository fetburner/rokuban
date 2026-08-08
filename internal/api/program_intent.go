package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ensureProgramSnapshot は program_snapshots に (site, programId) の行があることを
// 保証する。program_intents / program_overrides はいずれもこの行への FK を持つため、
// その書き込みより先に呼ぶ必要がある（internal/db/queries/program_intents.sql・
// program_overrides.sql のコメント参照）。
//
// EPG 射影にまだあれば最新の事実で upsert する（「射影にある間は更新」。
// docs/schema.md §3）。射影から消えていても既存のスナップショット行があれば
// それをそのまま使う（凍結。既存の予約の overrides を、対象番組が放送済みで
// 射影のローリングウィンドウから外れた後にも編集できるようにするため）。
// 射影にも既存行にも無ければ pgx.ErrNoRows を返す（呼び出し元が 400 に変換する）。
func ensureProgramSnapshot(ctx context.Context, q *sqlcgen.Queries, site string, programID int64) error {
	source, err := q.GetProgramSnapshotSource(ctx, sqlcgen.GetProgramSnapshotSourceParams{
		Site:      site,
		ProgramID: programID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, existingErr := q.GetProgramSnapshot(ctx, sqlcgen.GetProgramSnapshotParams{
				Site: site, ProgramID: programID,
			}); existingErr == nil {
				return nil
			}
		}
		return err
	}
	if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
		Site:        site,
		ProgramID:   programID,
		Title:       source.Title,
		StartAt:     source.StartAt,
		DurationMs:  source.DurationMs,
		NetworkID:   source.NetworkID,
		ServiceID:   source.ServiceID,
		ChannelType: source.ChannelType,
		Channel:     source.Channel,
		EventID:     source.EventID,
		ServiceName: source.ServiceName,
	}); err != nil {
		return fmt.Errorf("upserting program snapshot: %w", err)
	}
	return nil
}

// PutProgramIntent はユーザー意図（record/skip）を (site, programId) を自身の
// キーとして書く (PUT /api/sites/{site}/programs/{programId}/intent、issue #29)。
//
// PutProgramIntent は reservations に触れない。導出の書き手は ruler で、
// 例外はルール削除 API の同期削除 1 本（docs/schema/reservations.md
// §3「書き込み所有権」）。ruler_pass ヒントを入れて実体化を早めるが、
// 同一トランザクションで reservations を作らない
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
		action = db.IntentRecord
	case Skip:
		action = db.IntentSkip
	default:
		return PutProgramIntent400JSONResponse{Error: fmt.Sprintf("invalid action %q", req.Body.Action)}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	if err := ensureProgramSnapshot(ctx, q, req.Site, req.ProgramId); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PutProgramIntent400JSONResponse{Error: "program not found in EPG projection"}, nil
		}
		return nil, fmt.Errorf("ensuring program snapshot: %w", err)
	}

	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site:      req.Site,
		ProgramID: req.ProgramId,
		Action:    action,
	}); err != nil {
		return nil, fmt.Errorf("upserting program intent: %w", err)
	}
	if err := h.insertRulerPassHint(ctx, tx, req.Site); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return PutProgramIntent204Response{}, nil
}

// DeleteProgramIntent は意図を消して「意見なし」に戻す
// (DELETE /api/sites/{site}/programs/{programId}/intent、issue #29)。
//
// 取消（「録るな」の明示的な主張）ではない。取消は
// PUT .../intent {action: skip}。program_overrides には触れない（別軸）。
// 行が無くても冪等に 204（DeleteRecording と同じ規律）。
func (h *Server) DeleteProgramIntent(ctx context.Context, req DeleteProgramIntentRequestObject) (DeleteProgramIntentResponseObject, error) {
	if !h.knownSite(req.Site) {
		return DeleteProgramIntent400JSONResponse{Error: "unknown site"}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	if _, err := q.DeleteProgramIntent(ctx, sqlcgen.DeleteProgramIntentParams{
		Site:      req.Site,
		ProgramID: req.ProgramId,
	}); err != nil {
		return nil, fmt.Errorf("deleting program intent: %w", err)
	}
	if err := h.insertRulerPassHint(ctx, tx, req.Site); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return DeleteProgramIntent204Response{}, nil
}
