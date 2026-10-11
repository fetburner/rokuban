package api

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

type encodeQueueSnapshot struct {
	queued  int64
	running int64
}

// loadEncodeQueue は desired と encoded の差分、および実行中の試行から待機列を導出する
// （TestGetEncodeQueueSummaryFromDomainState）。
func (h *Server) loadEncodeQueue(ctx context.Context) (encodeQueueSnapshot, error) {
	counts, err := sqlcgen.New(h.pool).CountEncodeQueue(ctx, sqlcgen.CountEncodeQueueParams{
		KnownProfiles: h.queueEncodeProfileNames,
		CutProfiles:   h.queueCutProfileNames,
	})
	if err != nil {
		return encodeQueueSnapshot{}, fmt.Errorf("counting encode queue: %w", err)
	}
	return encodeQueueSnapshot{queued: counts.Queued, running: counts.Running}, nil
}

// GetEncodeQueue は現在 desired で未観測のエンコードと実行中の試行数を返す。
func (h *Server) GetEncodeQueue(ctx context.Context, _ GetEncodeQueueRequestObject) (GetEncodeQueueResponseObject, error) {
	snapshot, err := h.loadEncodeQueue(ctx)
	if err != nil {
		return nil, err
	}
	return GetEncodeQueue200JSONResponse{
		Queued:  snapshot.queued,
		Running: snapshot.running,
	}, nil
}
