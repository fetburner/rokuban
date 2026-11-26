package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/mysqldb"
	"github.com/fetburner/rokuban/internal/store"
)

var (
	_ store.MediaPublish          = (*Media)(nil)
	_ store.DerivedMediaPublisher = (*Media)(nil)
	_ store.PendingCopyReclaim    = (*Media)(nil)
)

// CommitOriginal は原本・進捗の削除・凍結方針を一つの tx で確定する。
// 同じ rel_path の原本が既にあれば ErrConflict を返し、publish は呼ばない。
// publish が失敗するか commit が失敗すると、原本の行・方針・進捗の削除はすべて残らない。
func (s *Media) CommitOriginal(ctx context.Context, in store.CommitOriginalInput, resolvePolicy store.ResolveEncodePolicy, publish func() error) error {
	if resolvePolicy == nil {
		return errors.New("resolving encode policy: resolver is nil")
	}
	if publish == nil {
		return errors.New("publishing original: publisher is nil")
	}
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if in.WriterGeneration > 0 {
			if in.WriterOwner == ([16]byte{}) {
				return store.ErrIngestCheckpointLeaseLost
			}
			if err := lockLeasedCheckpoint(ctx, tx, in.RecordingID, in.WriterOwner, in.WriterGeneration); err != nil {
				return err
			}
		}
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO media_assets (recording_id, kind, size_bytes, sha256, ingest_generation)
SELECT p.recording_id, 'original', ?, ?, p.temp_generation
FROM recording_ingest_progress p
WHERE p.recording_id = ?`, in.SizeBytes, nullBytes(in.SHA256), in.RecordingID)
		if err != nil {
			return fmt.Errorf("inserting original media asset: %w", translateError(err))
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("inserting original media asset: no ingest progress row for recording %d (the original copies its generation from it)", in.RecordingID)
		}
		assetID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("reading original media asset ID: %w", err)
		}
		if err := recordCopyActive(ctx, tx, now, assetID, s.storeName, in.RelPath, 0); err != nil {
			return fmt.Errorf("recording original media asset copy: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recording_ingest_progress WHERE recording_id = ?`, in.RecordingID); err != nil {
			return fmt.Errorf("clearing ingest progress for recording %d: %w", in.RecordingID, err)
		}
		if in.WriterGeneration > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM recording_ingest_checkpoints WHERE recording_id = ?`, in.RecordingID); err != nil {
				return fmt.Errorf("clearing ingest checkpoint for recording %d: %w", in.RecordingID, err)
			}
		}

		policyInputs, err := encodePolicyInputsTx(ctx, tx, in.RecordingID)
		if err != nil {
			return err
		}
		policy, err := resolvePolicy(policyInputs)
		if err != nil {
			return fmt.Errorf("resolving encode policy for recording %d: %w", in.RecordingID, err)
		}
		if err := freezeEncodePolicy(ctx, tx, in.RecordingID, policy); err != nil {
			return err
		}
		if err := publish(); err != nil {
			return fmt.Errorf("publishing original file for recording %d: %w", in.RecordingID, err)
		}
		return nil
	})
}

// encodePolicyInputsTx は原本の凍結方針の入力を読む。予約は同じ局・番組の event に紐づくものを使う。
func encodePolicyInputsTx(ctx context.Context, tx *sql.Tx, recordingID int64) (store.EncodePolicyInputs, error) {
	in := store.EncodePolicyInputs{RecordingID: recordingID}
	err := tx.QueryRowContext(ctx, `
SELECT source, site, network_id, service_id, event_id FROM recordings WHERE id = ?`, recordingID).
		Scan(&in.Source, &in.Site, &in.NetworkID, &in.ServiceID, &in.EventID)
	if err != nil {
		return store.EncodePolicyInputs{}, mediaQueryError(err, fmt.Sprintf("loading recording %d for encode policy snapshot", recordingID))
	}
	var (
		programID int64
		base      []byte
		overrides []byte
		intent    sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
SELECT r.program_id, r.base, o.overrides, i.action
FROM program_snapshots ps
JOIN reservations r ON r.site = ps.site AND r.program_id = ps.program_id
LEFT JOIN program_intents i ON i.site = r.site AND i.program_id = r.program_id
LEFT JOIN program_overrides o ON o.site = r.site AND o.program_id = r.program_id
WHERE ps.site = ? AND ps.network_id = ? AND ps.service_id = ? AND ps.event_id = ?`,
		in.Site, in.NetworkID, in.ServiceID, in.EventID).Scan(&programID, &base, &overrides, &intent)
	if err == nil {
		res := &store.ReservationPolicyInputs{ProgramID: programID, Base: base, Overrides: overrides}
		if intent.Valid {
			action := intent.String
			res.IntentAction = &action
		}
		in.Reservation = res
	} else if !errors.Is(err, sql.ErrNoRows) {
		return store.EncodePolicyInputs{}, fmt.Errorf("loading reservation encode policy for recording %d: %w", recordingID, err)
	}
	return in, nil
}

