package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

var _ store.CMDetection = (*Analysis)(nil)

// GetWorkItem は録画と有効な原本の CM 検出入力を返す。
func (s *Analysis) GetWorkItem(ctx context.Context, recordingID int64) (store.CMDetectionWorkItem, error) {
	row, err := sqlcgen.New(s.pool).GetCMDetectionWorkItem(ctx, sqlcgen.GetCMDetectionWorkItemParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.storeName),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CMDetectionWorkItem{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return store.CMDetectionWorkItem{}, fmt.Errorf("loading CM detection work item for recording %d: %w", recordingID, err)
	}
	return store.CMDetectionWorkItem{
		ID:              row.ID,
		Station:         store.CMStationRef{NetworkID: row.NetworkID, ServiceID: row.ServiceID},
		RelPath:         row.RelPath,
		Enabled:         row.CmDetect,
		IsTrashed:       row.IsTrashed,
		OriginalMissing: row.OriginalMissing,
		Detected:        row.Detected,
	}, nil
}

// IsDetectionDesired は録画の現在の CM 検出が desired かを返す。
func (s *Analysis) IsDetectionDesired(ctx context.Context, recordingID int64) (bool, error) {
	desired, err := sqlcgen.New(s.pool).IsCMDetectionDesired(ctx, recordingID)
	if err != nil {
		return false, fmt.Errorf("checking desired CM detection for recording %d: %w", recordingID, err)
	}
	return desired, nil
}

// GetRecordingDuration は録画の実尺または番組尺をミリ秒で返す。
func (s *Analysis) GetRecordingDuration(ctx context.Context, recordingID int64) (int64, error) {
	duration, err := sqlcgen.New(s.pool).GetCMRecordingDuration(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return 0, fmt.Errorf("getting duration for recording %d: %w", recordingID, err)
	}
	return duration, nil
}

