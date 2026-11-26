package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

var _ store.EncodePolicy = (*Media)(nil)

// AppendProfiles は方針の行を取ってから desired を検証し、プロファイルの追加と失敗試行のリセットを行う。
func (s *Media) AppendProfiles(ctx context.Context, recordingID int64, names []string, validate func(desired []string) error) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := encodeRecordingPurged(ctx, tx, recordingID); err != nil {
			return err
		}
		if err := s.requireEncodeOriginal(ctx, tx, recordingID); err != nil {
			return err
		}
		desired, err := lockOrFreezeEncodePolicy(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		merged := append(desired, names...)
		if validate != nil {
			if err := validate(slices.Clone(merged)); err != nil {
				return err
			}
		}
		slices.Sort(merged)
		raw, err := json.Marshal(slices.Compact(merged))
		if err != nil {
			return fmt.Errorf("encoding desired profiles: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE recording_processing_policy SET encode_profiles = ?, updated_at = CURRENT_TIMESTAMP(6)
WHERE recording_id = ?`, string(raw), recordingID); err != nil {
			return fmt.Errorf("appending encode profiles for recording %d: %w", recordingID, err)
		}
		requested, err := json.Marshal(names)
		if err != nil {
			return fmt.Errorf("encoding requested profiles: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM encoded_asset_removal_requests
WHERE recording_id = ? AND JSON_CONTAINS(?, JSON_QUOTE(profile))`, recordingID, string(requested)); err != nil {
			return fmt.Errorf("clearing removal requests for recording %d: %w", recordingID, err)
		}
		return deleteFailedEncodeAttempts(ctx, tx, recordingID, string(requested))
	})
}