// freezeEncodePolicy は録画の凍結方針を書く。行が無ければ作り、有れば同じ値で書き換える。
func freezeEncodePolicy(ctx context.Context, tx *sql.Tx, recordingID int64, policy store.EncodePolicySnapshot) error {
	profiles, err := json.Marshal(nonNilProfiles(policy.EncodeProfiles))
	if err != nil {
		return fmt.Errorf("encoding encode profiles for recording %d: %w", recordingID, err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_processing_policy (recording_id, keep_original, encode_profiles, cm_detect)
VALUES (?, ?, CAST(? AS JSON), ?) AS incoming
ON DUPLICATE KEY UPDATE keep_original = incoming.keep_original,
    encode_profiles = incoming.encode_profiles, cm_detect = incoming.cm_detect`,
		recordingID, policy.KeepOriginal, string(profiles), policy.CMDetect); err != nil {
		return fmt.Errorf("freezing encode policy for recording %d: %w", recordingID, translateError(err))
	}
	return nil
}

// recordCopyActive は store のコピーを active で書く（同じ資産・世代の生きた行は created_at を変えない）。
// 別の資産か世代が同じ置き場・パスを使うと 0 行で、ErrConflict を返す。
func recordCopyActive(ctx context.Context, tx *sql.Tx, now time.Time, assetID int64, storeName, relPath string, generation int64) error {
	n, err := upsertActiveMediaAssetCopy(ctx, tx, now, assetID, storeName, relPath, generation)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("upserting media asset copy for asset %d store %q path %q generation %d affected %d rows: %w", assetID, storeName, relPath, generation, n, store.ErrConflict)
	}
	return nil
}

// upsertActiveMediaAssetCopy は active のコピー行を書く。影響行数は pgstore の ON CONFLICT ... WHERE と同じ意味で返す。
// 生きた行が同じ資産・世代ならその行を残して 1、別の資産・世代なら 0。
// 一意キーが 2 つあるので ON DUPLICATE KEY UPDATE は使わず、(store, rel_path_key) の行を FOR UPDATE で読んで分岐する。
func upsertActiveMediaAssetCopy(ctx context.Context, tx *sql.Tx, now time.Time, assetID int64, storeName, relPath string, generation int64) (int64, error) {
	var existingID, existingAsset, existingGeneration int64
	var existingState string
	err := tx.QueryRowContext(ctx, `
SELECT id, media_asset_id, generation, state FROM media_asset_copies
WHERE store = ? AND rel_path_key = UNHEX(SHA2(?, 256)) FOR UPDATE`, storeName, relPath).
		Scan(&existingID, &existingAsset, &existingGeneration, &existingState)
	if err == nil {
		if existingState != "deleted" && existingAsset == assetID && existingGeneration == generation {
			return 1, nil
		}
		return 0, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("reading media asset copy at %q: %w", relPath, err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at, deleted_at)
VALUES (?, ?, ?, ?, 'active', ?, NULL)`, assetID, storeName, relPath, generation, now); err != nil {
		return 0, fmt.Errorf("inserting media asset copy at %q: %w", relPath, translateError(err))
	}
	return 1, nil
}

// lockRecordingFlags は録画の行を FOR UPDATE で取り、ごみ箱と purge の状態を返す。録画が無ければ ErrNotFound。
// 録画の行に触れる最初の文として呼ぶ（docs の直列化の規則）。
func lockRecordingFlags(ctx context.Context, tx *sql.Tx, recordingID int64) (trashed, purged bool, err error) {
	err = tx.QueryRowContext(ctx, `
SELECT deleted_at IS NOT NULL, purged_at IS NOT NULL FROM recordings WHERE id = ? FOR UPDATE`, recordingID).Scan(&trashed, &purged)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if err != nil {
		return false, false, fmt.Errorf("locking recording %d: %w", recordingID, err)
	}
	return trashed, purged, nil
}

// upsertDerivedMediaAsset は派生物の論理行を公開する。行の世代が新しければ stale を true にして、何も書かない。
// 同じ世代か新しい世代なら書き換える。pgstore の UpsertDerivedMediaAsset の 0 行と同じ判定。
func upsertDerivedMediaAsset(ctx context.Context, tx *sql.Tx, now time.Time, recordingID int64, kind string, profile *string, sizeBytes, generation int64, sha []byte) (int64, bool, error) {
	profileKey := ""
	if profile != nil {
		profileKey = *profile
	}
	var id, current int64
	err := tx.QueryRowContext(ctx, `
SELECT id, derived_generation FROM media_assets
WHERE recording_id = ? AND kind = ? AND profile_key = ? FOR UPDATE`, recordingID, kind, profileKey).Scan(&id, &current)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := tx.ExecContext(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation, sha256, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, recordingID, kind, optionalValue(profile), sizeBytes, generation, nullBytes(sha), now)
		if err != nil {
			return 0, false, fmt.Errorf("inserting %s media asset: %w", kind, translateError(err))
		}
		id, err = res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("reading %s media asset ID: %w", kind, err)
		}
		return id, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("locking %s media asset: %w", kind, err)
	}
	if generation < current {
		return id, true, nil
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE media_assets SET size_bytes = ?, derived_generation = ?, sha256 = ?, updated_at = ?
WHERE id = ?`, sizeBytes, generation, nullBytes(sha), now, id); err != nil {
		return 0, false, fmt.Errorf("updating %s media asset: %w", kind, translateError(err))
	}
	return id, false, nil
}

// staleDerivedError は stale な派生物の公開を ErrConflict に写す（thumbnail / seek_tiles には再計画が無い）。
func staleDerivedError(kind string) error {
	return fmt.Errorf("media asset %s already holds a newer derived generation: %w", kind, store.ErrConflict)
}

// encodeKeepRanges は凍結区間を JSON の配列にする。空の区間は落とし、全部空なら error。pgstore の KeepRangesParam と同じ。
func encodeKeepRanges(keep []chapters.Range) ([]byte, error) {
	out := make([]chapters.Range, 0, len(keep))
	for _, r := range keep {
		if r.EndMs <= r.StartMs {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errors.New("keep ranges are empty")
	}
	return json.Marshal(out)
}

// pendingCopyMissingIfAbsent は pending の公開を取りやめるときに、行が既に無ければ ErrPendingMediaAssetCopyMissing を返す。
func pendingCopyMissingIfAbsent(ctx context.Context, tx *sql.Tx, storeName string, pending bool, relPath string) error {
	if !pending {
		return nil
	}
	_, found, err := lockMediaAssetCopyByPhysicalPath(ctx, tx, storeName, relPath)
	if err != nil {
		return fmt.Errorf("checking pending copy %q after skipped publish: %w", relPath, err)
	}
	if found {
		return nil
	}
	return fmt.Errorf("pending copy %q has no live row: %w", relPath, store.ErrPendingMediaAssetCopyMissing)
}

// stalePublishPlanAfterPendingCopyCheck は計画が古いときの戻り値を作る。pending の行が無ければそれも含める。
func stalePublishPlanAfterPendingCopyCheck(ctx context.Context, tx *sql.Tx, storeName string, pending bool, relPath string) error {
	err := pendingCopyMissingIfAbsent(ctx, tx, storeName, pending, relPath)
	if errors.Is(err, store.ErrPendingMediaAssetCopyMissing) {
		return errors.Join(store.ErrStalePublishPlan, err)
	}
	if err != nil {
		return err
	}
	return store.ErrStalePublishPlan
}

// activatePendingCopy は commit の tx で pending を active にする。行が回収されていれば ErrStalePublishPlan を返す。
func activatePendingCopy(ctx context.Context, tx *sql.Tx, now time.Time, assetID int64, storeName, relPath string, generation int64) error {
	res, err := tx.ExecContext(ctx, `
UPDATE media_asset_copies SET state = 'active', created_at = ?, deleted_at = NULL
WHERE media_asset_id = ? AND store = ? AND rel_path_key = UNHEX(SHA2(?, 256))
  AND generation = ? AND state = 'pending'`, now, assetID, storeName, relPath, generation)
	if err != nil {
		return fmt.Errorf("activating pending copy %q: %w", relPath, translateError(err))
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	missingErr := pendingCopyMissingIfAbsent(ctx, tx, storeName, true, relPath)
	if errors.Is(missingErr, store.ErrPendingMediaAssetCopyMissing) {
		return fmt.Errorf("activating pending copy %q of asset %d generation %d: %w", relPath, assetID, generation,
			errors.Join(store.ErrStalePublishPlan, missingErr))
	}
	if missingErr != nil {
		return fmt.Errorf("activating pending copy %q of asset %d generation %d: %w", relPath, assetID, generation, missingErr)
	}
	return fmt.Errorf("pending copy %q of asset %d generation %d is no longer pending: %w", relPath, assetID, generation, store.ErrStalePublishPlan)
}

// recordCopy は公開の tx でコピーを現行にする。pending なら先に commit した行を active にし、そうでなければ active で書く。
func recordCopy(ctx context.Context, tx *sql.Tx, now time.Time, assetID int64, storeName, relPath string, generation int64, pending bool) error {
	if pending {
		return activatePendingCopy(ctx, tx, now, assetID, storeName, relPath, generation)
	}
	return recordCopyActive(ctx, tx, now, assetID, storeName, relPath, generation)
}

// PublishEncoded は試行・desired・作り直し要求・cut 区間を再確認して encoded を公開する。
// 判定の規則は pgstore の PublishEncoded と同じ（同じ純関数 store.PlanEncodedPublish を使う）。
// 版の削除・事後追加と直列化する。cut はさらにチャプター編集と直列化する（録画の行を先に取る）。
func (s *Media) PublishEncoded(ctx context.Context, in store.EncodedPublishInput, publish func() error) (bool, error) {
	if publish == nil {
		return false, errors.New("publishing encoded media: publisher is nil")
	}
	published := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		if in.Cut {
			_, purged, err := lockRecordingFlags(ctx, tx, in.RecordingID)
			if errors.Is(err, store.ErrNotFound) || (err == nil && purged) {
				return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
			}
			if err != nil {
				return fmt.Errorf("locking recording %d before encoded publish: %w", in.RecordingID, err)
			}
		}
		running, err := encodeAttemptRunning(ctx, tx, in)
		if err != nil {
			return err
		}
		if !running {
			return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
		}
		state, err := encodedPublishStateTx(ctx, tx, in)
		if err != nil {
			return err
		}
		if in.PendingCopy && in.ObservedGeneration == nil && state.EncodedAsset != nil &&
			!state.EncodedAsset.Active && state.EncodedAsset.DerivedGeneration == in.DerivedGeneration {
			// 初回の公開で、論理行はこの試行の PreparePendingMediaAssetCopy が作った。別の試行が進めた行ではない。
			in.ObservedGeneration = &state.EncodedAsset.DerivedGeneration
		}
		plan := store.PlanEncodedPublish(in, state, true)
		if plan.Stale {
			return stalePublishPlanAfterPendingCopyCheck(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
		}
		if plan.Skip {
			return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
		}

		profile := in.Profile
		id, stale, err := upsertDerivedMediaAsset(ctx, tx, now, in.RecordingID, "encoded", &profile, in.SizeBytes, in.DerivedGeneration, in.SHA256)
		if err != nil {
			return err
		}
		if stale {
			return stalePublishPlanAfterPendingCopyCheck(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
		}
		if err := recordCopy(ctx, tx, now, id, s.storeName, in.RelPath, in.DerivedGeneration, in.PendingCopy); err != nil {
			if errors.Is(err, store.ErrStalePublishPlan) {
				return err
			}
			return fmt.Errorf("recording encoded media asset copy: %w", err)
		}
		if in.Cut {
			if err := replaceCutTx(ctx, tx, id, in.KeepRanges); err != nil {
				return err
			}
		}
		if err := publish(); err != nil {
			return fmt.Errorf("publishing encoded file for recording %d profile %q: %w", in.RecordingID, in.Profile, err)
		}
		published = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return published, nil
}

// encodeAttemptRunning は、この公開の試行が現行の running の試行かを FOR UPDATE で確かめる。
func encodeAttemptRunning(ctx context.Context, tx *sql.Tx, in store.EncodedPublishInput) (bool, error) {
	var attemptState string
	var attemptCount int32
	err := tx.QueryRowContext(ctx, `
SELECT state, attempt_count FROM recording_encode_attempts
WHERE recording_id = ? AND profile = ? FOR UPDATE`, in.RecordingID, in.Profile).Scan(&attemptState, &attemptCount)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("loading current encode attempt: %w", err)
	}
	return attemptState == "running" && attemptCount == in.Attempt, nil
}

// encodedPublishStateTx は公開の計画に要る状態を、録画の行を取った後の tx の中で読む。
func encodedPublishStateTx(ctx context.Context, tx *sql.Tx, in store.EncodedPublishInput) (store.EncodedPublishState, error) {
	var state store.EncodedPublishState
	var desired sql.NullBool
	err := tx.QueryRowContext(ctx, `
SELECT JSON_CONTAINS(encode_profiles, JSON_QUOTE(?)) FROM recording_processing_policy
WHERE recording_id = ? FOR SHARE`, in.Profile, in.RecordingID).Scan(&desired)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, fmt.Errorf("loading desired encode profiles: %w", err)
	}
	state.Desired = desired.Valid && desired.Bool

	if in.HasRebuildRequest {
		var raw []byte
		err := tx.QueryRowContext(ctx, `
SELECT requested_keep FROM encoded_asset_rebuild_requests
WHERE recording_id = ? AND profile = ? FOR SHARE`, in.RecordingID, in.Profile).Scan(&raw)
		if err == nil {
			state.HasRebuildRequest = true
			state.RebuildRequestKeep, err = decodeKeepRanges(raw)
			if err != nil {
				return state, fmt.Errorf("decoding rebuild request for recording %d profile %q: %w", in.RecordingID, in.Profile, err)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return state, fmt.Errorf("loading rebuild request for recording %d profile %q: %w", in.RecordingID, in.Profile, err)
		}
	} else if in.Cut {
		state.CurrentCutKeep, state.ChapterOwned, err = currentCutKeepTx(ctx, tx, in.RecordingID)
		if err != nil {
			return state, fmt.Errorf("rechecking chapter keep before publish for recording %d: %w", in.RecordingID, err)
		}
	}

	var assetID int64
	var active bool
	var derived int64
	err = tx.QueryRowContext(ctx, `
SELECT a.id, EXISTS (SELECT 1 FROM active_media_asset_copies ac WHERE ac.media_asset_id = a.id), a.derived_generation
FROM media_assets a
WHERE a.recording_id = ? AND a.kind = 'encoded' AND a.profile = ?`, in.RecordingID, in.Profile).Scan(&assetID, &active, &derived)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("loading existing encoded asset: %w", err)
	}
	observation := store.EncodedAssetObservation{ID: assetID, Active: active, DerivedGeneration: derived}
	if in.Cut {
		observation.KeepRanges, err = assetKeepRangesTx(ctx, tx, assetID)
		if err != nil {
			return state, err
		}
	}
	state.EncodedAsset = &observation
	return state, nil
}

// replaceCutTx は凍結した区間を、同じ tx の中で書き直す（行が無ければ作る）。
func replaceCutTx(ctx context.Context, tx *sql.Tx, assetID int64, keep []chapters.Range) error {
	ranges, err := encodeKeepRanges(keep)
	if err != nil {
		return fmt.Errorf("encoding frozen keep ranges: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM media_asset_cuts WHERE media_asset_id = ?`, assetID); err != nil {
		return fmt.Errorf("clearing media_asset_cuts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO media_asset_cuts (media_asset_id, keep_ranges) VALUES (?, ?)`, assetID, ranges); err != nil {
		return fmt.Errorf("writing media_asset_cuts: %w", translateError(err))
	}
	return nil
}

