package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

var _ store.EncodePolicy = (*Media)(nil)

// AppendProfiles は active な原本を持つ録画の desired にプロファイルを追加し、失敗試行を戻す。
func (s *Media) AppendProfiles(ctx context.Context, recordingID int64, names []string, validate func(desired []string) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning encode profile append for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if _, err := q.GetRecordingByID(ctx, recordingID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return fmt.Errorf("loading recording %d: %w", recordingID, err)
	}
	if _, err := q.GetActiveOriginalMediaAsset(ctx, recordingID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrOriginalUnavailable)
		}
		return fmt.Errorf("loading active original for recording %d: %w", recordingID, err)
	}

	// The policy row lock must precede reading desired. If a concurrent removal is
	// still uncommitted, this statement waits and returns the committed state.
	policy, err := q.LockRecordingEncodePolicy(ctx, recordingID)
	var desired []string
	if errors.Is(err, pgx.ErrNoRows) {
		desired = []string{}
	} else if err != nil {
		return fmt.Errorf("locking encode policy for recording %d: %w", recordingID, err)
	} else {
		desired = slices.Clone(policy.EncodeProfiles)
	}
	merged := append(slices.Clone(desired), names...)
	if validate != nil {
		if err := validate(merged); err != nil {
			return err
		}
	}
	if err := q.AppendRecordingEncodeProfiles(ctx, sqlcgen.AppendRecordingEncodeProfilesParams{ID: recordingID, Profiles: names}); err != nil {
		return fmt.Errorf("appending encode profiles for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	if _, err := q.DeleteFailedRecordingEncodeAttempts(ctx, sqlcgen.DeleteFailedRecordingEncodeAttemptsParams{
		RecordingID: recordingID,
		Profiles:    names,
	}); err != nil {
		return fmt.Errorf("resetting failed encode attempts for recording %d: %w", recordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing encode profile append for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return nil
}

// RemoveEncodedAsset は録画の行と方針の行の順に変更を直列化し、版を 1 本外す。
func (s *Media) RemoveEncodedAsset(ctx context.Context, recordingID int64, profile string, validate func(desired []string) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning encoded asset removal for recording %d profile %q: %w", recordingID, profile, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	recording, err := lockRecordingState(ctx, q, recordingID)
	if err != nil {
		return err
	}
	if recording.IsPurged {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	assetID, err := q.GetActiveEncodedMediaAssetID(ctx, sqlcgen.GetActiveEncodedMediaAssetIDParams{
		RecordingID: recordingID,
		Profile:     &profile,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("recording %d profile %q: %w", recordingID, profile, store.ErrEncodedAssetNotFound)
	}
	if err != nil {
		return fmt.Errorf("loading encoded asset for recording %d profile %q: %w", recordingID, profile, err)
	}

	if err := q.FreezeRecordingEncodePolicyIfMissing(ctx, recordingID); err != nil {
		return fmt.Errorf("freezing encode policy for recording %d: %w", recordingID, err)
	}
	policy, err := q.LockRecordingEncodePolicy(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("locking encode policy for recording %d: %w", recordingID, err)
	}
	if slices.Contains(policy.EncodeProfiles, profile) && validate != nil {
		remaining := slices.DeleteFunc(slices.Clone(policy.EncodeProfiles), func(name string) bool { return name == profile })
		if err := validate(remaining); err != nil {
			return err
		}
	}
	if err := q.RemoveRecordingEncodeProfile(ctx, sqlcgen.RemoveRecordingEncodeProfileParams{RecordingID: recordingID, Profile: profile}); err != nil {
		return fmt.Errorf("removing encode profile %q from recording %d: %w", profile, recordingID, mapDatabaseError(err))
	}
	if err := q.DeleteEncodedAssetRebuildRequest(ctx, sqlcgen.DeleteEncodedAssetRebuildRequestParams{RecordingID: recordingID, Profile: profile}); err != nil {
		return fmt.Errorf("clearing rebuild request for recording %d profile %q: %w", recordingID, profile, err)
	}
	if err := q.InsertEncodedAssetRemovalRequest(ctx, sqlcgen.InsertEncodedAssetRemovalRequestParams{RecordingID: recordingID, Profile: profile}); err != nil {
		return fmt.Errorf("recording removal request for recording %d profile %q: %w", recordingID, profile, mapDatabaseError(err))
	}
	removable, err := q.IsRemovedEncodedAsset(ctx, assetID)
	if err != nil {
		return fmt.Errorf("checking remaining copies of recording %d: %w", recordingID, err)
	}
	if !removable {
		return fmt.Errorf("recording %d profile %q: %w", recordingID, profile, store.ErrLastViewableCopy)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing removal of recording %d profile %q: %w", recordingID, profile, mapDatabaseError(err))
	}
	return nil
}

// RequestCutRebuild は録画の行をロックしてチャプターを読み、現在の keep を作り直し要求に記録する。
func (s *Media) RequestCutRebuild(ctx context.Context, recordingID int64, profile string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning cut rebuild request for recording %d profile %q: %w", recordingID, profile, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if _, err := lockRecordingState(ctx, q, recordingID); err != nil {
		return err
	}
	if _, err := q.GetActiveOriginalMediaAsset(ctx, recordingID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrOriginalUnavailable)
		}
		return fmt.Errorf("loading active original for recording %d: %w", recordingID, err)
	}
	state, err := q.GetRecordingChapterState(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("loading chapter state for recording %d: %w", recordingID, err)
	}
	if !state.Owned {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrChaptersNotOwned)
	}
	spansJSON, err := q.GetRecordingChapterSpansJSON(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("loading chapter spans for recording %d: %w", recordingID, err)
	}
	var spans []chapters.Span
	if err := json.Unmarshal(spansJSON, &spans); err != nil {
		return fmt.Errorf("decoding chapter spans for recording %d: %w", recordingID, err)
	}
	keep := chapters.KeepRanges(chapters.Derive(true, spans, nil, state.ProgramDurationMs))
	if len(keep) == 0 {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrKeepRangesEmpty)
	}
	desired, err := q.GetRecordingEncodeProfilesForShare(ctx, recordingID)
	if errors.Is(err, pgx.ErrNoRows) {
		desired = nil
	} else if err != nil {
		return fmt.Errorf("loading desired encode profiles for recording %d: %w", recordingID, err)
	}
	if !slices.Contains(desired, profile) {
		return fmt.Errorf("recording %d profile %q: %w", recordingID, profile, store.ErrProfileNotDesired)
	}
	requestedKeep, err := db.KeepRangesParam(keep)
	if err != nil {
		return fmt.Errorf("encoding requested keep ranges for recording %d: %w", recordingID, err)
	}
	if _, err := q.DeleteFailedRecordingEncodeAttempts(ctx, sqlcgen.DeleteFailedRecordingEncodeAttemptsParams{
		RecordingID: recordingID,
		Profiles:    []string{profile},
	}); err != nil {
		return fmt.Errorf("resetting failed encode attempt for recording %d profile %q: %w", recordingID, profile, err)
	}
	if err := q.UpsertEncodedAssetRebuildRequest(ctx, sqlcgen.UpsertEncodedAssetRebuildRequestParams{
		RecordingID:   recordingID,
		Profile:       profile,
		RequestedKeep: requestedKeep,
	}); err != nil {
		return fmt.Errorf("recording rebuild request for recording %d profile %q: %w", recordingID, profile, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing rebuild request for recording %d profile %q: %w", recordingID, profile, mapDatabaseError(err))
	}
	return nil
}

