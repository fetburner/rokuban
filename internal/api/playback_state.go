package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

const continueWatchingLimit = 6

// ListContinueWatching は再生位置が残る録画を、最後に見た位置の更新順で返す。
func (h *Server) ListContinueWatching(ctx context.Context, _ ListContinueWatchingRequestObject) (ListContinueWatchingResponseObject, error) {
	recordings, err := queryRecordings(ctx, h.pool, recordingsFilter{
		SortDesc:         true,
		ContinueWatching: true,
		Limit:            continueWatchingLimit,
	}, h.profileSets())
	if err != nil {
		return nil, fmt.Errorf("listing recordings to resume: %w", err)
	}
	return ListContinueWatching200JSONResponse(recordings), nil
}

// PutRecordingPlaybackPosition は原本の時間軸上の位置を保存する。
func (h *Server) PutRecordingPlaybackPosition(ctx context.Context, req PutRecordingPlaybackPositionRequestObject) (PutRecordingPlaybackPositionResponseObject, error) {
	if req.Body == nil || req.Body.PositionMs < 2000 {
		return PutRecordingPlaybackPosition400JSONResponse{Error: "positionMs must be at least 2000"}, nil
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning playback position write for recording %d: %w", req.Id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if _, err := q.LockRecordingForPlaybackState(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PutRecordingPlaybackPosition404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("locking recording %d for playback position write: %w", req.Id, err)
	}
	if _, err := q.UpsertRecordingPlaybackPosition(ctx, sqlcgen.UpsertRecordingPlaybackPositionParams{
		RecordingID: req.Id,
		PositionMs:  req.Body.PositionMs,
	}); err != nil {
		return nil, fmt.Errorf("saving playback position for recording %d: %w", req.Id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing playback position for recording %d: %w", req.Id, err)
	}
	return PutRecordingPlaybackPosition204Response{}, nil
}

// DeleteRecordingPlaybackPosition は位置の行を冪等に削除する。
func (h *Server) DeleteRecordingPlaybackPosition(ctx context.Context, req DeleteRecordingPlaybackPositionRequestObject) (DeleteRecordingPlaybackPositionResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning playback position delete for recording %d: %w", req.Id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	exists, err := q.RecordingExistsForPlaybackState(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("checking recording %d for playback position delete: %w", req.Id, err)
	}
	if !exists {
		return DeleteRecordingPlaybackPosition404JSONResponse{Error: "recording not found"}, nil
	}
	if _, err := q.DeleteRecordingPlaybackPosition(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("deleting playback position for recording %d: %w", req.Id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing playback position delete for recording %d: %w", req.Id, err)
	}
	return DeleteRecordingPlaybackPosition204Response{}, nil
}

// PutRecordingWatched は印を押した行だけに INSERT し、その行の位置を同じ tx で消す。
func (h *Server) PutRecordingWatched(ctx context.Context, req PutRecordingWatchedRequestObject) (PutRecordingWatchedResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning watched marker write for recording %d: %w", req.Id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if _, err := q.LockRecordingForPlaybackState(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PutRecordingWatched404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("locking recording %d for watched marker write: %w", req.Id, err)
	}
	rows, err := q.UpsertRecordingWatched(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("saving watched marker for recording %d: %w", req.Id, err)
	}
	if rows == 0 {
		return PutRecordingWatched404JSONResponse{Error: "recording not found"}, nil
	}
	if _, err := q.DeleteRecordingPlaybackPosition(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("clearing playback position for watched recording %d: %w", req.Id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing watched marker for recording %d: %w", req.Id, err)
	}
	return PutRecordingWatched204Response{}, nil
}

// DeleteRecordingWatched は押した行だけでなく、放送イベント全体の印を消す。
func (h *Server) DeleteRecordingWatched(ctx context.Context, req DeleteRecordingWatchedRequestObject) (DeleteRecordingWatchedResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning watched marker delete for recording %d: %w", req.Id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	exists, err := q.RecordingExistsForPlaybackState(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("checking recording %d for watched marker delete: %w", req.Id, err)
	}
	if !exists {
		return DeleteRecordingWatched404JSONResponse{Error: "recording not found"}, nil
	}
	if _, err := q.DeleteRecordingWatchedForEvent(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("clearing watched markers for recording event %d: %w", req.Id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing watched marker delete for recording %d: %w", req.Id, err)
	}
	return DeleteRecordingWatched204Response{}, nil
}