// PublishThumbnail は録画とサムネイルの状態を再確認して公開する。
// 同じ録画のごみ箱移動・完全削除と直列化し、ごみ箱か削除済みの録画は公開しない。
// 置き換え（Expected あり）は、同じ録画の現行のサムネイルを FOR UPDATE で取ってから比べる。
func (s *Media) PublishThumbnail(ctx context.Context, in store.ThumbnailPublishInput, publish func() error) (bool, error) {
	if publish == nil {
		return false, errors.New("publishing thumbnail: publisher is nil")
	}
	published := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		trashed, purged, err := lockRecordingFlags(ctx, tx, in.RecordingID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
			}
			return err
		}
		if trashed || purged {
			return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
		}

		var assetID int64
		var skip bool
		if in.Expected == nil {
			assetID, skip, err = s.publishThumbnailNewTx(ctx, tx, now, in)
		} else {
			assetID, skip, err = s.publishThumbnailReplaceTx(ctx, tx, now, in)
		}
		if err != nil {
			return err
		}
		if skip {
			return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
		}
		if err := recordCopy(ctx, tx, now, assetID, s.storeName, in.RelPath, in.DerivedGeneration, in.PendingCopy); err != nil {
			return fmt.Errorf("recording thumbnail media asset copy: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO media_asset_thumbnail_seeks (media_asset_id, seek_ms) VALUES (?, ?) AS incoming
ON DUPLICATE KEY UPDATE seek_ms = incoming.seek_ms`, assetID, in.SeekMs); err != nil {
			return fmt.Errorf("upserting thumbnail seek position: %w", translateError(err))
		}
		if err := publish(); err != nil {
			return fmt.Errorf("publishing thumbnail file: %w", err)
		}
		published = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return published, nil
}

// publishThumbnailNewTx は初回のサムネイルの論理行を、原本と現行の有無を確かめてから書く。
// skip が true なら公開しない（既にあるか、原本が違う）。
func (s *Media) publishThumbnailNewTx(ctx context.Context, tx *sql.Tx, now time.Time, in store.ThumbnailPublishInput) (int64, bool, error) {
	ok, err := s.hasActiveDerivedTx(ctx, tx, in.RecordingID, "thumbnail")
	if err != nil {
		return 0, false, err
	}
	if ok {
		return 0, true, nil
	}
	originalID, err := s.activeOriginalIDTx(ctx, tx, in.RecordingID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("loading active original: %w", err)
	}
	if originalID != in.InputAssetID {
		return 0, true, nil
	}
	id, stale, err := upsertDerivedMediaAsset(ctx, tx, now, in.RecordingID, "thumbnail", nil, in.SizeBytes, in.DerivedGeneration, in.SHA256)
	if err != nil {
		return 0, false, err
	}
	if stale {
		return 0, false, staleDerivedError("thumbnail")
	}
	return id, false, nil
}

// publishThumbnailReplaceTx は現行のサムネイルの置き換えを、観測した状態と入力の両方を確かめてから書く。
// skip が true なら公開しない（観測した状態が変わった、または入力が使えない）。
func (s *Media) publishThumbnailReplaceTx(ctx context.Context, tx *sql.Tx, now time.Time, in store.ThumbnailPublishInput) (int64, bool, error) {
	observedStore := in.Expected.Store
	if observedStore == "" {
		observedStore = s.storeName
	}
	cur, err := s.lockActiveThumbnailTx(ctx, tx, in.RecordingID, observedStore)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("locking active thumbnail: %w", err)
	}
	if cur.id != in.Expected.ID || cur.relPath != in.Expected.RelPath ||
		cur.generation != in.Expected.DerivedGeneration || !sameOptionalInt64(cur.seekMs, in.Expected.SeekMs) {
		return 0, true, nil
	}
	inputStore := in.InputStore
	if inputStore == "" {
		inputStore = s.originalStore
	}
	if inputStore != s.originalStore && inputStore != s.storeName && inputStore != s.derivedStoreName() {
		return 0, false, fmt.Errorf("thumbnail input store %q is outside the original, publishing and desired stores", inputStore)
	}
	active, err := isActiveThumbnailInputTx(ctx, tx, in.InputAssetID, inputStore)
	if err != nil {
		return 0, false, fmt.Errorf("checking thumbnail input asset: %w", err)
	}
	if !active {
		return 0, true, nil
	}
	_, stale, err := upsertDerivedMediaAsset(ctx, tx, now, in.RecordingID, "thumbnail", nil, in.SizeBytes, in.DerivedGeneration, in.SHA256)
	if err != nil {
		return 0, false, err
	}
	if stale {
		return 0, false, staleDerivedError("thumbnail")
	}
	return cur.id, false, nil
}

func sameOptionalInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// PublishSeekTiles は録画とシークタイルの状態を再確認して公開する。
// 同じ録画のごみ箱移動・完全削除と直列化し、ごみ箱か削除済みの録画は公開しない。
func (s *Media) PublishSeekTiles(ctx context.Context, in store.SeekTilesPublishInput, publish func() error) (bool, error) {
	if publish == nil {
		return false, errors.New("publishing seek tiles: publisher is nil")
	}
	published := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		trashed, purged, err := lockRecordingFlags(ctx, tx, in.RecordingID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
			}
			return err
		}
		if trashed || purged {
			return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
		}
		ok, err := s.hasActiveDerivedTx(ctx, tx, in.RecordingID, "seek_tiles")
		if err != nil {
			return err
		}
		if ok {
			return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
		}
		if _, err := s.activeOriginalIDTx(ctx, tx, in.RecordingID); errors.Is(err, sql.ErrNoRows) {
			return pendingCopyMissingIfAbsent(ctx, tx, s.storeName, in.PendingCopy, in.RelPath)
		} else if err != nil {
			return fmt.Errorf("loading active original for seek tiles: %w", err)
		}
		assetID, stale, err := upsertDerivedMediaAsset(ctx, tx, now, in.RecordingID, "seek_tiles", nil, in.SizeBytes, in.DerivedGeneration, in.SHA256)
		if err != nil {
			return err
		}
		if stale {
			return staleDerivedError("seek_tiles")
		}
		if err := recordCopy(ctx, tx, now, assetID, s.storeName, in.RelPath, in.DerivedGeneration, in.PendingCopy); err != nil {
			return fmt.Errorf("recording seek tiles media asset copy: %w", err)
		}
		if err := publish(); err != nil {
			return fmt.Errorf("publishing seek tiles file: %w", err)
		}
		published = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return published, nil
}

// hasActiveDerivedTx は tx の中で、派生物の置き場（または一時置き場）に active な行があるかを返す。
func (s *Media) hasActiveDerivedTx(ctx context.Context, tx *sql.Tx, recordingID int64, kind string) (bool, error) {
	storeCond, storeArgs := s.derivedStoreCondition("copy.store")
	args := append([]any{recordingID, kind}, storeArgs...)
	var found int
	err := tx.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM media_assets asset
    JOIN media_asset_copies copy ON copy.media_asset_id = asset.id
    WHERE asset.recording_id = ? AND asset.kind = ?
      AND copy.state = 'active' AND copy.generation = asset.derived_generation
      AND `+storeCond+`
)`, args...).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("checking active %s: %w", kind, err)
	}
	return found == 1, nil
}

