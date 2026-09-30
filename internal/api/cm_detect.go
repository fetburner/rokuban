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
			NetworkId:        int(row.NetworkID),
			ServiceId:        int(row.ServiceID),
			ServiceName:      row.ServiceName,
			State:            state,
			RecordingCount:   row.RecordingCount,
			FailedCount:      row.FailedCount,
			FrameRecordingId: row.FrameRecordingID,
			LearnedAt:        utcTimePtr(row.LearnedAt),
		}
		if row.PreviewPng != nil {
			preview := row.PreviewPng
			item.PreviewPng = &preview
		}
		if row.LastError != "" {
			message := row.LastError
			item.LastError = &message
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
		items = append(items, item)
	}
	return ListCMLogos200JSONResponse(items), nil
}

// PutCMLogoArea teaches the logo area for a station and forgets its learned logo.
//
// **枠を保存する同じ tx で `cm_logos` を消す。** 学習済みロゴは教えた枠の外で
// 学習されたものなので、残すと次の検出が枠ではなくその古いロゴを使う。
// 消せば次の検出が枠の中で学習し直す（「覚えたロゴを捨てる」と同じ経路）。
func (h *Server) PutCMLogoArea(ctx context.Context, req PutCMLogoAreaRequestObject) (PutCMLogoAreaResponseObject, error) {
	if req.Body == nil {
		return PutCMLogoArea400JSONResponse{Error: "logo area is required"}, nil
	}
	body := *req.Body
	// 枠は記録上の画素の矩形。表現できないものは DB の CHECK でも止まるが、
	// 400 で理由を返せるようにここでも判定する（不変条件 10）。
	if body.X < 0 || body.Y < 0 || body.W <= 0 || body.H <= 0 ||
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
	if _, err := q.DeleteCMLogo(ctx, sqlcgen.DeleteCMLogoParams{
		NetworkID: int32(req.NetworkId),
		ServiceID: int32(req.ServiceId),
	}); err != nil {
		return nil, fmt.Errorf("forgetting CM logo for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing logo area for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	return PutCMLogoArea204Response{}, nil
}

// DeleteCMLogoArea returns a station to the automatic logo-area search.
func (h *Server) DeleteCMLogoArea(ctx context.Context, req DeleteCMLogoAreaRequestObject) (DeleteCMLogoAreaResponseObject, error) {
	if _, err := sqlcgen.New(h.pool).DeleteCMLogoArea(ctx, sqlcgen.DeleteCMLogoAreaParams{
		NetworkID: int32(req.NetworkId),
		ServiceID: int32(req.ServiceId),
	}); err != nil {
		return nil, fmt.Errorf("deleting logo area for network %d service %d: %w", req.NetworkId, req.ServiceId, err)
	}
	return DeleteCMLogoArea204Response{}, nil
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
