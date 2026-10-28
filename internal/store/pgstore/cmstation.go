package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

var _ store.CMStation = (*Analysis)(nil)

// ListCMLogoStates は局ごとの CM ロゴ状態を返す。
func (s *Analysis) ListCMLogoStates(ctx context.Context) ([]store.CMLogoState, error) {
	rows, err := sqlcgen.New(s.pool).ListCMLogoStates(ctx, mediaStoreNameArg(s.storeName))
	if err != nil {
		return nil, fmt.Errorf("listing CM logo states: %w", err)
	}
	states := make([]store.CMLogoState, 0, len(rows))
	for _, row := range rows {
		state := store.CMLogoState{
			NetworkID:          row.NetworkID,
			ServiceID:          row.ServiceID,
			ServiceName:        row.ServiceName,
			Site:               row.Site,
			RecordingCount:     row.RecordingCount,
			LearnedAt:          row.LearnedAt,
			PreviewPNG:         row.PreviewPng,
			FailedCount:        row.FailedCount,
			PendingCount:       row.PendingCount,
			LastFailureStage:   row.LastFailureStage,
			DetectedCount:      row.DetectedCount,
			RedetectableCount:  row.RedetectableCount,
			LearnedCodedWidth:  row.LearnedCodedWidth,
			LearnedCodedHeight: row.LearnedCodedHeight,
			FrameRecordingID:   row.FrameRecordingID,
		}
		if row.X != nil {
			state.Area = &store.CMLogoArea{
				X: *row.X, Y: *row.Y, W: *row.W, H: *row.H,
				CodedWidth: *row.CodedWidth, CodedHeight: *row.CodedHeight,
				UpdatedAt: *row.AreaUpdatedAt,
			}
		}
		if row.CandidateState != nil {
			state.Candidate = &store.CMLogoCandidateSummary{
				State: *row.CandidateState, Stage: row.CandidateStage, Error: row.CandidateError,
				X: *row.CandidateX, Y: *row.CandidateY, W: *row.CandidateW, H: *row.CandidateH,
				CodedWidth: *row.CandidateCodedWidth, CodedHeight: *row.CandidateCodedHeight,
				RecordingID: row.CandidateRecordingID, PreviewPNG: row.CandidatePreviewPng,
				AttemptedAt: *row.CandidateAttemptedAt,
			}
		}
		states = append(states, state)
	}
	return states, nil
}