// activeOriginalIDTx は録画の active な原本の ID を、束縛された原本の置き場から読む（sql.ErrNoRows なら無い）。
func (s *Media) activeOriginalIDTx(ctx context.Context, tx *sql.Tx, recordingID int64) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
SELECT a.id
FROM media_assets a
`+preferredActiveCopyLateral+`
WHERE a.recording_id = ? AND a.kind = 'original'`,
		nullableString(s.originalStore), nullableString(s.originalStore), recordingID).Scan(&id)
	return id, err
}

// thumbnailRow は FOR UPDATE で読んだ現行のサムネイル。
type thumbnailRow struct {
	id         int64
	relPath    string
	generation int64
	seekMs     *int64
}

// lockActiveThumbnailTx は置き換えの比較のために、現行のサムネイルの論理行を FOR UPDATE で取る。
func (s *Media) lockActiveThumbnailTx(ctx context.Context, tx *sql.Tx, recordingID int64, storeName string) (thumbnailRow, error) {
	var row thumbnailRow
	var seek sql.NullInt64
	err := tx.QueryRowContext(ctx, `
SELECT a.id, c.rel_path, a.derived_generation, seeks.seek_ms
FROM media_assets a
`+preferredActiveCopyLateral+`
LEFT JOIN media_asset_thumbnail_seeks seeks ON seeks.media_asset_id = a.id
WHERE a.recording_id = ? AND a.kind = 'thumbnail'
FOR UPDATE OF a`, storeName, storeName, recordingID).Scan(&row.id, &row.relPath, &row.generation, &seek)
	if err != nil {
		return thumbnailRow{}, err
	}
	if seek.Valid {
		row.seekMs = &seek.Int64
	}
	return row, nil
}

// isActiveThumbnailInputTx は生成に使ったファイルがまだ使えるかを、tx の中で確かめる。
func isActiveThumbnailInputTx(ctx context.Context, tx *sql.Tx, assetID int64, storeName string) (bool, error) {
	var found int
	err := tx.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM media_assets a
    WHERE a.id = ? AND a.kind IN ('original', 'encoded')
      AND EXISTS (
          SELECT 1 FROM media_asset_copies copy
          WHERE copy.media_asset_id = a.id AND copy.state = 'active' AND copy.generation = a.derived_generation
            AND (? = '' OR copy.store = ?)
            AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = copy.id)
      )
)`, assetID, storeName, storeName).Scan(&found)
	return found == 1, err
}