// BeginAttempt は現在の方針世代で試行を開始し、実行するかを返す。
func (s *Analysis) BeginAttempt(ctx context.Context, recordingID int64, maxAttempts int32) (store.CMDetectionAttempt, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.CMDetectionAttempt{}, false, fmt.Errorf("beginning CM detection attempt for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	item, err := q.GetCMDetectionWorkItem(ctx, sqlcgen.GetCMDetectionWorkItemParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.storeName),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CMDetectionAttempt{}, false, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return store.CMDetectionAttempt{}, false, fmt.Errorf("loading station for CM detection attempt on recording %d: %w", recordingID, err)
	}
	row, err := q.BeginCMDetectionAttempt(ctx, sqlcgen.BeginCMDetectionAttemptParams{
		RecordingID: recordingID,
		MaxAttempts: maxAttempts,
	})
	if err != nil {
		return store.CMDetectionAttempt{}, false, fmt.Errorf("starting CM detection attempt for recording %d: %w", recordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CMDetectionAttempt{}, false, fmt.Errorf("committing CM detection attempt for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return store.CMDetectionAttempt{
		RecordingID:      recordingID,
		Station:          store.CMStationRef{NetworkID: item.NetworkID, ServiceID: item.ServiceID},
		AttemptCount:     row.AttemptCount,
		PolicyGeneration: row.PolicyGeneration,
	}, row.ShouldRun, nil
}

// FailAttempt は一致する running 試行を失敗状態にし、更新したかを返す。
func (s *Analysis) FailAttempt(ctx context.Context, attempt store.CMDetectionAttempt, state string, stage *string, message string) (bool, error) {
	rows, err := sqlcgen.New(s.pool).MarkCMDetectionFailure(ctx, sqlcgen.MarkCMDetectionFailureParams{
		State: state, Stage: stage, Error: &message,
		RecordingID: attempt.RecordingID, AttemptCount: attempt.AttemptCount,
		PolicyGeneration: attempt.PolicyGeneration,
	})
	if err != nil {
		return false, fmt.Errorf("marking CM detection attempt %d for recording %d as %s: %w", attempt.AttemptCount, attempt.RecordingID, state, err)
	}
	return rows > 0, nil
}

// CancelAttempt は一致する2回目以降の running 試行を予算を消費しない状態に戻す。
func (s *Analysis) CancelAttempt(ctx context.Context, attempt store.CMDetectionAttempt) error {
	_, err := sqlcgen.New(s.pool).CancelCMDetectionRunningAttempt(ctx, sqlcgen.CancelCMDetectionRunningAttemptParams{
		RecordingID: attempt.RecordingID, AttemptCount: attempt.AttemptCount,
		PolicyGeneration: attempt.PolicyGeneration,
	})
	if err != nil {
		return fmt.Errorf("canceling CM detection attempt %d for recording %d: %w", attempt.AttemptCount, attempt.RecordingID, err)
	}
	return nil
}

// AbortAttempt は一致する初回の running 試行を削除する。
func (s *Analysis) AbortAttempt(ctx context.Context, attempt store.CMDetectionAttempt) error {
	_, err := sqlcgen.New(s.pool).DeleteCMDetectionRunningAttempt(ctx, sqlcgen.DeleteCMDetectionRunningAttemptParams{
		RecordingID: attempt.RecordingID, AttemptCount: attempt.AttemptCount,
		PolicyGeneration: attempt.PolicyGeneration,
	})
	if err != nil {
		return fmt.Errorf("aborting CM detection attempt %d for recording %d: %w", attempt.AttemptCount, attempt.RecordingID, err)
	}
	return nil
}

// SaveDetection は一致する現在世代の試行について、正規化した CM 区間を保存する。
// 局、録画、試行の順で書き手を直列化する。ごみ箱の録画でも desired でなければ試行を消すので、
// lockRecording ではなく lockRecordingState を使う。
// 局ロゴの版は、解析をロゴありで始めた試行（LogoLearnedAt が非 nil）だけ照合する。
func (s *Analysis) SaveDetection(ctx context.Context, attempt store.CMDetectionAttempt, ranges []chapters.Range) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning CM result transaction for recording %d: %w", attempt.RecordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{
		NetworkID: attempt.Station.NetworkID, ServiceID: attempt.Station.ServiceID,
	}); err != nil {
		return false, fmt.Errorf("locking station logo state for CM result: %w", err)
	}
	if _, err := lockRecordingState(ctx, q, attempt.RecordingID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("locking recording %d for CM result: %w", attempt.RecordingID, err)
	}
	currentAttemptCount, err := q.LockCMDetectionAttempt(ctx, sqlcgen.LockCMDetectionAttemptParams{
		RecordingID: attempt.RecordingID, PolicyGeneration: attempt.PolicyGeneration,
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && currentAttemptCount != attempt.AttemptCount) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking CM detection attempt fence for recording %d: %w", attempt.RecordingID, err)
	}
	desired, err := q.IsCMDetectionDesired(ctx, attempt.RecordingID)
	if err != nil {
		return false, fmt.Errorf("rechecking CM detection policy for recording %d: %w", attempt.RecordingID, err)
	}
	if !desired {
		if err := q.DeleteCMDetectionAttempt(ctx, attempt.RecordingID); err != nil {
			return false, fmt.Errorf("clearing disabled CM detection attempt for recording %d: %w", attempt.RecordingID, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("committing disabled CM detection state for recording %d: %w", attempt.RecordingID, mapDatabaseError(err))
		}
		return false, nil
	}
	if attempt.LogoLearnedAt != nil {
		currentLogo, logoErr := q.GetCMLogo(ctx, sqlcgen.GetCMLogoParams{
			NetworkID: attempt.Station.NetworkID, ServiceID: attempt.Station.ServiceID,
		})
		if logoErr != nil && !errors.Is(logoErr, pgx.ErrNoRows) {
			return false, fmt.Errorf("checking station logo version for recording %d: %w", attempt.RecordingID, logoErr)
		}
		if errors.Is(logoErr, pgx.ErrNoRows) || !currentLogo.LearnedAt.Equal(*attempt.LogoLearnedAt) {
			if err := q.DeleteCMDetectionAttempt(ctx, attempt.RecordingID); err != nil {
				return false, fmt.Errorf("discarding stale CM result attempt for recording %d: %w", attempt.RecordingID, err)
			}
			if err := tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("committing stale CM result discard for recording %d: %w", attempt.RecordingID, mapDatabaseError(err))
			}
			return false, nil
		}
	}
	if err := q.SaveCMDetection(ctx, sqlcgen.SaveCMDetectionParams{
		RecordingID: attempt.RecordingID, CmRanges: encodeInt8Multirange(ranges),
	}); err != nil {
		return false, fmt.Errorf("saving CM ranges for recording %d: %w", attempt.RecordingID, mapDatabaseError(err))
	}
	if err := q.DeleteCMDetectionAttempt(ctx, attempt.RecordingID); err != nil {
		return false, fmt.Errorf("clearing CM detection attempt for recording %d: %w", attempt.RecordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing CM result for recording %d: %w", attempt.RecordingID, mapDatabaseError(err))
	}
	return true, nil
}

