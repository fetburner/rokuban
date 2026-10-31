package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
)

// cmDetectDisabledMessage は cm_detect.enabled=false のデプロイで検出を要求されたときの 409 本文。
const cmDetectDisabledMessage = "CM detection is disabled: set cm_detect.enabled: true and run the Dockerfile.full image"

// RetryRecordingCMDetection clears a previous attempt so an active recording can be analyzed again.
func (h *Server) RetryRecordingCMDetection(ctx context.Context, req RetryRecordingCMDetectionRequestObject) (RetryRecordingCMDetectionResponseObject, error) {
	if !h.capabilities.CmDetect {
		return RetryRecordingCMDetection409JSONResponse{Error: cmDetectDisabledMessage}, nil
	}
	retry, err := h.cmDetectionStore.RetryDetection(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return RetryRecordingCMDetection404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("retrying CM detection for recording %d: %w", req.Id, err)
	}
	if !retry.ActiveOriginal {
		return RetryRecordingCMDetection409JSONResponse{Error: "active original media asset required to retry CM detection"}, nil
	}
	if retry.Desired && h.enqueuer != nil {
		// 置き場は原本のコピーがある置き場。引けなければヒントは積まず、reconcile が拾う。
		if storeName, ok := h.originalStoreOf(ctx, req.Id); ok {
			if _, err := h.enqueuer.Enqueue(ctx, jobs.CMDetectJobArgs{
				Store: storeName, RecordingID: req.Id, RecordingDurationMs: retry.RecordingDurationMs,
			}); err != nil {
				slog.ErrorContext(ctx, "api: failed to enqueue CM detection retry hint", "recording_id", req.Id, "err", err)
			}
		}
	}
	return RetryRecordingCMDetection204Response{}, nil
}

// ListCMLogos returns learned logos and failed detections grouped by broadcast service.
func (h *Server) ListCMLogos(ctx context.Context, _ ListCMLogosRequestObject) (ListCMLogosResponseObject, error) {
	rows, err := h.cmStationStore.ListCMLogoStates(ctx)
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
			NetworkId:         int(row.NetworkID),
			ServiceId:         int(row.ServiceID),
			ServiceName:       row.ServiceName,
			Site:              row.Site,
			State:             state,
			RecordingCount:    row.RecordingCount,
			FailedCount:       row.FailedCount,
			PendingCount:      row.PendingCount,
			DetectedCount:     row.DetectedCount,
			RedetectableCount: row.RedetectableCount,
			FrameRecordingId:  row.FrameRecordingID,
			LearnedAt:         utcTimePtr(row.LearnedAt),
		}
		if row.LearnedCodedWidth != nil {
			codedWidth := int(*row.LearnedCodedWidth)
			item.CodedWidth = &codedWidth
		}
		if row.LearnedCodedHeight != nil {
			codedHeight := int(*row.LearnedCodedHeight)
			item.CodedHeight = &codedHeight
		}
		if row.PreviewPNG != nil {
			preview := row.PreviewPNG
			item.PreviewPng = &preview
		}
		if row.LastFailureStage != "" {
			stage := row.LastFailureStage
			item.LastFailureStage = &stage
		}
		if row.Area != nil {
			item.LogoArea = &CMLogoArea{
				X: int(row.Area.X), Y: int(row.Area.Y), W: int(row.Area.W), H: int(row.Area.H),
				CodedWidth: int(row.Area.CodedWidth), CodedHeight: int(row.Area.CodedHeight),
				UpdatedAt: row.Area.UpdatedAt,
			}
		}
		if row.Candidate != nil {
			candidate := &CMLogoCandidate{
				State: CMLogoCandidateState(row.Candidate.State),
				X:     int(row.Candidate.X), Y: int(row.Candidate.Y), W: int(row.Candidate.W), H: int(row.Candidate.H),
				CodedWidth: int(row.Candidate.CodedWidth), CodedHeight: int(row.Candidate.CodedHeight),
				AttemptedAt: row.Candidate.AttemptedAt,
			}
			if row.Candidate.Stage != nil {
				stage := CMLogoCandidateStage(*row.Candidate.Stage)
				candidate.Stage = &stage
			}
			if row.Candidate.Error != nil {
				errorMessage := *row.Candidate.Error
				candidate.Error = &errorMessage
			}
			if row.Candidate.PreviewPNG != nil {
				preview := row.Candidate.PreviewPNG
				candidate.PreviewPng = &preview
			}
			if row.Candidate.RecordingID != nil {
				recordingID := *row.Candidate.RecordingID
				candidate.RecordingId = &recordingID
			}
			item.Candidate = candidate
		}
		items = append(items, item)
	}
	return ListCMLogos200JSONResponse(items), nil
}