// RemoveEncodedAsset は録画の行、方針の行の順に取り、視聴可能なコピーが残る場合に版を外す。
func (s *Media) RemoveEncodedAsset(ctx context.Context, recordingID int64, profile string, validate func(desired []string) error) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		_, purged, err := lockRecordingFlags(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		if purged {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		var assetID int64
		derived := nullableString(s.derivedStoreName())
		err = tx.QueryRowContext(ctx, `
SELECT a.id FROM media_assets a
WHERE a.recording_id = ? AND a.kind = 'encoded' AND a.profile = ?
  AND EXISTS (SELECT 1 FROM active_media_asset_copies c
              WHERE c.media_asset_id = a.id AND (? IS NULL OR c.store = ?))`,
			recordingID, profile, derived, derived).Scan(&assetID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("recording %d profile %q: %w", recordingID, profile, store.ErrEncodedAssetNotFound)
		}
		if err != nil {
			return fmt.Errorf("loading encoded asset for recording %d profile %q: %w", recordingID, profile, err)
		}

		desired, err := lockOrFreezeEncodePolicy(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		contained := slices.Contains(desired, profile)
		remaining := slices.DeleteFunc(desired, func(name string) bool { return name == profile })
		if contained && validate != nil {
			if err := validate(slices.Clone(remaining)); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(remaining)
		if err != nil {
			return fmt.Errorf("encoding remaining profiles: %w", err)
		}
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE recording_processing_policy
SET keep_original = CASE WHEN JSON_LENGTH(?) = 0 THEN 'always' ELSE keep_original END,
    encode_profiles = ?, updated_at = ?
WHERE recording_id = ?`, string(raw), string(raw), now, recordingID); err != nil {
			return fmt.Errorf("removing encode profile %q from recording %d: %w", profile, recordingID, err)
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM encoded_asset_rebuild_requests WHERE recording_id = ? AND profile = ?`, recordingID, profile); err != nil {
			return fmt.Errorf("clearing rebuild request for recording %d profile %q: %w", recordingID, profile, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO encoded_asset_removal_requests (recording_id, profile, requested_at) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE requested_at = requested_at`, recordingID, profile, now); err != nil {
			return fmt.Errorf("recording removal request for recording %d profile %q: %w", recordingID, profile, err)
		}
		var removable bool
		if err := tx.QueryRowContext(ctx, `
SELECT `+removedEncodedAssetSQL+` FROM media_assets a WHERE a.id = ?`, assetID).Scan(&removable); err != nil {
			return fmt.Errorf("checking remaining copies of recording %d: %w", recordingID, err)
		}
		if !removable {
			return fmt.Errorf("recording %d profile %q: %w", recordingID, profile, store.ErrLastViewableCopy)
		}
		return nil
	})
}

// RequestCutRebuild は録画の行を取ってから確認済みチャプターを読み、現在の keep を作り直し要求に記録する。
func (s *Media) RequestCutRebuild(ctx context.Context, recordingID int64, profile string) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockRecordingState(ctx, tx, recordingID); err != nil {
			return err
		}
		if err := s.requireEncodeOriginal(ctx, tx, recordingID); err != nil {
			return err
		}
		keep, owned, err := currentCutKeepTx(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrChaptersNotOwned)
		}
		if len(keep) == 0 {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrKeepRangesEmpty)
		}
		desired, err := encodeProfileDesiredForShare(ctx, tx, recordingID, profile)
		if err != nil {
			return err
		}
		if !desired {
			return fmt.Errorf("recording %d profile %q: %w", recordingID, profile, store.ErrProfileNotDesired)
		}
		requested, err := json.Marshal([]string{profile})
		if err != nil {
			return fmt.Errorf("encoding requested profile: %w", err)
		}
		if err := deleteFailedEncodeAttempts(ctx, tx, recordingID, string(requested)); err != nil {
			return err
		}
		raw, err := encodeKeepRanges(keep)
		if err != nil {
			return fmt.Errorf("encoding requested keep for recording %d: %w", recordingID, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO encoded_asset_rebuild_requests (recording_id, profile, requested_keep, requested_at)
VALUES (?, ?, ?, CURRENT_TIMESTAMP(6)) AS incoming
ON DUPLICATE KEY UPDATE requested_keep = incoming.requested_keep, requested_at = incoming.requested_at`,
			recordingID, profile, string(raw)); err != nil {
			return fmt.Errorf("recording rebuild request for recording %d profile %q: %w", recordingID, profile, err)
		}
		return nil
	})
}

// SetPolicy は保持と CM 検出の適用条件を各書き込み文で評価し、両方を 1 つの tx で更新する。
func (s *Media) SetPolicy(ctx context.Context, recordingID int64, update store.EncodePolicyUpdate) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		purged, err := encodeRecordingPurged(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		if purged {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		var keepMatched bool
		if update.KeepOriginal != nil {
			// PK に一致する行を更新対象にし、desired の条件は SET で評価する。
			keep := *update.KeepOriginal
			keepMatched, err = execPolicyWrite(ctx, tx, `
UPDATE recording_processing_policy
SET keep_original = IF(LAST_INSERT_ID(? <> 'until_encoded' OR JSON_LENGTH(encode_profiles) > 0), ?, keep_original),
    updated_at = ?
WHERE recording_id = ?`, keep, keep, now, recordingID)
			if err != nil {
				return fmt.Errorf("updating keep policy for recording %d: %w", recordingID, err)
			}
			if !keepMatched && keep == "until_encoded" {
				return fmt.Errorf("recording %d: %w", recordingID, store.ErrEncodeProfilesRequired)
			}
		}
		if update.CMDetect == nil {
			return nil
		}
		if *update.CMDetect {
			return enableCMDetection(ctx, tx, recordingID, now, keepMatched)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE recording_processing_policy
SET cm_detect_generation = cm_detect_generation + IF(cm_detect, 1, 0),
    cm_detect = FALSE, updated_at = ?
WHERE recording_id = ?`, now, recordingID); err != nil {
			return fmt.Errorf("disabling CM detection for recording %d: %w", recordingID, err)
		}
		return nil
	})
}

// presentCMOriginalSQL は CM 検出を有効にできる原本の条件。置き場を限定しない。
const presentCMOriginalSQL = `r.deleted_at IS NULL AND EXISTS (
    SELECT 1 FROM media_assets o
    JOIN active_present_media_asset_copies c ON c.media_asset_id = o.id
    WHERE o.recording_id = r.id AND o.kind = 'original'
)`

func enableCMDetection(ctx context.Context, tx *sql.Tx, recordingID int64, now time.Time, keepMatched bool) error {
	matched, err := execPolicyWrite(ctx, tx, `
UPDATE recording_processing_policy p
SET cm_detect_generation = cm_detect_generation + IF(cm_detect, 0, 1),
    cm_detect = LAST_INSERT_ID(1), updated_at = ?
WHERE p.recording_id = ? AND EXISTS (
    SELECT 1 FROM recordings r WHERE r.id = p.recording_id AND `+presentCMOriginalSQL+`
)`, now, recordingID)
	if err == nil && !matched && !keepMatched {
		// 初回凍結と、UPDATE で行が無かった後に別の書き手が凍結した場合を扱う。
		matched, err = execPolicyWrite(ctx, tx, `
INSERT INTO recording_processing_policy (recording_id, keep_original, cm_detect, cm_detect_generation, created_at, updated_at)
SELECT r.id, 'always', LAST_INSERT_ID(1), 1, ?, ?
FROM recordings r
WHERE r.id = ? AND `+presentCMOriginalSQL+`
ON DUPLICATE KEY UPDATE
    cm_detect_generation = recording_processing_policy.cm_detect_generation + IF(recording_processing_policy.cm_detect, 0, 1),
    cm_detect = LAST_INSERT_ID(1), updated_at = ?`, now, now, recordingID, now)
	}
	if err != nil {
		return fmt.Errorf("enabling CM detection for recording %d: %w", recordingID, err)
	}
	if !matched {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrOriginalUnavailable)
	}
	return nil
}

// execPolicyWrite は条件付き DML が LAST_INSERT_ID(1) を評価したかを同じ接続で読む。
// RunEncodePolicy の「policy update is atomic and returns typed precondition errors」で同値再送を、
// その中の「frozen policy without desired profiles rejects until_encoded」で保持の条件不成立を検査する。
func execPolicyWrite(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	if _, err := tx.ExecContext(ctx, "SELECT LAST_INSERT_ID(0)"); err != nil {
		return false, fmt.Errorf("resetting policy write marker: %w", err)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return false, err
	}
	var matched bool
	if err := tx.QueryRowContext(ctx, "SELECT LAST_INSERT_ID()").Scan(&matched); err != nil {
		return false, fmt.Errorf("reading policy write marker: %w", err)
	}
	return matched, nil
}

func encodeRecordingPurged(ctx context.Context, tx *sql.Tx, recordingID int64) (bool, error) {
	var purged bool
	err := tx.QueryRowContext(ctx, "SELECT purged_at IS NOT NULL FROM recordings WHERE id = ?", recordingID).Scan(&purged)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if err != nil {
		return false, fmt.Errorf("loading recording %d: %w", recordingID, err)
	}
	return purged, nil
}

func (s *Media) requireEncodeOriginal(ctx context.Context, tx *sql.Tx, recordingID int64) error {
	_, err := s.activeOriginalIDTx(ctx, tx, recordingID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrOriginalUnavailable)
	}
	if err != nil {
		return fmt.Errorf("loading active original for recording %d: %w", recordingID, err)
	}
	return nil
}

// lockOrFreezeEncodePolicy は方針行を FOR UPDATE で取り、無ければ凍結してから取り直す。
// MySQL 9.7.4 では upsert が重複行より先に外部キーの親である録画行を共有ロックするため、
// 方針行があっても upsert から始めると録画行で待つ。
// RunEncodePolicy の「append validates after a concurrent removal commits」で方針行での待機を検査する。
func lockOrFreezeEncodePolicy(ctx context.Context, tx *sql.Tx, recordingID int64) ([]string, error) {
	const lockPolicy = `
SELECT encode_profiles FROM recording_processing_policy WHERE recording_id = ? FOR UPDATE`
	var raw []byte
	err := tx.QueryRowContext(ctx, lockPolicy, recordingID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_processing_policy (recording_id, keep_original)
VALUES (?, 'always') ON DUPLICATE KEY UPDATE recording_id = recording_id`, recordingID); err != nil {
			return nil, fmt.Errorf("freezing encode policy for recording %d: %w", recordingID, err)
		}
		err = tx.QueryRowContext(ctx, lockPolicy, recordingID).Scan(&raw)
	}
	if err != nil {
		return nil, fmt.Errorf("locking encode policy for recording %d: %w", recordingID, err)
	}
	var desired []string
	if err := json.Unmarshal(raw, &desired); err != nil {
		return nil, fmt.Errorf("decoding desired profiles for recording %d: %w", recordingID, err)
	}
	return desired, nil
}

func deleteFailedEncodeAttempts(ctx context.Context, tx *sql.Tx, recordingID int64, profilesJSON string) error {
	if _, err := tx.ExecContext(ctx, `
DELETE FROM recording_encode_attempts
WHERE recording_id = ? AND state = 'failed' AND JSON_CONTAINS(?, JSON_QUOTE(profile))`,
		recordingID, profilesJSON); err != nil {
		return fmt.Errorf("resetting failed encode attempts for recording %d: %w", recordingID, err)
	}
	return nil
}

// encodeProfileDesiredForShare は作り直し・公開で必要な desired の判定を方針の共有ロック下で行う。
func encodeProfileDesiredForShare(ctx context.Context, tx *sql.Tx, recordingID int64, profile string) (bool, error) {
	var desired bool
	err := tx.QueryRowContext(ctx, `
SELECT JSON_CONTAINS(encode_profiles, JSON_QUOTE(?)) FROM recording_processing_policy
WHERE recording_id = ? FOR SHARE`, profile, recordingID).Scan(&desired)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("loading desired profiles for recording %d: %w", recordingID, err)
	}
	return desired, nil
}