// SaveLearnedLogo は局の枠版と試行が一致する場合にロゴを保存する。
// 局、試行の順で書き手を直列化する。
func (s *Analysis) SaveLearnedLogo(ctx context.Context, attempt store.CMDetectionAttempt, logo store.CMDetectionLogo, observedAreaVersion *time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning learned station logo save for recording %d: %w", attempt.RecordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if err := q.LockCMStation(ctx, sqlcgen.LockCMStationParams{
		NetworkID: attempt.Station.NetworkID, ServiceID: attempt.Station.ServiceID,
	}); err != nil {
		return false, fmt.Errorf("locking station logo state for recording %d: %w", attempt.RecordingID, err)
	}
	currentAttemptCount, err := q.LockCMDetectionAttempt(ctx, sqlcgen.LockCMDetectionAttemptParams{
		RecordingID: attempt.RecordingID, PolicyGeneration: attempt.PolicyGeneration,
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && currentAttemptCount != attempt.AttemptCount) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking CM detection attempt fence for recording %d: %w", attempt.RecordingID, err)
	}
	rows, err := q.InsertLearnedCMLogo(ctx, sqlcgen.InsertLearnedCMLogoParams{
		NetworkID: attempt.Station.NetworkID, ServiceID: attempt.Station.ServiceID,
		Lgd: logo.LGD, PreviewPng: logo.PreviewPNG, LearnedFrom: attempt.RecordingID,
		ObservedAreaUpdatedAt: observedAreaVersion,
		CodedWidth:            logo.CodedWidth, CodedHeight: logo.CodedHeight,
	})
	if err != nil {
		return false, fmt.Errorf("saving learned station logo for recording %d: %w", attempt.RecordingID, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing learned station logo for recording %d: %w", attempt.RecordingID, mapDatabaseError(err))
	}
	return rows > 0, nil
}

