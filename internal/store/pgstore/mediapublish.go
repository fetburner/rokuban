package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/serverevent"
	"github.com/fetburner/rokuban/internal/store"
)

var (
	_ store.MediaPublish           = (*Media)(nil)
	_ store.EncodeProgressReporter = (*Media)(nil)
)

// CommitOriginal は原本・進捗の削除・凍結方針を一つの tx で確定する。
// 同じ rel_path の原本が既にあれば ErrConflict を返し、publish は呼ばない。
// 衝突は行の挿入時の一意違反で判定するので、並行する別の ingest とも直列化される。
func (s *Media) CommitOriginal(
	ctx context.Context,
	in store.CommitOriginalInput,
	resolvePolicy store.ResolveEncodePolicy,
	publish func() error,
) error {
	if resolvePolicy == nil {
		return errors.New("resolving encode policy: resolver is nil")
	}
	if publish == nil {
		return errors.New("publishing original: publisher is nil")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning original media commit: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if _, err := q.CreateMediaAsset(ctx, sqlcgen.CreateMediaAssetParams{
		RecordingID: in.RecordingID,
		Kind:        store.AssetKindOriginal,
		RelPath:     in.RelPath,
		SizeBytes:   in.SizeBytes,
	}); err != nil {
		return fmt.Errorf("inserting original media asset: %w", mapDatabaseError(err))
	}
	if err := q.DeleteRecordingIngestProgress(ctx, in.RecordingID); err != nil {
		return fmt.Errorf("clearing ingest progress for recording %d: %w", in.RecordingID, err)
	}

	recording, err := q.GetRecordingByID(ctx, in.RecordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = fmt.Errorf("recording %d: %w", in.RecordingID, store.ErrNotFound)
		}
		return fmt.Errorf("loading recording for encode policy snapshot: %w", err)
	}
	policyInputs := store.EncodePolicyInputs{
		RecordingID: in.RecordingID,
		Source:      recording.Source, Site: recording.Site,
		NetworkID: recording.NetworkID, ServiceID: recording.ServiceID, EventID: recording.EventID,
	}
	reservationRow, err := q.GetReservationEncodePolicyByEvent(ctx, sqlcgen.GetReservationEncodePolicyByEventParams{
		Site: recording.Site, NetworkID: recording.NetworkID,
		ServiceID: recording.ServiceID, EventID: recording.EventID,
	})
	if err == nil {
		policyInputs.Reservation = &store.ReservationPolicyInputs{
			ProgramID:    reservationRow.Reservation.ProgramID,
			Base:         reservationRow.Reservation.Base,
			Overrides:    reservationRow.Overrides,
			IntentAction: reservationRow.IntentAction,
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("loading reservation encode policy for recording %d: %w", in.RecordingID, err)
	}

	policy, err := resolvePolicy(policyInputs)
	if err != nil {
		return fmt.Errorf("resolving encode policy for recording %d: %w", in.RecordingID, err)
	}
	if err := q.FreezeRecordingEncodePolicy(ctx, sqlcgen.FreezeRecordingEncodePolicyParams{
		RecordingID:    in.RecordingID,
		KeepOriginal:   policy.KeepOriginal,
		EncodeProfiles: policy.EncodeProfiles,
		CmDetect:       policy.CMDetect,
	}); err != nil {
		return fmt.Errorf("freezing encode policy for recording %d: %w", in.RecordingID, mapDatabaseError(err))
	}

	if err := publish(); err != nil {
		return fmt.Errorf("publishing original file for recording %d: %w", in.RecordingID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing original media for recording %d: %w", in.RecordingID, mapDatabaseError(err))
	}
	return nil
}

// ClearIngestProgress は録画の ingest 進捗行を削除する。行が無くても成功する。
func (s *Media) ClearIngestProgress(ctx context.Context, recordingID int64) error {
	if err := sqlcgen.New(s.pool).DeleteRecordingIngestProgress(ctx, recordingID); err != nil {
		return fmt.Errorf("clearing ingest progress for recording %d: %w", recordingID, err)
	}
	return nil
}

// PublishEncoded は試行・desired・作り直し要求・cut 区間を再確認して encoded を公開する。
// 版の削除・事後追加と直列化する（cut でない版も）。cut はさらにチャプター編集・作り直し要求とも直列化する。
// cut は録画を先にロックし、試行・desired・要求の順に読む。
// 古い計画なら ErrStalePublishPlan を返す。
//
// 再確認の判定は次のとおりである。
//   - desired に無い profile は公開しない。版を外す tx と直列化して読む。
//   - 作り直し要求に由来するジョブは、押した時点の keep が利用者の確認なので、
//     要求行の keep と一致する限り公開する。チャプターの現在値とは照合しない。
//     押し直しで要求が上書きされていれば公開しない。
//   - 要求に由来しない初回の cut は、エンコード中にチャプターが編集されたら公開しない。
//     編集前の区間で desired を満たすと until_encoded の原本が消え、編集した内容を描けなくなる。
//   - 要求に由来しないジョブは active な cut 版を置き換えない。
func (s *Media) PublishEncoded(ctx context.Context, in store.EncodedPublishInput, publish func() error) (bool, error) {
	if publish == nil {
		return false, errors.New("publishing encoded media: publisher is nil")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning encoded asset commit: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if in.Cut {
		// cut の公開はごみ箱の録画にも許す。消去済みと行なしだけ公開しない。
		locked, err := lockRecordingState(ctx, q, in.RecordingID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && locked.IsPurged) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("locking recording %d before encoded publish: %w", in.RecordingID, err)
		}
	}

	attempt, err := q.GetRecordingEncodeAttemptForUpdate(ctx, sqlcgen.GetRecordingEncodeAttemptForUpdateParams{
		RecordingID: in.RecordingID,
		Profile:     in.Profile,
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (attempt.State != "running" || attempt.AttemptCount != in.Attempt)) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("loading current encode attempt: %w", err)
	}

	desired, err := q.GetRecordingEncodeProfilesForShare(ctx, in.RecordingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("loading desired encode profiles: %w", err)
	}
	if !slices.Contains(desired, in.Profile) {
		return false, nil
	}

	if in.HasRebuildRequest {
		requested, err := q.GetEncodedAssetRebuildRequestForShare(ctx, sqlcgen.GetEncodedAssetRebuildRequestForShareParams{
			RecordingID: in.RecordingID,
			Profile:     in.Profile,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("loading rebuild request for recording %d profile %q: %w", in.RecordingID, in.Profile, err)
		}
		requestedKeep, err := KeepRangesFromMultirange(requested)
		if err != nil {
			return false, fmt.Errorf("decoding rebuild request for recording %d profile %q: %w", in.RecordingID, in.Profile, err)
		}
		if !chapters.SameRanges(requestedKeep, in.RequestedKeep) {
			return false, nil
		}
	} else if in.Cut {
		currentKeep, owned, err := currentCutKeep(ctx, q, in.RecordingID)
		if err != nil {
			return false, fmt.Errorf("rechecking chapter keep before publish for recording %d: %w", in.RecordingID, err)
		}
		if !owned || !chapters.SameRanges(currentKeep, in.KeepRanges) {
			return false, nil
		}
	}

	var existing *store.EncodedAssetObservation
	row, err := q.GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: in.RecordingID,
		Profile:     &in.Profile,
	})
	if err == nil {
		observation := store.EncodedAssetObservation{ID: row.ID, RelPath: row.RelPath, State: row.State}
		if in.Cut {
			keep, err := assetKeepRanges(ctx, q, row.ID)
			if err != nil {
				return false, err
			}
			observation.KeepRanges = keep
		}
		existing = &observation
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("loading existing encoded asset: %w", err)
	}

	plan := store.PlanEncodedPublish(existing, in.Cut, in.KeepRanges, &in.ObservedRelPath)
	if plan.Stale {
		return false, store.ErrStalePublishPlan
	}
	if plan.Skip {
		return false, nil
	}
	// 作り直し要求に由来しない cut ジョブは、active な cut 版を置き換えない。
	// 置き換えてよいのは利用者が押した作り直しだけである。
	if in.Cut && !in.HasRebuildRequest && plan.Replaces {
		return false, nil
	}

	assetID, err := q.UpsertEncodedMediaAsset(ctx, sqlcgen.UpsertEncodedMediaAssetParams{
		RecordingID: in.RecordingID,
		Profile:     &in.Profile,
		RelPath:     in.RelPath,
		SizeBytes:   in.SizeBytes,
	})
	if err != nil {
		return false, fmt.Errorf("upserting encoded media asset: %w", mapDatabaseError(err))
	}
	if in.Cut {
		ranges, err := KeepRangesParam(in.KeepRanges)
		if err != nil {
			return false, fmt.Errorf("encoding frozen keep ranges: %w", err)
		}
		if err := replaceMediaAssetCuts(ctx, q, assetID, ranges); err != nil {
			return false, err
		}
	}
	if err := publish(); err != nil {
		return false, fmt.Errorf("publishing encoded file for recording %d profile %q: %w", in.RecordingID, in.Profile, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing encoded asset for recording %d profile %q: %w", in.RecordingID, in.Profile, mapDatabaseError(err))
	}
	return true, nil
}

func currentCutKeep(ctx context.Context, q *sqlcgen.Queries, recordingID int64) ([]chapters.Range, bool, error) {
	state, err := q.GetRecordingChapterState(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return nil, false, fmt.Errorf("loading chapter state: %w", err)
	}
	if !state.Owned {
		return nil, false, nil
	}
	raw, err := q.GetRecordingChapterSpansJSON(ctx, recordingID)
	if err != nil {
		return nil, false, fmt.Errorf("loading chapter spans: %w", err)
	}
	var spans []chapters.Span
	if err := json.Unmarshal(raw, &spans); err != nil {
		return nil, false, fmt.Errorf("decoding chapter spans: %w", err)
	}
	return chapters.KeepRanges(chapters.Derive(true, spans, nil, state.ProgramDurationMs)), true, nil
}

// GetCurrentCutKeep は確認済みチャプターから現在の keep 区間を導出する。
func (s *Media) GetCurrentCutKeep(ctx context.Context, recordingID int64) ([]chapters.Range, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, false, fmt.Errorf("beginning current cut keep read for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)

	keep, owned, err := currentCutKeep(ctx, sqlcgen.New(tx), recordingID)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("committing current cut keep read for recording %d: %w", recordingID, err)
	}
	return keep, owned, nil
}

func assetKeepRanges(ctx context.Context, q *sqlcgen.Queries, assetID int64) ([]chapters.Range, error) {
	raw, err := q.GetMediaAssetKeepRangesJSON(ctx, assetID)
	if err != nil {
		return nil, fmt.Errorf("loading frozen cut ranges for media asset %d: %w", assetID, err)
	}
	var ranges []chapters.Range
	if err := json.Unmarshal(raw, &ranges); err != nil {
		return nil, fmt.Errorf("decoding frozen cut ranges for media asset %d: %w", assetID, err)
	}
	return ranges, nil
}

func replaceMediaAssetCuts(ctx context.Context, q *sqlcgen.Queries, assetID int64, ranges KeepRanges) error {
	if err := q.DeleteMediaAssetCuts(ctx, assetID); err != nil {
		return fmt.Errorf("clearing media_asset_cuts: %w", err)
	}
	if err := q.InsertMediaAssetCuts(ctx, sqlcgen.InsertMediaAssetCutsParams{
		MediaAssetID: assetID,
		KeepRanges:   ranges,
	}); err != nil {
		return fmt.Errorf("writing media_asset_cuts: %w", mapDatabaseError(err))
	}
	return nil
}

// PublishThumbnail は録画とサムネイルの状態を再確認して公開する。
// 同じ録画のごみ箱移動・完全削除と直列化し、ごみ箱か削除済みの録画は公開しない。
// 置き換えは、同じ録画への別のサムネイル公開とも直列化する。
func (s *Media) PublishThumbnail(ctx context.Context, in store.ThumbnailPublishInput, publish func() error) (bool, error) {
	if publish == nil {
		return false, errors.New("publishing thumbnail: publisher is nil")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning thumbnail commit: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	locked, err := lockRecordingState(ctx, q, in.RecordingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if locked.IsTrashed || locked.IsPurged {
		return false, nil
	}

	var assetID int64
	if in.Expected == nil {
		if _, err := q.GetActiveThumbnailMediaAssetID(ctx, in.RecordingID); err == nil {
			return false, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("checking active thumbnail: %w", err)
		}
		original, err := q.GetActiveOriginalMediaAsset(ctx, in.RecordingID)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("loading active original: %w", err)
		}
		if original.ID != in.InputAssetID {
			return false, nil
		}
		assetID, err = q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
			RecordingID: in.RecordingID, RelPath: in.RelPath, SizeBytes: in.SizeBytes,
		})
		if err != nil {
			return false, fmt.Errorf("upserting thumbnail media asset: %w", mapDatabaseError(err))
		}
	} else {
		current, err := q.LockActiveThumbnailMediaAsset(ctx, in.RecordingID)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("locking active thumbnail: %w", err)
		}
		if current.ID != in.Expected.ID || current.RelPath != in.Expected.RelPath || !sameOptionalInt64(current.SeekMs, in.Expected.SeekMs) {
			return false, nil
		}
		active, err := q.IsActiveThumbnailInput(ctx, in.InputAssetID)
		if err != nil {
			return false, fmt.Errorf("checking thumbnail input asset: %w", err)
		}
		if !active {
			return false, nil
		}
		assetID = current.ID
		if err := q.UpdateThumbnailMediaAssetPath(ctx, sqlcgen.UpdateThumbnailMediaAssetPathParams{
			RelPath: in.RelPath, SizeBytes: in.SizeBytes, ID: assetID,
		}); err != nil {
			return false, fmt.Errorf("updating thumbnail media path: %w", mapDatabaseError(err))
		}
	}
	if err := q.UpsertMediaAssetThumbnailSeek(ctx, sqlcgen.UpsertMediaAssetThumbnailSeekParams{
		MediaAssetID: assetID, SeekMs: in.SeekMs,
	}); err != nil {
		return false, fmt.Errorf("upserting thumbnail seek position: %w", err)
	}
	if err := publish(); err != nil {
		return false, fmt.Errorf("publishing thumbnail file: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing thumbnail media asset: %w", mapDatabaseError(err))
	}
	return true, nil
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning seek tiles commit: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	locked, err := lockRecordingState(ctx, q, in.RecordingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if locked.IsTrashed || locked.IsPurged {
		return false, nil
	}
	if _, err := q.GetActiveSeekTilesMediaAssetID(ctx, in.RecordingID); err == nil {
		return false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("checking active seek tiles: %w", err)
	}
	if _, err := q.GetActiveOriginalMediaAsset(ctx, in.RecordingID); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("loading active original for seek tiles: %w", err)
	}
	if _, err := q.UpsertSeekTilesMediaAsset(ctx, sqlcgen.UpsertSeekTilesMediaAssetParams{
		RecordingID: in.RecordingID, RelPath: in.RelPath, SizeBytes: in.SizeBytes,
	}); err != nil {
		return false, fmt.Errorf("upserting seek tiles media asset: %w", mapDatabaseError(err))
	}
	if err := publish(); err != nil {
		return false, fmt.Errorf("publishing seek tiles file: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("committing seek tiles media asset: %w", mapDatabaseError(err))
	}
	return true, nil
}

// RegisterInPlace は既存ファイルを指す録画とアセットの行を一つの tx で登録する。
// rel_path が最も強い identity なので、行の state を問わず既存の recording_id を再利用する。
// deleted の行は所有権だけを渡す。パスが再利用されて kind や profile が違いうるので、
// 同じ組を active に戻すか新しく作るかは UpsertInPlaceMediaAsset が決める。
func (s *Media) RegisterInPlace(ctx context.Context, in store.InPlaceInput) (store.InPlaceResult, error) {
	if len(in.Assets) == 0 {
		return store.InPlaceResult{}, errors.New("in-place registration requires at least one asset")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.InPlaceResult{}, fmt.Errorf("beginning in-place registration: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	assets := make([]*sqlcgen.GetInPlaceAssetByRelPathRow, len(in.Assets))
	var recordingID int64
	for i, asset := range in.Assets {
		row, err := q.GetInPlaceAssetByRelPath(ctx, asset.RelPath)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return store.InPlaceResult{}, fmt.Errorf("looking up in-place asset %q: %w", asset.RelPath, err)
		}
		if recordingID != 0 && recordingID != row.RecordingID {
			return store.InPlaceResult{}, fmt.Errorf("in-place assets belong to different recordings: %d and %d: %w", recordingID, row.RecordingID, store.ErrConflict)
		}
		recordingID = row.RecordingID
		if !row.Published {
			continue
		}
		if row.Kind != asset.Kind || !sameStringPointer(row.Profile, asset.Profile) {
			return store.InPlaceResult{}, fmt.Errorf("published asset %q is %s/%v, requested %s/%v: %w", asset.RelPath, row.Kind, row.Profile, asset.Kind, asset.Profile, store.ErrConflict)
		}
		published := row
		assets[i] = &published
	}

	r := in.Recording
	if recordingID == 0 {
		recordingID, err = q.UpsertInPlaceRecording(ctx, sqlcgen.UpsertInPlaceRecordingParams{
			Source: r.Source, Site: r.Site, NetworkID: r.NetworkID, ServiceID: r.ServiceID, EventID: r.EventID,
			ServiceName: r.ServiceName, ChannelType: r.ChannelType, Channel: r.Channel, Title: r.Title,
			ProgramStartAt: r.ProgramStartAt, ProgramDurationMs: r.ProgramDurationMs, Status: r.Status,
			StartedAt: r.StartedAt, EndedAt: r.EndedAt,
		})
		if err != nil {
			return store.InPlaceResult{}, fmt.Errorf("upserting in-place recording: %w", mapDatabaseError(err))
		}
	}

	result := store.InPlaceResult{RecordingID: recordingID, AssetIDs: make([]int64, len(in.Assets))}
	for i, asset := range in.Assets {
		if assets[i] != nil {
			result.AssetIDs[i] = assets[i].ID
			continue
		}
		assetID, err := q.UpsertInPlaceMediaAsset(ctx, sqlcgen.UpsertInPlaceMediaAssetParams{
			RecordingID: recordingID, Kind: asset.Kind, Profile: asset.Profile, RelPath: asset.RelPath, SizeBytes: asset.SizeBytes,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				err = errors.Join(store.ErrConflict, err)
			}
			return store.InPlaceResult{}, fmt.Errorf("upserting in-place asset %q: %w", asset.RelPath, mapDatabaseError(err))
		}
		result.AssetIDs[i] = assetID
	}
	if err := tx.Commit(ctx); err != nil {
		return store.InPlaceResult{}, fmt.Errorf("committing in-place registration: %w", mapDatabaseError(err))
	}
	return result, nil
}

// UpsertIngestProgress は転送中の進捗を tx なしで記録する。失敗は呼び出し側が警告にとどめる。
func (s *Media) UpsertIngestProgress(ctx context.Context, progress store.IngestProgress) error {
	if err := sqlcgen.New(s.pool).UpsertRecordingIngestProgress(ctx, sqlcgen.UpsertRecordingIngestProgressParams{
		RecordingID: progress.RecordingID, WrittenBytes: progress.WrittenBytes, ExpectedBytes: progress.ExpectedBytes,
	}); err != nil {
		return fmt.Errorf("upserting ingest progress for recording %d: %w", progress.RecordingID, err)
	}
	return nil
}

// ReportEncodeProgress は encode-progress 通知を PostgreSQL NOTIFY で送る。
func (s *Media) ReportEncodeProgress(ctx context.Context, progress store.EncodeProgress) error {
	payload, err := json.Marshal(serverevent.EncodeProgressEvent{
		Type:        serverevent.EncodeProgressEventType,
		RecordingID: progress.RecordingID,
		Profile:     progress.Profile,
		Progress:    progress.Progress,
	})
	if err != nil {
		return fmt.Errorf("encoding encode progress for recording %d profile %q: %w", progress.RecordingID, progress.Profile, err)
	}
	if err := sqlcgen.New(s.pool).NotifyTopic(ctx, string(payload)); err != nil {
		return fmt.Errorf("notifying encode progress for recording %d profile %q: %w", progress.RecordingID, progress.Profile, err)
	}
	return nil
}

func sameStringPointer(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
