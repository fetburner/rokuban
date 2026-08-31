package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/worker"
)

const encodeJobListPageSize = 10_000

type encodeQueueSnapshot struct {
	queuedRecordingIDs  []int64
	runningRecordingIDs []int64
}

// loadEncodeQueue は River の公開 API から active な encode ジョブを全件取得する。
func (h *Server) loadEncodeQueue(ctx context.Context) (encodeQueueSnapshot, error) {
	if h.river == nil {
		return encodeQueueSnapshot{}, fmt.Errorf("river client is not configured")
	}

	params := river.NewJobListParams().
		Kinds((worker.EncodeJobArgs{}).Kind()).
		States(
			rivertype.JobStateAvailable,
			rivertype.JobStatePending,
			rivertype.JobStateScheduled,
			rivertype.JobStateRetryable,
			rivertype.JobStateRunning,
		).
		First(encodeJobListPageSize)

	var snapshot encodeQueueSnapshot
	for {
		result, err := h.river.JobList(ctx, params)
		if err != nil {
			return encodeQueueSnapshot{}, fmt.Errorf("listing encode jobs: %w", err)
		}
		for _, job := range result.Jobs {
			var args worker.EncodeJobArgs
			if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
				return encodeQueueSnapshot{}, fmt.Errorf("decoding encode job %d args: %w", job.ID, err)
			}
			switch job.State {
			case rivertype.JobStateAvailable, rivertype.JobStatePending,
				rivertype.JobStateScheduled, rivertype.JobStateRetryable:
				snapshot.queuedRecordingIDs = append(snapshot.queuedRecordingIDs, args.RecordingID)
			case rivertype.JobStateRunning:
				snapshot.runningRecordingIDs = append(snapshot.runningRecordingIDs, args.RecordingID)
			default:
				return encodeQueueSnapshot{}, fmt.Errorf("unexpected active encode job state %q", job.State)
			}
		}
		if len(result.Jobs) < encodeJobListPageSize {
			return snapshot, nil
		}
		if result.LastCursor == nil {
			return encodeQueueSnapshot{}, fmt.Errorf("listing encode jobs: full page has no cursor")
		}
		params = params.After(result.LastCursor)
	}
}

// GetEncodeQueue は active なエンコードジョブを待機中と実行中に分けて返す。
func (h *Server) GetEncodeQueue(ctx context.Context, _ GetEncodeQueueRequestObject) (GetEncodeQueueResponseObject, error) {
	snapshot, err := h.loadEncodeQueue(ctx)
	if err != nil {
		return nil, err
	}
	return GetEncodeQueue200JSONResponse{
		Queued:  int64(len(snapshot.queuedRecordingIDs)),
		Running: int64(len(snapshot.runningRecordingIDs)),
	}, nil
}