// RetryDetection は結果と試行を消し、原本がある場合に再試行情報を返す。
func (s *Analysis) RetryDetection(ctx context.Context, recordingID int64) (store.CMDetectionRetry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.CMDetectionRetry{}, fmt.Errorf("beginning CM detection retry for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	recording, err := q.GetRecordingByID(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.CMDetectionRetry{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return store.CMDetectionRetry{}, fmt.Errorf("loading recording %d for CM detection retry: %w", recordingID, err)
	}
	if recording.PurgedAt != nil {
		return store.CMDetectionRetry{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	active, err := q.GetCMRetryOriginal(ctx, sqlcgen.GetCMRetryOriginalParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.storeName),
	})
	if err != nil {
		return store.CMDetectionRetry{}, fmt.Errorf("checking active original for recording %d: %w", recordingID, err)
	}
	if !active {
		return store.CMDetectionRetry{ActiveOriginal: false}, nil
	}
	if err := q.DeleteCMDetection(ctx, recordingID); err != nil {
		return store.CMDetectionRetry{}, fmt.Errorf("clearing CM detection result for recording %d: %w", recordingID, err)
	}
	if err := q.DeleteCMDetectionAttempt(ctx, recordingID); err != nil {
		return store.CMDetectionRetry{}, fmt.Errorf("clearing CM detection attempt for recording %d: %w", recordingID, err)
	}
	desired, err := q.IsCMDetectionDesired(ctx, recordingID)
	if err != nil {
		return store.CMDetectionRetry{}, fmt.Errorf("checking CM detection policy for recording %d: %w", recordingID, err)
	}
	result := store.CMDetectionRetry{ActiveOriginal: true, Desired: desired}
	if desired {
		result.RecordingDurationMs, err = q.GetCMRecordingDuration(ctx, recordingID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.CMDetectionRetry{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
			}
			return store.CMDetectionRetry{}, fmt.Errorf("getting recording %d duration for CM detection: %w", recordingID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CMDetectionRetry{}, fmt.Errorf("committing CM detection retry for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return result, nil
}

// DeleteStaleAttempts は現在の方針世代でない CM 検出試行を削除する。
func (s *Analysis) DeleteStaleAttempts(ctx context.Context) error {
	if err := sqlcgen.New(s.pool).DeleteStaleCMDetectionAttempts(ctx); err != nil {
		return fmt.Errorf("clearing stale CM detection attempts: %w", err)
	}
	return nil
}

// ListMissingDetections は desired な CM 検出を recording ID 順に先頭から limit 件返す。
func (s *Analysis) ListMissingDetections(ctx context.Context, limit int32) ([]store.CMDetectionJob, error) {
	rows, err := sqlcgen.New(s.pool).ListMissingCMDetections(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("listing missing CM detections: %w", err)
	}
	jobs := make([]store.CMDetectionJob, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, store.CMDetectionJob{RecordingID: row.RecordingID, RecordingDurationMs: row.RecordingDurationMs})
	}
	return jobs, nil
}

// ListMissingLogoCandidates は desired な候補解析を (network_id, service_id) 順に先頭から limit 件返す。
func (s *Analysis) ListMissingLogoCandidates(ctx context.Context, limit int32) ([]store.CMLogoCandidateJob, error) {
	rows, err := sqlcgen.New(s.pool).ListMissingCMLogoCandidates(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("listing missing CM logo candidates: %w", err)
	}
	jobs := make([]store.CMLogoCandidateJob, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, store.CMLogoCandidateJob{
			Station:     store.CMStationRef{NetworkID: row.NetworkID, ServiceID: row.ServiceID},
			RecordingID: row.RecordingID, RecordingDurationMs: row.RecordingDurationMs,
			AreaUpdatedAt: row.AreaUpdatedAt,
		})
	}
	return jobs, nil
}

// FailOrphanLogoCandidates は worker の締切と recoveryGrace を過ぎた running 候補を失敗にする。
// 締切と attempted_at の比較には、同じ読み取りで取得した DB の時刻を使う。
func (s *Analysis) FailOrphanLogoCandidates(ctx context.Context, recoveryGrace time.Duration, timeoutFor func(int64) time.Duration) error {
	if timeoutFor == nil {
		return fmt.Errorf("CM logo candidate timeout function is nil")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning orphan CM logo candidate recovery: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	candidates, err := q.ListRunningCMLogoCandidates(ctx)
	if err != nil {
		return fmt.Errorf("listing running CM logo candidates: %w", err)
	}
	for _, candidate := range candidates {
		durationMs, err := q.GetCMRecordingDuration(ctx, candidate.RecordingID)
		if errors.Is(err, pgx.ErrNoRows) {
			// 録画が消えた候補は実行側の締切を引けない。結果は不要で、遅い書き込みは版照合で拒否される。
			durationMs = 0
		} else if err != nil {
			return fmt.Errorf("getting recording %d duration for orphan CM logo candidate: %w", candidate.RecordingID, err)
		}
		deadline := candidate.AttemptedAt.Add(timeoutFor(durationMs)).Add(recoveryGrace)
		if !candidate.CurrentTime.After(deadline) {
			continue
		}
		if _, err := q.FailOrphanCMLogoCandidate(ctx, sqlcgen.FailOrphanCMLogoCandidateParams{
			NetworkID: candidate.NetworkID, ServiceID: candidate.ServiceID,
			RecordingID: candidate.RecordingID, AreaUpdatedAt: candidate.ObservedAreaUpdatedAt,
			AttemptedAt: candidate.AttemptedAt,
		}); err != nil {
			return fmt.Errorf("failing orphan CM logo candidate for network %d service %d: %w", candidate.NetworkID, candidate.ServiceID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing orphan CM logo candidate recovery: %w", mapDatabaseError(err))
	}
	return nil
}

// encodeInt8Multirange は区間を PostgreSQL int8multirange の入力形式に変換する。
func encodeInt8Multirange(ranges []chapters.Range) string {
	parts := make([]string, 0, len(ranges))
	for _, span := range ranges {
		if span.EndMs <= span.StartMs {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%d,%d)", span.StartMs, span.EndMs))
	}
	if len(parts) == 0 {
		return "{}"
	}
	return "{" + strings.Join(parts, ",") + "}"
}