// PutCMLogoArea saves the taught logo area and queues asynchronous candidate analysis.
// The existing station logo remains usable until the candidate is adopted.
func (h *Server) PutCMLogoArea(ctx context.Context, req PutCMLogoAreaRequestObject) (PutCMLogoAreaResponseObject, error) {
	if req.Body == nil {
		return PutCMLogoArea400JSONResponse{Error: "logo area is required"}, nil
	}
	body := *req.Body
	// 枠は記録上の画素の矩形。表現できないものは DB の CHECK でも止まるが、
	// 400 で理由を返せるようにここでも判定する（不変条件 10）。
	if body.RecordingId <= 0 || body.X < 0 || body.Y < 0 || body.W <= 0 || body.H <= 0 ||
		body.CodedWidth <= 0 || body.CodedHeight <= 0 ||
		body.X+body.W > body.CodedWidth || body.Y+body.H > body.CodedHeight {
		return PutCMLogoArea400JSONResponse{Error: "logo area must be a rectangle inside the recorded frame"}, nil
	}
	station := store.CMStationRef{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}
	job, err := h.cmStationStore.SaveLogoArea(ctx, station, store.CMLogoAreaInput{
		RecordingID: body.RecordingId,
		X:           int32(body.X), Y: int32(body.Y), W: int32(body.W), H: int32(body.H),
		CodedWidth: int32(body.CodedWidth), CodedHeight: int32(body.CodedHeight),
	})
	if errors.Is(err, store.ErrCMLogoRecordingNotFound) {
		return PutCMLogoArea400JSONResponse{Error: "recording not found"}, nil
	}
	if errors.Is(err, store.ErrCMLogoRecordingWrongStation) {
		return PutCMLogoArea400JSONResponse{Error: "recording does not belong to this station"}, nil
	}
	if errors.Is(err, store.ErrCMLogoRecordingOriginalUnavailable) {
		return PutCMLogoArea409JSONResponse{Error: "active original media asset required for logo analysis"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("saving logo area for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	h.enqueueCMLogoCandidateHint(ctx, job)
	return PutCMLogoArea204Response{}, nil
}

// DeleteCMLogoArea returns a station to the automatic logo-area search.
func (h *Server) DeleteCMLogoArea(ctx context.Context, req DeleteCMLogoAreaRequestObject) (DeleteCMLogoAreaResponseObject, error) {
	station := store.CMStationRef{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}
	if err := h.cmStationStore.DeleteLogoArea(ctx, station); err != nil {
		return nil, fmt.Errorf("deleting logo area for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	h.enqueueCMDetectReconcileHint(ctx)
	return DeleteCMLogoArea204Response{}, nil
}

// DeleteCMLogo forgets a station logo; the next eligible detection will learn it again.
func (h *Server) DeleteCMLogo(ctx context.Context, req DeleteCMLogoRequestObject) (DeleteCMLogoResponseObject, error) {
	station := store.CMStationRef{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}
	result, err := h.cmStationStore.DeleteLogo(ctx, station)
	if err != nil {
		return nil, fmt.Errorf("deleting CM logo for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if result.CandidateJob != nil {
		h.enqueueCMLogoCandidateHint(ctx, *result.CandidateJob)
	} else {
		h.enqueueCMDetectReconcileHint(ctx)
	}
	return DeleteCMLogo204Response{}, nil
}

// DeleteCMLogoCandidate discards the current candidate. The unchanged area
// remains an intent, so the reconcile pass can request a fresh candidate later.
func (h *Server) DeleteCMLogoCandidate(ctx context.Context, req DeleteCMLogoCandidateRequestObject) (DeleteCMLogoCandidateResponseObject, error) {
	station := store.CMStationRef{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}
	if err := h.cmStationStore.DeleteLogoCandidate(ctx, station); err != nil {
		return nil, fmt.Errorf("deleting CM logo candidate for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	h.enqueueCMDetectReconcileHint(ctx)
	return DeleteCMLogoCandidate204Response{}, nil
}

// AdoptCMLogoCandidate atomically promotes a ready candidate to the station logo.
func (h *Server) AdoptCMLogoCandidate(ctx context.Context, req AdoptCMLogoCandidateRequestObject) (AdoptCMLogoCandidateResponseObject, error) {
	redetect := true
	if req.Body != nil && req.Body.Redetect != nil {
		redetect = *req.Body.Redetect
	}
	station := store.CMStationRef{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}
	err := h.cmStationStore.AdoptLogoCandidate(ctx, station, redetect)
	if err != nil {
		var adoptionErr *store.CMLogoAdoptionError
		if errors.As(err, &adoptionErr) {
			switch adoptionErr.Reason {
			case store.CMLogoAdoptionCandidateUnavailable:
				return AdoptCMLogoCandidate409JSONResponse{Error: "CM logo candidate is not available"}, nil
			case store.CMLogoAdoptionCandidateNotReady:
				return AdoptCMLogoCandidate409JSONResponse{Error: fmt.Sprintf("CM logo candidate is %s, not ready", adoptionErr.State)}, nil
			case store.CMLogoAdoptionAreaMissing:
				return AdoptCMLogoCandidate409JSONResponse{Error: "taught logo area no longer exists"}, nil
			case store.CMLogoAdoptionAreaChanged:
				return AdoptCMLogoCandidate409JSONResponse{Error: "taught logo area changed after candidate analysis"}, nil
			case store.CMLogoAdoptionCandidateUnusable:
				return AdoptCMLogoCandidate409JSONResponse{Error: "CM logo candidate has no adoptable recording or logo"}, nil
			}
		}
		return nil, fmt.Errorf("adopting CM logo candidate for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	h.enqueueCMDetectReconcileHint(ctx)
	return AdoptCMLogoCandidate204Response{}, nil
}

func (h *Server) enqueueCMLogoCandidateHint(ctx context.Context, job store.CMLogoCandidateJob) {
	if h.enqueuer == nil {
		return
	}
	storeName, ok := h.originalStoreOf(ctx, job.RecordingID)
	if !ok {
		return
	}
	args := jobs.CMLogoCandidateJobArgs{
		Store:     storeName,
		NetworkID: job.Station.NetworkID, ServiceID: job.Station.ServiceID,
		RecordingID: job.RecordingID, RecordingDurationMs: job.RecordingDurationMs,
		AreaUpdatedAt: job.AreaUpdatedAt,
	}
	if _, err := h.enqueuer.Enqueue(ctx, args); err != nil {
		slog.ErrorContext(ctx, "api: failed to enqueue CM logo candidate hint",
			"network_id", job.Station.NetworkID, "service_id", job.Station.ServiceID,
			"recording_id", job.RecordingID, "err", err)
	}
}

func (h *Server) enqueueCMDetectReconcileHint(ctx context.Context) {
	if h.enqueuer == nil {
		return
	}
	if _, err := h.enqueuer.Enqueue(ctx, jobs.CMDetectReconcileArgs{}); err != nil {
		slog.ErrorContext(ctx, "api: failed to enqueue CM detection reconcile hint", "err", err)
	}
}

// originalStoreOf は録画の active な原本のコピーがある置き場を返す。引けなければ false を返す
// （URL を省略する、ヒントを積まない。真実は reconcile が引き直す）。
func (h *Server) originalStoreOf(ctx context.Context, recordingID int64) (string, bool) {
	if h.originalLocator == nil {
		return "", false
	}
	storeName, err := h.originalLocator.OriginalStore(ctx, recordingID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "api: locating the original of a recording", "recording_id", recordingID, "err", err)
		}
		return "", false
	}
	return storeName, true
}