// SetPolicy は指定された保持・CM 検出方針を 1 つのトランザクションで更新する。
func (s *Media) SetPolicy(ctx context.Context, recordingID int64, update store.EncodePolicyUpdate) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning encode policy update for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	recording, err := q.GetRecordingByID(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return fmt.Errorf("loading recording %d: %w", recordingID, err)
	}
	if recording.PurgedAt != nil {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if update.KeepOriginal != nil {
		rows, err := q.SetRecordingKeepOriginal(ctx, sqlcgen.SetRecordingKeepOriginalParams{
			RecordingID:  recordingID,
			KeepOriginal: *update.KeepOriginal,
		})
		if err != nil {
			return fmt.Errorf("updating keep policy for recording %d: %w", recordingID, mapDatabaseError(err))
		}
		if rows == 0 && *update.KeepOriginal == "until_encoded" {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrEncodeProfilesRequired)
		}
	}
	if update.CMDetect != nil {
		if *update.CMDetect {
			rows, err := q.SetRecordingCMDetection(ctx, recordingID)
			if err != nil {
				return fmt.Errorf("enabling CM detection for recording %d: %w", recordingID, mapDatabaseError(err))
			}
			if rows == 0 {
				return fmt.Errorf("recording %d: %w", recordingID, store.ErrOriginalUnavailable)
			}
		} else if err := q.ClearRecordingCMDetection(ctx, recordingID); err != nil {
			return fmt.Errorf("disabling CM detection for recording %d: %w", recordingID, mapDatabaseError(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing encode policy for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return nil
}
