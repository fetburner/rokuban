package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/jobs"
)

// cmDetectDisabledMessage は cm_detect.enabled=false のデプロイで検出を要求されたときの 409 本文。
const cmDetectDisabledMessage = "CM detection is disabled: set cm_detect.enabled: true and run the Dockerfile.full image"

// RetryRecordingCMDetection clears a previous attempt so an active recording can be analyzed again.
func (h *Server) RetryRecordingCMDetection(ctx context.Context, req RetryRecordingCMDetectionRequestObject) (RetryRecordingCMDetectionResponseObject, error) {
	if !h.capabilities.CmDetect {
		return RetryRecordingCMDetection409JSONResponse{Error: cmDetectDisabledMessage}, nil
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning CM detection retry for recording %d: %w", req.Id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	recording, err := q.GetRecordingByID(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RetryRecordingCMDetection404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("loading recording %d: %w", req.Id, err)
	}
	if recording.PurgedAt != nil {
		return RetryRecordingCMDetection404JSONResponse{Error: "recording not found"}, nil
	}
	active, err := q.GetCMRetryOriginal(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("checking active original for recording %d: %w", req.Id, err)
	}
	if !active {
		return RetryRecordingCMDetection409JSONResponse{Error: "active original media asset required to retry CM detection"}, nil
	}
	if err := q.DeleteCMDetection(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("clearing CM detection result for recording %d: %w", req.Id, err)
	}
	if err := q.DeleteCMDetectionAttempt(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("clearing CM detection attempt for recording %d: %w", req.Id, err)
	}
	desired, err := q.IsCMDetectionDesired(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("checking CM detection policy for recording %d: %w", req.Id, err)
	}
	if desired && h.river != nil {
		if _, err := h.river.InsertTx(ctx, tx, jobs.CMDetectJobArgs{RecordingID: req.Id}, nil); err != nil {
			return nil, fmt.Errorf("inserting CM detection retry for recording %d: %w", req.Id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing CM detection retry for recording %d: %w", req.Id, err)
	}
	return RetryRecordingCMDetection204Response{}, nil
}

// ListCMLogos returns learned logos and failed detections grouped by broadcast service.
func (h *Server) ListCMLogos(ctx context.Context, _ ListCMLogosRequestObject) (ListCMLogosResponseObject, error) {
	rows, err := sqlcgen.New(h.pool).ListCMLogoStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing CM logo states: %w", err)
	}
	items := make([]CMLogoState, 0, len(rows))
	for _, row := range rows {
		state := CMLogoStateStateUnlearned
		if row.LearnedAt != nil {
			state = CMLogoStateStateLearned
		}
		if row.FailedCount > 0 {
			state = CMLogoStateStateFailed
		}
		item := CMLogoState{
			NetworkId:      int(row.NetworkID),
			ServiceId:      int(row.ServiceID),
			ServiceName:    row.ServiceName,
			State:          state,
			RecordingCount: row.RecordingCount,
			FailedCount:    row.FailedCount,
			LearnedAt:      utcTimePtr(row.LearnedAt),
		}
		if row.PreviewPng != nil {
			preview := row.PreviewPng
			item.PreviewPng = &preview
		}
		items = append(items, item)
	}
	return ListCMLogos200JSONResponse(items), nil
}

// DeleteCMLogo forgets a station logo; the next eligible detection will learn it again.
func (h *Server) DeleteCMLogo(ctx context.Context, req DeleteCMLogoRequestObject) (DeleteCMLogoResponseObject, error) {
	if _, err := sqlcgen.New(h.pool).DeleteCMLogo(ctx, sqlcgen.DeleteCMLogoParams{
		NetworkID: int32(req.NetworkId),
		ServiceID: int32(req.ServiceId),
	}); err != nil {
		return nil, fmt.Errorf("deleting CM logo for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	return DeleteCMLogo204Response{}, nil
}
