package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobs"
)

const encodeJobListPageSize = 10_000

type encodeJobRecordingIDs struct {
	queued  []int64
	running []int64
}

// loadEncodeJobRecordingIDs は録画一覧の encodeState 絞り込み用に、River の active
// encode job から録画 ID を集める。待機列の件数は loadEncodeQueue でドメイン表から導出する。
func (h *Server) loadEncodeJobRecordingIDs(ctx context.Context) (encodeJobRecordingIDs, error) {
	if h.river == nil {
		return encodeJobRecordingIDs{}, fmt.Errorf("river client is not configured")
	}

	params := river.NewJobListParams().
		Kinds((jobs.EncodeJobArgs{}).Kind()).
		States(
			rivertype.JobStateAvailable,
			rivertype.JobStatePending,
			rivertype.JobStateScheduled,
			rivertype.JobStateRetryable,
			rivertype.JobStateRunning,
		).
		First(encodeJobListPageSize)

	var ids encodeJobRecordingIDs
	for {
		result, err := h.river.JobList(ctx, params)
		if err != nil {
			return encodeJobRecordingIDs{}, fmt.Errorf("listing encode jobs: %w", err)
		}
		for _, job := range result.Jobs {
			var args jobs.EncodeJobArgs
			if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
				return encodeJobRecordingIDs{}, fmt.Errorf("decoding encode job %d args: %w", job.ID, err)
			}
			switch job.State {
			case rivertype.JobStateAvailable, rivertype.JobStatePending,
				rivertype.JobStateScheduled, rivertype.JobStateRetryable:
				ids.queued = append(ids.queued, args.RecordingID)
			case rivertype.JobStateRunning:
				ids.running = append(ids.running, args.RecordingID)
			default:
				return encodeJobRecordingIDs{}, fmt.Errorf("unexpected active encode job state %q", job.State)
			}
		}
		if len(result.Jobs) < encodeJobListPageSize {
			return ids, nil
		}
		if result.LastCursor == nil {
			return encodeJobRecordingIDs{}, fmt.Errorf("listing encode jobs: full page has no cursor")
		}
		params = params.After(result.LastCursor)
	}
}