// generationFromRelPath は rel_path の末尾の `.g{n}.` / `.u{n}.` から世代を読む。読めなければ 0。
// pgstore の media_asset_generation_from_rel_path と同じ正規表現と範囲（int8 に収まる 19 桁まで）。
var generationPattern = regexp.MustCompile(`\.[gu]([1-9][0-9]*)\.[^./]+$`)

func generationFromRelPath(relPath string) int64 {
	m := generationPattern.FindStringSubmatch(relPath)
	if m == nil {
		return 0
	}
	g := m[1]
	if len(g) > 19 || (len(g) == 19 && g > "9223372036854775807") {
		return 0
	}
	n, err := strconv.ParseInt(g, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// PreparePendingMediaAssetCopy は派生物を Put する前に、論理行と pending のコピー行を commit する。
// 同じ論理資産への予約と公開は、論理行を FOR UPDATE で取って直列化する。無い行の初回作成が並んだときは、
// 一意違反の後に先に作られた行を読み直す。
// pending は現行のコピーではない。置き換えは新しい世代の行を足すだけで、現行の active と世代を書き換えない。
func (s *Media) PreparePendingMediaAssetCopy(ctx context.Context, in store.PendingMediaAssetCopy) error {
	if in.Kind != store.AssetKindEncoded && in.Kind != store.AssetKindThumbnail && in.Kind != store.AssetKindSeekTiles {
		return fmt.Errorf("pending copy kind %q is not a derived media asset", in.Kind)
	}
	if in.RecordingID <= 0 || in.SizeBytes < 0 || in.DerivedGeneration < 0 {
		return errors.New("pending copy requires a positive recording ID and non-negative size and generation")
	}
	if _, err := mediapath.Resolve("/rokuban-pending-copy", in.RelPath); err != nil {
		return fmt.Errorf("validating pending media asset path: %w", err)
	}
	if (in.Kind == store.AssetKindEncoded) != (in.Profile != nil && *in.Profile != "") {
		return errors.New("encoded pending copy requires a profile; other derived kinds must not have one")
	}
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		profileKey := ""
		if in.Profile != nil {
			profileKey = *in.Profile
		}
		// 論理行を取る。同じ論理資産への予約と公開を直列化し、世代の最大値の読みと挿入の間に別の予約が入らないようにする。
		var assetID int64
		err := tx.QueryRowContext(ctx, `
SELECT id FROM media_assets WHERE recording_id = ? AND kind = ? AND profile_key = ? FOR UPDATE`,
			in.RecordingID, in.Kind, profileKey).Scan(&assetID)
		if errors.Is(err, sql.ErrNoRows) {
			now, err := transactionTime(ctx, tx)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation, updated_at)
VALUES (?, ?, ?, ?, ?, ?)`, in.RecordingID, in.Kind, optionalValue(in.Profile), in.SizeBytes, in.DerivedGeneration, now)
			if err != nil && mysqldb.IsUniqueViolation(err) {
				// READ COMMITTED では、無い行への FOR UPDATE はギャップロックを取らないので、同じ論理資産の初回予約が
				// 並ぶと後の INSERT が一意違反になる。先に作られた行を読み直して続ける（pgstore の ON CONFLICT DO NOTHING の後の読み直し）。
				// MySQL は文が失敗しても tx が続く。
				if err := tx.QueryRowContext(ctx, `SELECT id FROM media_assets WHERE recording_id = ? AND kind = ? AND profile_key = ? FOR UPDATE`, in.RecordingID, in.Kind, profileKey).Scan(&assetID); err != nil {
					return fmt.Errorf("reading logical media asset reserved concurrently: %w", err)
				}
			} else {
				if err != nil {
					return fmt.Errorf("reserving logical media asset for pending copy: %w", translateError(err))
				}
				assetID, err = res.LastInsertId()
				if err != nil {
					return fmt.Errorf("reading logical media asset ID: %w", err)
				}
			}
		} else if err != nil {
			return fmt.Errorf("reserving logical media asset for pending copy: %w", err)
		}
		var maxGeneration int64
		if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(generation), -1) FROM media_asset_copies WHERE media_asset_id = ?`, assetID).Scan(&maxGeneration); err != nil {
			return fmt.Errorf("checking previously used generations: %w", err)
		}
		if in.DerivedGeneration < maxGeneration || (in.ReserveNewGeneration && in.DerivedGeneration == maxGeneration) {
			return fmt.Errorf("generation %d is not newer than used generation %d: %w",
				in.DerivedGeneration, maxGeneration, store.ErrGenerationAdvanced)
		}

		var existingAsset, existingGeneration int64
		var existingState string
		err = tx.QueryRowContext(ctx, `
SELECT media_asset_id, generation, state FROM media_asset_copies
WHERE store = ? AND rel_path_key = UNHEX(SHA2(?, 256)) FOR UPDATE`, s.storeName, in.RelPath).
			Scan(&existingAsset, &existingGeneration, &existingState)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state)
VALUES (?, ?, ?, ?, 'pending')`, assetID, s.storeName, in.RelPath, in.DerivedGeneration); err != nil {
				return fmt.Errorf("creating pending media asset copy: %w", translateError(err))
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking existing pending media asset copy: %w", err)
		}
		if existingState == "deleted" {
			// 墓石が (store, rel_path) を予約している。
			return fmt.Errorf("media asset copy key %q is reserved by a tombstone: %w", in.RelPath, store.ErrConflict)
		}
		if existingAsset != assetID || existingGeneration != in.DerivedGeneration || existingState != "pending" {
			return fmt.Errorf("media asset copy key %q is already assigned to another asset, generation, or lifecycle state: %w", in.RelPath, store.ErrConflict)
		}
		return nil
	})
}

// HasLiveMediaAssetCopy はこの置き場に未削除の rel_path があるかを返す。
func (s *Media) HasLiveMediaAssetCopy(ctx context.Context, relPath string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM media_asset_copies c
    WHERE c.store = ? AND c.rel_path_key = UNHEX(SHA2(?, 256)) AND c.state <> 'deleted'
)`, s.storeName, relPath).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("checking live media asset copy %q: %w", relPath, err)
	}
	return found == 1, nil
}

