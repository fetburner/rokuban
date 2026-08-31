package api

import (
	"context"
	"fmt"
)

// GetEncodeQueue は active なエンコードジョブを待機中と実行中に分けて返す。
func (h *Server) GetEncodeQueue(ctx context.Context, _ GetEncodeQueueRequestObject) (GetEncodeQueueResponseObject, error) {
	const query = `
		SELECT
			count(*) FILTER (WHERE state IN ('available', 'pending', 'scheduled', 'retryable')),
			count(*) FILTER (WHERE state = 'running')
		FROM river_job
		WHERE kind = 'encode'`

	var result EncodeQueueSummary
	if err := h.pool.QueryRow(ctx, query).Scan(&result.Queued, &result.Running); err != nil {
		return nil, fmt.Errorf("counting encode queue: %w", err)
	}
	return GetEncodeQueue200JSONResponse(result), nil
}
