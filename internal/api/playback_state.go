package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/store"
)

const continueWatchingLimit = 6

// ListContinueWatching は再生位置が残る録画を、最後に見た位置の更新順で返す。
func (h *Server) ListContinueWatching(ctx context.Context, _ ListContinueWatchingRequestObject) (ListContinueWatchingResponseObject, error) {
	if h.recordingQuery == nil {
		return nil, fmt.Errorf("recording query is required")
	}
	rows, err := h.recordingQuery.List(ctx, store.RecordingFilter{
		SortDesc:         true,
		ContinueWatching: true,
		Limit:            continueWatchingLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing recordings to resume: %w", err)
	}
	recordings, err := recordingQueryRowsToAPI(rows, false, h.profileSets())
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
	if err := h.playbackStore.SavePosition(ctx, req.Id, req.Body.PositionMs); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PutRecordingPlaybackPosition404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("saving playback position for recording %d: %w", req.Id, err)
	}
	return PutRecordingPlaybackPosition204Response{}, nil
}

// DeleteRecordingPlaybackPosition は位置の行を冪等に削除する。
func (h *Server) DeleteRecordingPlaybackPosition(ctx context.Context, req DeleteRecordingPlaybackPositionRequestObject) (DeleteRecordingPlaybackPositionResponseObject, error) {
	if err := h.playbackStore.ClearPosition(ctx, req.Id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeleteRecordingPlaybackPosition404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("deleting playback position for recording %d: %w", req.Id, err)
	}
	return DeleteRecordingPlaybackPosition204Response{}, nil
}

// PutRecordingWatched は印を押した行だけに INSERT し、その行の位置を同じ tx で消す。
func (h *Server) PutRecordingWatched(ctx context.Context, req PutRecordingWatchedRequestObject) (PutRecordingWatchedResponseObject, error) {
	if err := h.playbackStore.MarkWatched(ctx, req.Id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PutRecordingWatched404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("saving watched marker for recording %d: %w", req.Id, err)
	}
	return PutRecordingWatched204Response{}, nil
}

// DeleteRecordingWatched は押した行だけでなく、放送イベント全体の印を消す。
func (h *Server) DeleteRecordingWatched(ctx context.Context, req DeleteRecordingWatchedRequestObject) (DeleteRecordingWatchedResponseObject, error) {
	if err := h.playbackStore.ClearWatchedForEvent(ctx, req.Id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeleteRecordingWatched404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("clearing watched markers for recording event %d: %w", req.Id, err)
	}
	return DeleteRecordingWatched204Response{}, nil
}