// GetLogoArea は局に設定されたロゴ枠を返す。
func (s *Analysis) GetLogoArea(ctx context.Context, station store.CMStationRef) (store.CMLogoArea, error) {
	row, err := sqlcgen.New(s.pool).GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CMLogoArea{}, fmt.Errorf("CM logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, store.ErrNotFound)
		}
		return store.CMLogoArea{}, fmt.Errorf("loading CM logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	return cmLogoAreaFromRow(row), nil
}

// GetLogo は局の学習済みロゴを返す。
func (s *Analysis) GetLogo(ctx context.Context, station store.CMStationRef) (store.CMLogo, error) {
	row, err := sqlcgen.New(s.pool).GetCMLogo(ctx, sqlcgen.GetCMLogoParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CMLogo{}, fmt.Errorf("CM logo for network %d service %d: %w", station.NetworkID, station.ServiceID, store.ErrNotFound)
		}
		return store.CMLogo{}, fmt.Errorf("loading CM logo for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	return store.CMLogo{LGD: row.Lgd, CodedWidth: row.CodedWidth, CodedHeight: row.CodedHeight, LearnedAt: row.LearnedAt}, nil
}

// GetLogoCandidate は局の現在の候補を返す。
func (s *Analysis) GetLogoCandidate(ctx context.Context, station store.CMStationRef) (store.CMLogoCandidate, error) {
	row, err := sqlcgen.New(s.pool).GetCMLogoCandidate(ctx, sqlcgen.GetCMLogoCandidateParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CMLogoCandidate{}, fmt.Errorf("CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, store.ErrNotFound)
		}
		return store.CMLogoCandidate{}, fmt.Errorf("loading CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	return store.CMLogoCandidate{
		State: row.State, Stage: row.Stage, Error: row.Error,
		X: row.X, Y: row.Y, W: row.W, H: row.H,
		CodedWidth: row.CodedWidth, CodedHeight: row.CodedHeight,
		RecordingID: row.RecordingID, ObservedAreaUpdatedAt: row.ObservedAreaUpdatedAt,
		LGD: row.Lgd, PreviewPNG: row.PreviewPng, AttemptedAt: row.AttemptedAt,
	}, nil
}

// GetCandidateRecording は候補解析に必要な録画と原本の状態を返す。
func (s *Analysis) GetCandidateRecording(ctx context.Context, recordingID int64) (store.CMLogoCandidateRecording, error) {
	row, err := sqlcgen.New(s.pool).GetCMDetectionWorkItem(ctx, sqlcgen.GetCMDetectionWorkItemParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.storeName),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CMLogoCandidateRecording{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return store.CMLogoCandidateRecording{}, fmt.Errorf("loading CM logo candidate recording %d: %w", recordingID, err)
	}
	return store.CMLogoCandidateRecording{
		ID: row.ID, NetworkID: row.NetworkID, ServiceID: row.ServiceID,
		RelPath: row.RelPath, IsTrashed: row.IsTrashed, OriginalMissing: row.OriginalMissing,
	}, nil
}

// SaveLogoArea は局ロック下で枠を保存し、既存候補を消して解析ヒントを返す。
func (s *Analysis) SaveLogoArea(ctx context.Context, station store.CMStationRef, input store.CMLogoAreaInput) (store.CMLogoCandidateJob, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.CMLogoCandidateJob{}, fmt.Errorf("beginning logo area save for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	recording, err := q.GetRecordingByID(ctx, input.RecordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CMLogoCandidateJob{}, fmt.Errorf("recording %d: %w", input.RecordingID,
				errors.Join(store.ErrNotFound, store.ErrCMLogoRecordingNotFound))
		}
		return store.CMLogoCandidateJob{}, fmt.Errorf("loading analysis recording %d: %w", input.RecordingID, err)
	}
	if recording.NetworkID != station.NetworkID || recording.ServiceID != station.ServiceID {
		return store.CMLogoCandidateJob{}, fmt.Errorf("recording %d: %w", input.RecordingID, store.ErrCMLogoRecordingWrongStation)
	}
	activeOriginal, err := q.HasCMRecordingOriginal(ctx, sqlcgen.HasCMRecordingOriginalParams{
		RecordingID: input.RecordingID, NetworkID: station.NetworkID, ServiceID: station.ServiceID,
		StoreName: mediaStoreNameArg(s.storeName),
	})
	if err != nil {
		return store.CMLogoCandidateJob{}, fmt.Errorf("checking analysis recording %d: %w", input.RecordingID, err)
	}
	if !activeOriginal {
		return store.CMLogoCandidateJob{}, fmt.Errorf("recording %d: %w", input.RecordingID, store.ErrCMLogoRecordingOriginalUnavailable)
	}
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return store.CMLogoCandidateJob{}, fmt.Errorf("locking logo state for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	if err := q.UpsertCMLogoArea(ctx, sqlcgen.UpsertCMLogoAreaParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
		X: input.X, Y: input.Y, W: input.W, H: input.H,
		CodedWidth: input.CodedWidth, CodedHeight: input.CodedHeight,
	}); err != nil {
		return store.CMLogoCandidateJob{}, fmt.Errorf("saving logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return store.CMLogoCandidateJob{}, fmt.Errorf("clearing CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID})
	if err != nil {
		return store.CMLogoCandidateJob{}, fmt.Errorf("loading saved logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	durationMs, err := q.GetCMRecordingDuration(ctx, input.RecordingID)
	if err != nil {
		return store.CMLogoCandidateJob{}, fmt.Errorf("getting recording %d duration for CM logo candidate: %w", input.RecordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CMLogoCandidateJob{}, fmt.Errorf("committing logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	return store.CMLogoCandidateJob{
		Station: station, RecordingID: input.RecordingID,
		RecordingDurationMs: durationMs, AreaUpdatedAt: area.UpdatedAt,
	}, nil
}

// DeleteLogoArea は局の採用待ち試行と枠を局ロック下で原子的に消す。
func (s *Analysis) DeleteLogoArea(ctx context.Context, station store.CMStationRef) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning logo area deletion for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return fmt.Errorf("locking logo state for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	if err := q.DeleteCMAdoptAttemptsForStation(ctx, sqlcgen.DeleteCMAdoptAttemptsForStationParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return fmt.Errorf("clearing adoption waits for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	if _, err := q.DeleteCMLogoArea(ctx, sqlcgen.DeleteCMLogoAreaParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return fmt.Errorf("deleting logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing logo area deletion for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	return nil
}

// DeleteLogo は局ロック下でロゴを消し、次の候補解析先があれば返す。
func (s *Analysis) DeleteLogo(ctx context.Context, station store.CMStationRef) (store.CMLogoDeleteResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.CMLogoDeleteResult{}, fmt.Errorf("beginning CM logo deletion for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return store.CMLogoDeleteResult{}, fmt.Errorf("locking logo state for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	if _, err := q.DeleteCMLogo(ctx, sqlcgen.DeleteCMLogoParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return store.CMLogoDeleteResult{}, fmt.Errorf("deleting CM logo for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	result := store.CMLogoDeleteResult{}
	area, areaErr := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID})
	if areaErr == nil {
		if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
			return store.CMLogoDeleteResult{}, fmt.Errorf("clearing CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
		}
		recording, recordingErr := q.GetCMLogoAnalysisRecording(ctx, sqlcgen.GetCMLogoAnalysisRecordingParams{
			NetworkID: station.NetworkID, ServiceID: station.ServiceID, StoreName: mediaStoreNameArg(s.storeName),
		})
		if recordingErr == nil {
			durationMs, err := q.GetCMRecordingDuration(ctx, recording.ID)
			if err != nil {
				return store.CMLogoDeleteResult{}, fmt.Errorf("getting recording %d duration for CM logo candidate: %w", recording.ID, err)
			}
			result.CandidateJob = &store.CMLogoCandidateJob{
				Station: station, RecordingID: recording.ID,
				RecordingDurationMs: durationMs, AreaUpdatedAt: area.UpdatedAt,
			}
		} else if !errors.Is(recordingErr, pgx.ErrNoRows) {
			return store.CMLogoDeleteResult{}, fmt.Errorf("finding analysis recording for network %d service %d: %w", station.NetworkID, station.ServiceID, recordingErr)
		}
	} else if !errors.Is(areaErr, pgx.ErrNoRows) {
		return store.CMLogoDeleteResult{}, fmt.Errorf("loading logo area for network %d service %d: %w", station.NetworkID, station.ServiceID, areaErr)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CMLogoDeleteResult{}, fmt.Errorf("committing CM logo deletion for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	return result, nil
}

// DeleteLogoCandidate は局ロック下で現在の候補を消す。
func (s *Analysis) DeleteLogoCandidate(ctx context.Context, station store.CMStationRef) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning CM logo candidate deletion for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return fmt.Errorf("locking candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return fmt.Errorf("deleting CM logo candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing CM logo candidate deletion for network %d service %d: %w", station.NetworkID, station.ServiceID, mapDatabaseError(err))
	}
	return nil
}

// AdoptLogoCandidate は局ロック下で候補を検証・採用し、必要なら検出結果も消す。
func (s *Analysis) AdoptLogoCandidate(ctx context.Context, station store.CMStationRef, redetect bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning CM logo candidate adoption for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return fmt.Errorf("locking candidate for network %d service %d: %w", station.NetworkID, station.ServiceID, err)
	}
	candidate, err := q.GetCMLogoCandidate(ctx, sqlcgen.GetCMLogoCandidateParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionCandidateUnavailable}
	}
	if err != nil {
		return fmt.Errorf("loading CM logo candidate: %w", err)
	}
	if candidate.State != "ready" {
		return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionCandidateNotReady, State: candidate.State}
	}
	area, err := q.GetCMLogoArea(ctx, sqlcgen.GetCMLogoAreaParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionAreaMissing}
	}
	if err != nil {
		return fmt.Errorf("loading taught logo area: %w", err)
	}
	if !candidate.ObservedAreaUpdatedAt.Equal(area.UpdatedAt) {
		return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionAreaChanged}
	}
	if candidate.RecordingID == nil || len(candidate.Lgd) == 0 {
		return &store.CMLogoAdoptionError{Reason: store.CMLogoAdoptionCandidateUnusable}
	}
	if err := q.UpsertCMLogo(ctx, sqlcgen.UpsertCMLogoParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
		Lgd: candidate.Lgd, PreviewPng: candidate.PreviewPng,
		LearnedFrom: candidate.RecordingID,
		CodedWidth:  candidate.CodedWidth, CodedHeight: candidate.CodedHeight,
	}); err != nil {
		return fmt.Errorf("adopting CM logo candidate: %w", mapDatabaseError(err))
	}
	if _, err := q.DeleteCMLogoCandidate(ctx, sqlcgen.DeleteCMLogoCandidateParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return fmt.Errorf("deleting adopted CM logo candidate: %w", mapDatabaseError(err))
	}
	if redetect {
		if err := q.DeleteCMDetectionsForStationWithActiveOriginal(ctx, sqlcgen.DeleteCMDetectionsForStationWithActiveOriginalParams{
			NetworkID: station.NetworkID, ServiceID: station.ServiceID, StoreName: mediaStoreNameArg(s.storeName),
		}); err != nil {
			return fmt.Errorf("clearing active CM detections after adoption: %w", mapDatabaseError(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing CM logo candidate adoption: %w", mapDatabaseError(err))
	}
	return nil
}

// BeginLogoCandidate は局ロック下で desired を再確認し running 試行を作る。
func (s *Analysis) BeginLogoCandidate(ctx context.Context, station store.CMStationRef, recordingID int64, areaUpdatedAt time.Time) (time.Time, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("beginning CM logo candidate attempt: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{NetworkID: station.NetworkID, ServiceID: station.ServiceID}); err != nil {
		return time.Time{}, false, fmt.Errorf("CM logo candidate: locking station: %w", err)
	}
	desired, err := q.IsCMLogoCandidateDesired(ctx, sqlcgen.IsCMLogoCandidateDesiredParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID, AreaUpdatedAt: areaUpdatedAt,
	})
	if err != nil {
		return time.Time{}, false, fmt.Errorf("CM logo candidate: checking desired state: %w", err)
	}
	if !desired {
		rollback(tx)
		return time.Time{}, false, nil
	}
	attemptedAt, err := q.InsertCMLogoCandidateRunning(ctx, sqlcgen.InsertCMLogoCandidateRunningParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
		RecordingID: recordingID, AreaUpdatedAt: areaUpdatedAt,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			rollback(tx)
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("CM logo candidate: creating running row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, false, fmt.Errorf("CM logo candidate: committing running row: %w", mapDatabaseError(err))
	}
	return attemptedAt, true, nil
}

// MarkLogoCandidateReady は版と試行時刻が一致する候補だけを ready にする。
func (s *Analysis) MarkLogoCandidateReady(ctx context.Context, station store.CMStationRef, areaUpdatedAt, attemptedAt time.Time, lgd, previewPNG []byte) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning CM logo candidate save: %w", err)
	}
	defer rollback(tx)
	n, err := sqlcgen.New(tx).MarkCMLogoCandidateReady(ctx, sqlcgen.MarkCMLogoCandidateReadyParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
		AreaUpdatedAt: areaUpdatedAt, AttemptedAt: attemptedAt, Lgd: lgd, PreviewPng: previewPNG,
	})
	if err != nil {
		return false, fmt.Errorf("saving CM logo candidate: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing CM logo candidate save: %w", mapDatabaseError(err))
	}
	return n > 0, nil
}

// MarkLogoCandidateFailed は版と試行時刻が一致する候補だけを failed にする。
func (s *Analysis) MarkLogoCandidateFailed(ctx context.Context, station store.CMStationRef, areaUpdatedAt, attemptedAt time.Time, stage *string, message string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning CM logo candidate failure update: %w", err)
	}
	defer rollback(tx)
	n, err := sqlcgen.New(tx).MarkCMLogoCandidateFailure(ctx, sqlcgen.MarkCMLogoCandidateFailureParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
		AreaUpdatedAt: areaUpdatedAt, AttemptedAt: attemptedAt, Stage: stage, Error: &message,
	})
	if err != nil {
		return false, fmt.Errorf("marking CM logo candidate failed: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing CM logo candidate failure update: %w", mapDatabaseError(err))
	}
	return n > 0, nil
}

// DiscardLogoCandidate は版と試行時刻が一致する running 候補だけを消す。
func (s *Analysis) DiscardLogoCandidate(ctx context.Context, station store.CMStationRef, areaUpdatedAt, attemptedAt time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning CM logo candidate discard: %w", err)
	}
	defer rollback(tx)
	if _, err := sqlcgen.New(tx).DeleteCMLogoCandidateRunningAttempt(ctx, sqlcgen.DeleteCMLogoCandidateRunningAttemptParams{
		NetworkID: station.NetworkID, ServiceID: station.ServiceID,
		AreaUpdatedAt: areaUpdatedAt, AttemptedAt: attemptedAt,
	}); err != nil {
		return fmt.Errorf("discarding CM logo candidate: %w", mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing CM logo candidate discard: %w", mapDatabaseError(err))
	}
	return nil
}

func cmLogoAreaFromRow(row sqlcgen.GetCMLogoAreaRow) store.CMLogoArea {
	return store.CMLogoArea{
		X: row.X, Y: row.Y, W: row.W, H: row.H,
		CodedWidth: row.CodedWidth, CodedHeight: row.CodedHeight, UpdatedAt: row.UpdatedAt,
	}
}
