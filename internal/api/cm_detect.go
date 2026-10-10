package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

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
		durationMs, err := q.GetCMRecordingDuration(ctx, req.Id)
		if err != nil {
			return nil, fmt.Errorf("getting recording %d duration for CM detection: %w", req.Id, err)
		}
		if _, err := h.river.InsertTx(ctx, tx, jobs.CMDetectJobArgs{
			RecordingID: req.Id, RecordingDurationMs: durationMs,
		}, nil); err != nil {
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
		if row.PreviewPng != nil {
			preview := row.PreviewPng
			item.PreviewPng = &preview
		}
		if row.LastFailureStage != "" {
			stage := row.LastFailureStage
			item.LastFailureStage = &stage
		}
		// 枠は x が非 NULL のときだけある（主キーが同じなので a の列は揃って出る）。
		if row.X != nil {
			item.LogoArea = &CMLogoArea{
				X:           int(*row.X),
				Y:           int(*row.Y),
				W:           int(*row.W),
				H:           int(*row.H),
				CodedWidth:  int(*row.CodedWidth),
				CodedHeight: int(*row.CodedHeight),
				UpdatedAt:   *row.AreaUpdatedAt,
			}
		}
		if row.CandidateState != nil {
			candidate := &CMLogoCandidate{
				State:       CMLogoCandidateState(*row.CandidateState),
				X:           int(*row.CandidateX),
				Y:           int(*row.CandidateY),
				W:           int(*row.CandidateW),
				H:           int(*row.CandidateH),
				CodedWidth:  int(*row.CandidateCodedWidth),
				CodedHeight: int(*row.CandidateCodedHeight),
				AttemptedAt: *row.CandidateAttemptedAt,
			}
			if row.CandidateStage != nil {
				stage := CMLogoCandidateStage(*row.CandidateStage)
				candidate.Stage = &stage
			}
			if row.CandidateError != nil {
				errorMessage := *row.CandidateError
				candidate.Error = &errorMessage
			}
			if row.CandidatePreviewPng != nil {
				preview := row.CandidatePreviewPng
				candidate.PreviewPng = &preview
			}
			if row.CandidateRecordingID != nil {
				recordingID := *row.CandidateRecordingID
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
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning logo area save for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	recording, err := q.GetRecordingByID(ctx, body.RecordingId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PutCMLogoArea400JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("loading analysis recording %d: %w", body.RecordingId, err)
	}
	if recording.NetworkID != int32(req.NetworkId) || recording.ServiceID != int32(req.ServiceId) {
		return PutCMLogoArea400JSONResponse{Error: "recording does not belong to this station"}, nil
	}
	activeOriginal, err := q.HasCMRecordingOriginal(ctx, sqlcgen.HasCMRecordingOriginalParams{
		RecordingID: body.RecordingId,
		NetworkID:   int32(req.NetworkId),
		ServiceID:   int32(req.ServiceId),
	})
	if err != nil {
		return nil, fmt.Errorf("checking analysis recording %d: %w", body.RecordingId, err)
	}
	if !activeOriginal {
		return PutCMLogoArea409JSONResponse{Error: "active original media asset required for logo analysis"}, nil
	}
	// 検出ジョブの学習結果の保存と直列化する（worker の persistNewStationLogo と同じ鍵）。
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}); err != nil {
		return nil, fmt.Errorf("locking logo state for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if err := q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID:   int32(req.NetworkId),
		ServiceID:   int32(req.ServiceId),
		X:           int32(body.X),
		Y:           int32(body.Y),
		W:           int32(body.W),
		H:           int32(body.H),
		CodedWidth:  int32(body.CodedWidth),
		CodedHeight: int32(body.CodedHeight),
	}); err != nil {
		return nil, fmt.Errorf("saving logo area for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{
		NetworkID: int32(req.NetworkId),
		ServiceID: int32(req.ServiceId),
	}); err != nil {
		return nil, fmt.Errorf("clearing CM logo candidate for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{
		NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
	})
	if err != nil {
		return nil, fmt.Errorf("loading saved logo area for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if err := insertCMLogoCandidate(ctx, tx, h.river, jobs.CMLogoCandidateJobArgs{
		NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
		RecordingID: body.RecordingId, AreaUpdatedAt: area.UpdatedAt,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing logo area for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	return PutCMLogoArea204Response{}, nil
}

// DeleteCMLogoArea returns a station to the automatic logo-area search.
func (h *Server) DeleteCMLogoArea(ctx context.Context, req DeleteCMLogoAreaRequestObject) (DeleteCMLogoAreaResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning logo area deletion for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{
		NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
	}); err != nil {
		return nil, fmt.Errorf("locking logo state for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if err := q.DeleteCMAdoptAttemptsForStation(ctx, sqlcgen.DeleteCMAdoptAttemptsForStationParams{
		NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
	}); err != nil {
		return nil, fmt.Errorf("clearing adoption waits for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if _, err := q.DeleteCMLogoArea(ctx, sqlcgen.DeleteCMLogoAreaParams{
		NetworkID: int32(req.NetworkId),
		ServiceID: int32(req.ServiceId),
	}); err != nil {
		return nil, fmt.Errorf("deleting logo area for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if err := insertCMDetectReconcile(ctx, tx, h.river); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing logo area deletion for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	return DeleteCMLogoArea204Response{}, nil
}

// DeleteCMLogo forgets a station logo; the next eligible detection will learn it again.
func (h *Server) DeleteCMLogo(ctx context.Context, req DeleteCMLogoRequestObject) (DeleteCMLogoResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning CM logo deletion for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{
		NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
	}); err != nil {
		return nil, fmt.Errorf("locking logo state for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if _, err := q.DeleteCMLogo(ctx, sqlcgen.DeleteCMLogoParams{
		NetworkID: int32(req.NetworkId),
		ServiceID: int32(req.ServiceId),
	}); err != nil {
		return nil, fmt.Errorf("deleting CM logo for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if area, areaErr := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{
		NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
	}); areaErr == nil {
		if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{
			NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
		}); err != nil {
			return nil, fmt.Errorf("clearing CM logo candidate for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
		}
		recording, recordingErr := q.GetCMLogoAnalysisRecording(ctx, sqlcgen.GetCMLogoAnalysisRecordingParams{
			NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
		})
		if recordingErr == nil {
			if err := insertCMLogoCandidate(ctx, tx, h.river, jobs.CMLogoCandidateJobArgs{
				NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
				RecordingID: recording.ID, AreaUpdatedAt: area.UpdatedAt,
			}); err != nil {
				return nil, err
			}
		} else if !errors.Is(recordingErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("finding analysis recording for network %d service %d: %w", req.NetworkId, req.ServiceId, recordingErr)
		} else if err := insertCMDetectReconcile(ctx, tx, h.river); err != nil {
			return nil, err
		}
	} else if !errors.Is(areaErr, pgx.ErrNoRows) {
		return nil, fmt.Errorf("loading logo area for network %d service %d: %w", req.NetworkId, req.ServiceId, areaErr)
	} else if err := insertCMDetectReconcile(ctx, tx, h.river); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing CM logo deletion for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	return DeleteCMLogo204Response{}, nil
}

// DeleteCMLogoCandidate discards the current candidate. The unchanged area
// remains an intent, so the reconcile pass can request a fresh candidate later.
func (h *Server) DeleteCMLogoCandidate(ctx context.Context, req DeleteCMLogoCandidateRequestObject) (DeleteCMLogoCandidateResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning CM logo candidate deletion for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}); err != nil {
		return nil, fmt.Errorf("locking candidate for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}); err != nil {
		return nil, fmt.Errorf("deleting CM logo candidate for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if err := insertCMDetectReconcile(ctx, tx, h.river); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing candidate deletion for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	return DeleteCMLogoCandidate204Response{}, nil
}

// AdoptCMLogoCandidate atomically promotes a ready candidate to the station logo.
func (h *Server) AdoptCMLogoCandidate(ctx context.Context, req AdoptCMLogoCandidateRequestObject) (AdoptCMLogoCandidateResponseObject, error) {
	redetect := true
	if req.Body != nil && req.Body.Redetect != nil {
		redetect = *req.Body.Redetect
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning CM logo candidate adoption for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}); err != nil {
		return nil, fmt.Errorf("locking candidate for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	candidate, err := q.GetCMLogoCandidate(ctx, sqlcgen.GetCMLogoCandidateParams{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)})
	if errors.Is(err, pgx.ErrNoRows) {
		return AdoptCMLogoCandidate409JSONResponse{Error: "CM logo candidate is not available"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading CM logo candidate: %w", err)
	}
	if candidate.State != "ready" {
		return AdoptCMLogoCandidate409JSONResponse{Error: fmt.Sprintf("CM logo candidate is %s, not ready", candidate.State)}, nil
	}
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)})
	if errors.Is(err, pgx.ErrNoRows) {
		return AdoptCMLogoCandidate409JSONResponse{Error: "taught logo area no longer exists"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading taught logo area: %w", err)
	}
	if !candidate.ObservedAreaUpdatedAt.Equal(area.UpdatedAt) {
		return AdoptCMLogoCandidate409JSONResponse{Error: "taught logo area changed after candidate analysis"}, nil
	}
	if candidate.RecordingID == nil || len(candidate.Lgd) == 0 {
		return AdoptCMLogoCandidate409JSONResponse{Error: "CM logo candidate has no adoptable recording or logo"}, nil
	}
	if err := q.UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
		Lgd: candidate.Lgd, PreviewPng: candidate.PreviewPng,
		LearnedFrom: candidate.RecordingID,
		CodedWidth:  candidate.CodedWidth, CodedHeight: candidate.CodedHeight,
	}); err != nil {
		return nil, fmt.Errorf("adopting CM logo candidate: %w", err)
	}
	if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId)}); err != nil {
		return nil, fmt.Errorf("deleting adopted CM logo candidate: %w", err)
	}
	if redetect {
		if err := q.DeleteCMDetectionsForStationWithActiveOriginal(ctx, sqlcgen.DeleteCMDetectionsForStationWithActiveOriginalParams{
			NetworkID: int32(req.NetworkId), ServiceID: int32(req.ServiceId),
		}); err != nil {
			return nil, fmt.Errorf("clearing active CM detections after adoption: %w", err)
		}
	}
	if err := insertCMDetectReconcile(ctx, tx, h.river); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing CM logo candidate adoption: %w", err)
	}
	return AdoptCMLogoCandidate204Response{}, nil
}

func insertCMLogoCandidate(ctx context.Context, tx pgx.Tx, riverClient *river.Client[pgx.Tx], args jobs.CMLogoCandidateJobArgs) error {
	if riverClient == nil {
		return nil
	}
	durationMs, err := sqlcgen.New(tx).GetCMRecordingDuration(ctx, args.RecordingID)
	if err != nil {
		return fmt.Errorf("getting recording %d duration for CM logo candidate: %w", args.RecordingID, err)
	}
	args.RecordingDurationMs = durationMs
	if _, err := riverClient.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("inserting CM logo candidate analysis: %w", err)
	}
	return nil
}

func insertCMDetectReconcile(ctx context.Context, tx pgx.Tx, riverClient *river.Client[pgx.Tx]) error {
	if riverClient == nil {
		return nil
	}
	if _, err := riverClient.InsertTx(ctx, tx, jobs.CMDetectReconcileArgs{}, nil); err != nil {
		return fmt.Errorf("inserting CM detection reconcile: %w", err)
	}
	return nil
}