// ListAgedPendingMediaAssetCopies は、before より前に予約された pending の派生物コピーを返す。
func (s *Media) ListAgedPendingMediaAssetCopies(ctx context.Context, before time.Time, limit int32) ([]store.MediaDeleteAsset, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT copy.id, asset.id, asset.recording_id, copy.rel_path, asset.size_bytes, asset.kind, copy.generation
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.store = ? AND copy.state = 'pending' AND asset.kind <> 'original'
  AND copy.created_at < ?
ORDER BY copy.id
LIMIT ?`, s.storeName, before, limit)
	if err != nil {
		return nil, fmt.Errorf("listing aged pending media asset copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.MediaDeleteAsset{}
	for rows.Next() {
		var a store.MediaDeleteAsset
		if err := rows.Scan(&a.ID, &a.MediaAssetID, &a.RecordingID, &a.RelPath, &a.SizeBytes, &a.Kind, &a.Generation); err != nil {
			return nil, fmt.Errorf("scanning aged pending media asset copies: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading aged pending media asset copies: %w", err)
	}
	return out, nil
}

// ReclaimPendingMediaAssetCopy は pending の行をロックしたまま deleteObject を呼び、deleted にする。
// 転送の所有の証拠がある場合は、所有しているパスだけを deleteObject に渡し、転送の記録を消す。
func (s *Media) ReclaimPendingMediaAssetCopy(ctx context.Context, copyID int64, before time.Time, deleteObject func(ownedRelPaths []string) error) (bool, error) {
	if deleteObject == nil {
		return false, errors.New("reclaiming pending media asset copy: deleter is nil")
	}
	reclaimed := false
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		var assetID int64
		err := tx.QueryRowContext(ctx, `
SELECT copy.media_asset_id
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.id = ? AND copy.store = ? AND copy.state = 'pending' AND copy.created_at < ?
FOR UPDATE OF asset, copy`, copyID, s.storeName, before).Scan(&assetID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("locking pending media asset copy %d: %w", copyID, err)
		}
		var ownedRelPaths []string
		if slices.Contains(s.derivedStagingStores(), s.storeName) {
			var hasEvidence int
			if err := tx.QueryRowContext(ctx, `
SELECT (EXISTS (
    SELECT 1 FROM media_asset_copies c
    JOIN media_asset_copies sibling
      ON sibling.media_asset_id = c.media_asset_id AND sibling.generation = c.generation AND sibling.id <> c.id
    WHERE c.id = ?
) OR EXISTS (SELECT 1 FROM media_asset_copy_transfer_progress WHERE copy_id = ?)
  OR EXISTS (SELECT 1 FROM media_asset_transfer_attempts WHERE copy_id = ?)
  OR EXISTS (SELECT 1 FROM media_asset_transfer_objects WHERE copy_id = ?))`, copyID, copyID, copyID, copyID).Scan(&hasEvidence); err != nil {
				return fmt.Errorf("checking transfer evidence for pending copy %d: %w", copyID, err)
			}
			if hasEvidence == 1 {
				rows, err := tx.QueryContext(ctx, `SELECT rel_path FROM media_asset_transfer_objects WHERE copy_id = ? ORDER BY rel_path`, copyID)
				if err != nil {
					return fmt.Errorf("listing owned objects for pending copy %d: %w", copyID, err)
				}
				owned, err := scanStrings(rows)
				if err != nil {
					return fmt.Errorf("listing owned objects for pending copy %d: %w", copyID, err)
				}
				ownedRelPaths = append([]string{}, owned...)
			}
		}
		if err := deleteObject(ownedRelPaths); err != nil {
			return fmt.Errorf("deleting object of pending media asset copy %d: %w", copyID, err)
		}
		res, err := tx.ExecContext(ctx, `
UPDATE media_asset_copies SET state = 'deleted', deleted_at = COALESCE(deleted_at, CURRENT_TIMESTAMP(6))
WHERE id = ? AND store = ? AND state = 'pending'`, copyID, s.storeName)
		if err != nil {
			return fmt.Errorf("marking pending media asset copy %d deleted: %w", copyID, translateError(err))
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("marking pending media asset copy %d deleted updated %d rows, want one", copyID, n)
		}
		if ownedRelPaths != nil {
			for _, stmt := range []string{
				`DELETE FROM media_asset_transfer_attempts WHERE copy_id = ?`,
				`DELETE FROM media_asset_transfer_objects WHERE copy_id = ?`,
				`DELETE FROM media_asset_copy_transfer_progress WHERE copy_id = ?`,
			} {
				if _, err := tx.ExecContext(ctx, stmt, copyID); err != nil {
					return fmt.Errorf("clearing obsolete transfer state for copy %d: %w", copyID, err)
				}
			}
		}
		reclaimed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return reclaimed, nil
}

// scanStrings は 1 列の文字列の行を読んで閉じる。
func scanStrings(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
