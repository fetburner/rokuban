package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
)

// Media はメディア配信・reconcile・API・ストレージ観測の契約を PostgreSQL で実装する。
// pool を持ち、利用側の用途ごとに分けた interface を満たす。
type Media struct {
	pool          *pgxpool.Pool
	storeName     string
	originalStore string
	// derivedStore は until_encoded の原本を消してよいかを判定する、派生物の desired 置き場。
	// 空なら規則が無く、原本は消さない。コピーを読み書きする置き場（storeName）とは別の値で、
	// storeName を代わりに使ってはならない。
	derivedStore string
}

// NewMedia は PostgreSQL を使う Media を作る。storeName はこのプロセスが書く置き場の名前。
// 読み取り先の置き場を固定しないロールは "" を明示する。
// derivedStore は storage.derived で、until_encoded の原本を消してよいかの判定にだけ使う。
// 削除の判定をしない呼び出し元は "" を渡す。"" は規則が無い扱いで、原本は消えない。
func NewMedia(pool *pgxpool.Pool, storeName, derivedStore string) *Media {
	return NewMediaWithStores(pool, storeName, storeName, derivedStore)
}

// NewMediaWithStores builds a Media implementation with distinct stores for source and derived assets.
func NewMediaWithStores(pool *pgxpool.Pool, storeName, originalStore, derivedStore string) *Media {
	return &Media{pool: pool, storeName: storeName, originalStore: originalStore, derivedStore: derivedStore}
}

func (s *Media) originalStoreName() string {
	if s.originalStore != "" {
		return s.originalStore
	}
	return s.storeName
}

func (s *Media) derivedStoreName() string {
	if s.derivedStore != "" {
		return s.derivedStore
	}
	return s.storeName
}

func mediaStoreNameArg(storeName string) *string {
	if storeName == "" {
		return nil
	}
	return &storeName
}

// AdoptUnassignedStoreRows は置き場が空の観測行・進捗行をこの置き場へ割り当て、
// コピーのパスから派生世代を補正する（TestAdoptUnassignedStoreRows）。コピー行は作らない。
func (s *Media) AdoptUnassignedStoreRows(ctx context.Context) error {
	if !mediapath.ValidStoreName(s.storeName) {
		return fmt.Errorf("invalid media store name %q", s.storeName)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning media store adoption: %w", err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)
	if err := q.ReconcileDerivedGenerationFromCopyPaths(ctx); err != nil {
		return fmt.Errorf("reconciling derived media generations from copy paths: %w", err)
	}
	if err := q.AdoptUnassignedIngestTemps(ctx, s.storeName); err != nil {
		return fmt.Errorf("assigning unowned ingest temps: %w", err)
	}
	if err := q.AdoptUnassignedOrphans(ctx, s.storeName); err != nil {
		return fmt.Errorf("assigning unowned orphan observations: %w", err)
	}
	if err := q.AdoptUnassignedStorageSync(ctx, s.storeName); err != nil {
		return fmt.Errorf("assigning unowned storage observations: %w", err)
	}
	if err := q.AdoptUnassignedDeleteReconcileBreaker(ctx, s.storeName); err != nil {
		return fmt.Errorf("assigning unowned delete_reconcile breaker: %w", err)
	}
	if err := q.DeleteUnassignedDeleteReconcileBreaker(ctx); err != nil {
		return fmt.Errorf("removing unowned delete_reconcile breaker: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing media store adoption: %w", mapDatabaseError(err))
	}
	return nil
}

// HasStoreCopies はこの置き場のコピー行が DB に 1 件でもあるかを返す。
// storage adopt が、目印の無い root に目印を新しく作ってよいかの判断に使う。
func (s *Media) HasStoreCopies(ctx context.Context) (bool, error) {
	has, err := sqlcgen.New(s.pool).StoreHasMediaAssetCopies(ctx, s.storeName)
	if err != nil {
		return false, fmt.Errorf("checking media asset copies in store %q: %w", s.storeName, err)
	}
	return has, nil
}

// RequireAdoptedStoreRows は割り当て前の行（範囲が空の delete_reconcile ブレーカーを含む）や
// コピー欠落が残っていないことを確認する。
func (s *Media) RequireAdoptedStoreRows(ctx context.Context) error {
	count, err := sqlcgen.New(s.pool).CountUnassignedStorageRows(ctx)
	if err != nil {
		return fmt.Errorf("checking for unassigned media store rows: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("%d media store row(s) have no store or media asset copy (including delete_reconcile circuit breaker rows with an empty scope); run `rokuban storage adopt <store-name>` before starting workers", count)
	}
	return nil
}

var (
	_ store.MediaServing       = (*Media)(nil)
	_ store.MediaReconcile     = (*Media)(nil)
	_ store.IngestPrecheck     = (*Media)(nil)
	_ store.MediaDelete        = (*Media)(nil)
	_ store.MediaReader        = (*Media)(nil)
	_ store.TSScan             = (*Media)(nil)
	_ store.StorageObservation = (*Media)(nil)
)

func servingAsset(row sqlcgen.GetOriginalMediaAssetForServingRow) store.MediaAsset {
	return store.MediaAsset{ID: row.ID, Store: row.Store, RelPath: row.RelPath, SizeBytes: row.SizeBytes}
}

// GetOriginalMediaAssetForServing は配信可能な原本を返す。
func (s *Media) GetOriginalMediaAssetForServing(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetOriginalMediaAssetForServing(ctx, sqlcgen.GetOriginalMediaAssetForServingParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.originalStoreName()),
	})
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading original media asset for recording %d", recordingID))
	}
	return servingAsset(row), nil
}

// GetEncodedMediaAssetForServing は指定プロファイルの配信可能な encoded を返す。
func (s *Media) GetEncodedMediaAssetForServing(ctx context.Context, recordingID int64, profile string) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetEncodedMediaAssetForServing(ctx, sqlcgen.GetEncodedMediaAssetForServingParams{
		RecordingID: recordingID,
		Profile:     &profile,
		StoreName:   mediaStoreNameArg(s.derivedStoreName()),
	})
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading encoded media asset for recording %d profile %q", recordingID, profile))
	}
	return store.MediaAsset{ID: row.ID, Store: row.Store, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// GetThumbnailMediaAssetForServing は配信可能なサムネイルを返す。
func (s *Media) GetThumbnailMediaAssetForServing(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetThumbnailMediaAssetForServing(ctx, sqlcgen.GetThumbnailMediaAssetForServingParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.derivedStoreName()),
	})
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading thumbnail media asset for recording %d", recordingID))
	}
	return store.MediaAsset{ID: row.AssetID, Store: row.Store, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// GetSeekTilesMediaAssetForServing は配信可能なシークタイルを返す。
func (s *Media) GetSeekTilesMediaAssetForServing(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetSeekTilesMediaAssetForServing(ctx, sqlcgen.GetSeekTilesMediaAssetForServingParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.derivedStoreName()),
	})
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading seek tiles media asset for recording %d", recordingID))
	}
	return store.MediaAsset{ID: row.ID, Store: row.Store, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// GetActiveOriginalMediaAsset は追っかけ配信に使う active な原本を返す。
func (s *Media) GetActiveOriginalMediaAsset(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetActiveOriginalMediaAsset(ctx, sqlcgen.GetActiveOriginalMediaAssetParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.originalStoreName()),
	})
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading active original media asset for recording %d", recordingID))
	}
	return store.MediaAsset{ID: row.ID, Store: row.Store, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// GetRecordingEncodeTimes はエンコード締切の計算に使う録画時刻を返す。
func (s *Media) GetRecordingEncodeTimes(ctx context.Context, recordingID int64) (store.RecordingEncodeTimes, error) {
	row, err := sqlcgen.New(s.pool).GetRecordingEncodeTimes(ctx, recordingID)
	if err != nil {
		return store.RecordingEncodeTimes{}, mediaQueryError(err, fmt.Sprintf("loading recording times for recording %d", recordingID))
	}
	return store.RecordingEncodeTimes{
		StartedAt:         row.StartedAt,
		EndedAt:           row.EndedAt,
		ProgramDurationMs: row.ProgramDurationMs,
	}, nil
}

// GetRecordingNotifySubject はエンコード通知に載せる局と番組名を返す。
// GetRecordingByID と同じく purged の録画も返す。
func (s *Media) GetRecordingNotifySubject(ctx context.Context, recordingID int64) (store.RecordingNotifySubject, error) {
	row, err := sqlcgen.New(s.pool).GetRecordingByID(ctx, recordingID)
	if err != nil {
		return store.RecordingNotifySubject{}, mediaQueryError(err, fmt.Sprintf("loading recording notify subject for recording %d", recordingID))
	}
	return store.RecordingNotifySubject{Site: row.Site, Title: row.Title}, nil
}

// HasOriginalMediaAsset は state を問わず原本アセット行の有無を返す。
func (s *Media) HasOriginalMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	_, err := sqlcgen.New(s.pool).GetOriginalMediaAssetID(ctx, recordingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking original media asset for recording %d: %w", recordingID, err)
	}
	return true, nil
}

// GetLiveMediaAssetRecordingID は state <> 'deleted' の rel_path 所有者を返す。
func (s *Media) GetLiveMediaAssetRecordingID(ctx context.Context, relPath string) (int64, error) {
	recordingID, err := sqlcgen.New(s.pool).GetLiveMediaAssetByRelPath(ctx, sqlcgen.GetLiveMediaAssetByRelPathParams{
		RelPath: relPath, StoreName: mediaStoreNameArg(s.storeName),
	})
	if err != nil {
		return 0, mediaQueryError(err, fmt.Sprintf("loading live media asset for rel_path %q", relPath))
	}
	return recordingID, nil
}

// HasActiveThumbnailMediaAsset は active なサムネイルがあれば true を返す。
func (s *Media) HasActiveThumbnailMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	_, err := sqlcgen.New(s.pool).GetActiveThumbnailMediaAssetID(ctx, sqlcgen.GetActiveThumbnailMediaAssetIDParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.derivedStoreName()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking active thumbnail media asset for recording %d: %w", recordingID, err)
	}
	return true, nil
}

// HasActiveSeekTilesMediaAsset は active なシークタイルがあれば true を返す。
func (s *Media) HasActiveSeekTilesMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	_, err := sqlcgen.New(s.pool).GetActiveSeekTilesMediaAssetID(ctx, sqlcgen.GetActiveSeekTilesMediaAssetIDParams{
		RecordingID: recordingID, StoreName: mediaStoreNameArg(s.derivedStoreName()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking active seek tiles for recording %d: %w", recordingID, err)
	}
	return true, nil
}

// GetEncodedPublishState は公開判断に必要な状態を、ロックを取らない読み取り専用 RepeatableRead tx で読む。
func (s *Media) GetEncodedPublishState(
	ctx context.Context,
	recordingID int64,
	profile string,
	cut bool,
	includeCurrentCut bool,
) (store.EncodedPublishState, error) {
	desc := fmt.Sprintf("loading encoded publish state for recording %d profile %q", recordingID, profile)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return store.EncodedPublishState{}, fmt.Errorf("beginning %s: %w", desc, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	var state store.EncodedPublishState
	policy, err := q.GetRecordingEncodePolicy(ctx, recordingID)
	if err == nil {
		state.Desired = slices.Contains(policy.EncodeProfiles, profile)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.EncodedPublishState{}, fmt.Errorf("loading desired encode profiles for recording %d: %w", recordingID, err)
	}

	requested, err := q.GetEncodedAssetRebuildRequest(ctx, sqlcgen.GetEncodedAssetRebuildRequestParams{
		RecordingID: recordingID,
		Profile:     profile,
	})
	if err == nil {
		state.HasRebuildRequest = true
		state.RebuildRequestKeep, err = KeepRangesFromMultirange(requested)
		if err != nil {
			return store.EncodedPublishState{}, fmt.Errorf("decoding rebuild request for recording %d profile %q: %w", recordingID, profile, err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.EncodedPublishState{}, fmt.Errorf("loading rebuild request for recording %d profile %q: %w", recordingID, profile, err)
	}

	if includeCurrentCut && state.Desired {
		state.CurrentCutKeep, state.ChapterOwned, err = currentCutKeep(ctx, q, recordingID)
		if err != nil {
			return store.EncodedPublishState{}, fmt.Errorf("loading current cut state for recording %d: %w", recordingID, err)
		}
	}

	row, err := q.GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: recordingID,
		Profile:     &profile,
	})
	if err == nil {
		asset := store.EncodedAssetObservation{
			ID: row.ID, Active: row.Active, DerivedGeneration: row.DerivedGeneration,
		}
		if cut {
			asset.KeepRanges, err = assetKeepRanges(ctx, q, row.ID)
			if err != nil {
				return store.EncodedPublishState{}, err
			}
		}
		state.EncodedAsset = &asset
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.EncodedPublishState{}, fmt.Errorf("loading existing encoded asset for recording %d profile %q: %w", recordingID, profile, err)
	}
	maxGeneration, err := q.GetMaxEncodedMediaAssetCopyGeneration(ctx, sqlcgen.GetMaxEncodedMediaAssetCopyGenerationParams{
		RecordingID: recordingID,
		Profile:     &profile,
	})
	if err != nil {
		return store.EncodedPublishState{}, fmt.Errorf("loading used encoded generations for recording %d profile %q: %w", recordingID, profile, err)
	}
	if maxGeneration >= 0 {
		state.MaxCopyGeneration = &maxGeneration
	}

	if err := tx.Commit(ctx); err != nil {
		return store.EncodedPublishState{}, fmt.Errorf("committing %s: %w", desc, err)
	}
	return state, nil
}

// GetOriginalVODTarget はサイトに属する完了済み録画の配信可能な原本を返す。
func (s *Media) GetOriginalVODTarget(ctx context.Context, recordingID int64, site string) (store.OriginalVODTarget, error) {
	var target store.OriginalVODTarget
	err := s.pool.QueryRow(ctx, `SELECT a.id, c.rel_path, a.size_bytes, r.site
 FROM media_assets a
 JOIN recordings r ON r.id = a.recording_id
 JOIN LATERAL (
     SELECT copy.rel_path
     FROM preferred_active_media_asset_copy(a.id, $1::text) copy
 ) c ON true
 WHERE a.recording_id = $2 AND r.site = $3
 AND `+recordingPlaybackFinishedSQL+` AND `+activePlaybackOriginalSQL("$1")+` AND `+recordingPlaybackVisibleSQL,
		s.storeName, recordingID, site).Scan(&target.ID, &target.RelPath, &target.SizeBytes, &target.Site)
	if err != nil {
		return store.OriginalVODTarget{}, mediaQueryError(err, fmt.Sprintf("loading original VOD target for recording %d site %q", recordingID, site))
	}
	return target, nil
}

// GetRecordingHLSTarget は配信可能な原本か ingest temp と世代を返す。
func (s *Media) GetRecordingHLSTarget(ctx context.Context, recordingID int64) (store.RecordingHLSInputTarget, error) {
	var startedAt *time.Time
	var originalID, originalSize, originalGeneration pgtype.Int8
	var originalRelPath pgtype.Text
	var tempRelPath, tempStore pgtype.Text
	var tempSize, tempGeneration pgtype.Int8
	var hasOriginal bool
	err := s.pool.QueryRow(ctx, `SELECT r.started_at,
       a.id, c.rel_path, a.size_bytes, COALESCE(a.ingest_generation, 0)::bigint,
       p.temp_rel_path, p.temp_store, p.written_bytes, p.temp_generation,
       EXISTS (SELECT 1 FROM media_assets any_original
               WHERE any_original.recording_id = r.id AND any_original.kind = 'original')
FROM recordings r
LEFT JOIN media_assets a ON a.recording_id = r.id AND `+activePlaybackOriginalSQL("$2")+`
LEFT JOIN LATERAL (SELECT copy.rel_path FROM preferred_active_media_asset_copy(a.id, $2::text) copy) c ON true
LEFT JOIN recording_ingest_progress p ON p.recording_id = r.id
WHERE r.id = $1 AND `+recordingPlaybackVisibleSQL+`
  AND (a.id IS NOT NULL OR (p.recording_id IS NOT NULL AND NOT EXISTS (
      SELECT 1 FROM media_assets any_original
      WHERE any_original.recording_id = r.id AND any_original.kind = 'original'
  )))`, recordingID, s.storeName).Scan(
		&startedAt,
		&originalID, &originalRelPath, &originalSize, &originalGeneration,
		&tempRelPath, &tempStore, &tempSize, &tempGeneration,
		&hasOriginal,
	)

	if err != nil {
		return store.RecordingHLSInputTarget{}, mediaQueryError(err, fmt.Sprintf("loading recording HLS target for recording %d", recordingID))
	}
	if originalID.Valid {
		return store.RecordingHLSInputTarget{
			Kind:             store.RecordingHLSInputOriginal,
			ID:               originalID.Int64,
			RelPath:          originalRelPath.String,
			SizeBytes:        originalSize.Int64,
			IngestGeneration: originalGeneration.Int64,
			StartedAt:        startedAt,
		}, nil
	}
	if hasOriginal || !tempRelPath.Valid || !tempStore.Valid || !tempGeneration.Valid {
		return store.RecordingHLSInputTarget{}, fmt.Errorf("recording %d has no readable HLS input: %w", recordingID, store.ErrNotFound)
	}
	return store.RecordingHLSInputTarget{
		Kind:             store.RecordingHLSInputTemp,
		RelPath:          tempRelPath.String,
		StoreName:        tempStore.String,
		SizeBytes:        tempSize.Int64,
		IngestGeneration: tempGeneration.Int64,
		StartedAt:        startedAt,
	}, nil
}

// GetRecordingRemovalState は録画の削除・purge・supersede の状態を返す。
func (s *Media) GetRecordingRemovalState(ctx context.Context, recordingID int64) (store.RecordingRemovalState, error) {
	row, err := sqlcgen.New(s.pool).GetRecordingByID(ctx, recordingID)
	if err != nil {
		return store.RecordingRemovalState{}, mediaQueryError(err, fmt.Sprintf("loading recording removal state for recording %d", recordingID))
	}
	return store.RecordingRemovalState{
		DeletedAt:    row.DeletedAt,
		PurgedAt:     row.PurgedAt,
		SupersededAt: row.SupersededAt,
	}, nil
}

// GetChaseTarget は追っかけ再生に必要な mirakc record と録画状態を返す。
func (s *Media) GetChaseTarget(ctx context.Context, recordingID int64) (store.ChaseTarget, error) {
	row, err := sqlcgen.New(s.pool).GetChaseTarget(ctx, recordingID)
	if err != nil {
		return store.ChaseTarget{}, mediaQueryError(err, fmt.Sprintf("loading chase target for recording %d", recordingID))
	}
	if row.DeletedAt != nil || row.Site == "" || row.RecordID == "" {
		return store.ChaseTarget{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	return store.ChaseTarget{
		Site:             row.Site,
		RecordID:         row.RecordID,
		Status:           row.Status,
		RecordingStatus:  row.RecordingStatus,
		HasOriginalAsset: row.HasOriginalAsset,
	}, nil
}

// ListMissingEncodeProfiles は不足と満たされていない作り直しを合わせた候補をページ単位で返す。
func (s *Media) ListMissingEncodeProfiles(ctx context.Context, params store.MissingEncodeProfilesParams) ([]store.MissingEncodeProfile, error) {
	rows, err := sqlcgen.New(s.pool).ListMissingEncodeProfiles(ctx, sqlcgen.ListMissingEncodeProfilesParams{
		AfterRecordingID: params.AfterRecordingID,
		RecordingID:      params.RecordingID,
		KnownProfiles:    nonNilStrings(params.KnownProfiles),
		CutProfiles:      nonNilStrings(params.CutProfiles),
		AttemptLimit:     params.AttemptLimit,
		RowLimit:         params.RowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing missing encode profiles: %w", err)
	}
	out := make([]store.MissingEncodeProfile, 0, len(rows))
	for _, row := range rows {
		var keep []chapters.Range
		var decodeErr error
		if row.IsRebuild {
			keep, decodeErr = KeepRangesFromMultirange(row.RequestedKeep)
		}
		out = append(out, store.MissingEncodeProfile{
			RecordingID:   row.RecordingID,
			Profile:       row.Profile,
			IsRebuild:     row.IsRebuild,
			RequestedKeep: keep,
			DecodeErr:     decodeErr,
		})
	}
	return out, nil
}

// ListUnsatisfiableEncodeProfiles は現在の設定に無い desired profile と録画数を返す。
func (s *Media) ListUnsatisfiableEncodeProfiles(ctx context.Context, knownProfiles []string) ([]store.EncodeProfileCount, error) {
	rows, err := sqlcgen.New(s.pool).ListUnsatisfiableEncodeProfiles(ctx, nonNilStrings(knownProfiles))
	if err != nil {
		return nil, fmt.Errorf("listing unsatisfiable encode profiles: %w", err)
	}
	return unsatisfiableEncodeProfileCounts(rows), nil
}

// ListCutAwaitingReview は確認待ちの cut profile と録画数を返す。
func (s *Media) ListCutAwaitingReview(ctx context.Context, profiles []string) ([]store.EncodeProfileCount, error) {
	rows, err := sqlcgen.New(s.pool).ListCutAwaitingReview(ctx, nonNilStrings(profiles))
	if err != nil {
		return nil, fmt.Errorf("listing cut profiles awaiting review: %w", err)
	}
	return cutAwaitingReviewProfileCounts(rows), nil
}

// ListAwaitingReviewRecordingIDs は確認待ちの録画 ID を重複なしで返す。
func (s *Media) ListAwaitingReviewRecordingIDs(ctx context.Context, cutProfiles []string) ([]int64, error) {
	rows, err := sqlcgen.New(s.pool).ListAwaitingReviewRecordingIDs(ctx, nonNilStrings(cutProfiles))
	if err != nil {
		return nil, fmt.Errorf("listing recordings awaiting chapter review: %w", err)
	}
	return rows, nil
}

func unsatisfiableEncodeProfileCounts(rows []sqlcgen.ListUnsatisfiableEncodeProfilesRow) []store.EncodeProfileCount {
	out := make([]store.EncodeProfileCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.EncodeProfileCount{Profile: row.Profile, Recordings: row.Recordings})
	}
	return out
}

func cutAwaitingReviewProfileCounts(rows []sqlcgen.ListCutAwaitingReviewRow) []store.EncodeProfileCount {
	out := make([]store.EncodeProfileCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.EncodeProfileCount{Profile: row.Profile, Recordings: row.Recordings})
	}
	return out
}

// ListEncodedMeaningEvidence は過去の encoded と作り直し要求から意味の証拠を返す。
func (s *Media) ListEncodedMeaningEvidence(ctx context.Context) ([]store.EncodedMeaningEvidence, error) {
	rows, err := sqlcgen.New(s.pool).ListEncodedMeaningEvidence(ctx, mediaStoreNameArg(s.derivedStoreName()))
	if err != nil {
		return nil, fmt.Errorf("listing encoded meaning evidence: %w", err)
	}
	out := make([]store.EncodedMeaningEvidence, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.EncodedMeaningEvidence{
			Profile: row.Profile, RelPath: row.RelPath,
		})
	}
	return out, nil
}

// GetDerivedMediaAssetGeneration は派生アセットの最大割当済み世代を返す。
// 回収済みコピーの世代も含め、thumbnail と seek_tiles のキーを再利用しない。
func (s *Media) GetDerivedMediaAssetGeneration(ctx context.Context, recordingID int64, kind string) (int64, bool, error) {
	generation, err := sqlcgen.New(s.pool).GetDerivedMediaAssetGeneration(ctx, sqlcgen.GetDerivedMediaAssetGenerationParams{
		RecordingID: recordingID, Kind: kind,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("getting %s generation for recording %d: %w", kind, recordingID, err)
	}
	return generation, true, nil
}

// ListMissingThumbnailRecordings はサムネイルが無い録画をページ単位で返す。
func (s *Media) ListMissingThumbnailRecordings(ctx context.Context, page store.MediaReconcilePage) ([]int64, error) {
	rows, err := sqlcgen.New(s.pool).ListMissingThumbnailRecordings(ctx, sqlcgen.ListMissingThumbnailRecordingsParams{
		AfterRecordingID: page.AfterRecordingID,
		RecordingID:      page.RecordingID,
		RowLimit:         page.RowLimit,
		OriginalStore:    s.originalStoreName(),
		DerivedStore:     s.derivedStoreName(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing missing thumbnail recordings: %w", err)
	}
	return rows, nil
}

// ListThumbnailReselectCandidates は既存サムネイルの再選択候補を返す。
func (s *Media) ListThumbnailReselectCandidates(ctx context.Context, page store.MediaReconcilePage) ([]store.ThumbnailReselectCandidate, error) {
	rows, err := sqlcgen.New(s.pool).ListThumbnailReselectCandidates(ctx, sqlcgen.ListThumbnailReselectCandidatesParams{
		AfterRecordingID: page.AfterRecordingID,
		RecordingID:      page.RecordingID,
		RowLimit:         page.RowLimit,
		OriginalStore:    s.originalStoreName(),
		DerivedStore:     s.derivedStoreName(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing thumbnail reselection candidates: %w", err)
	}
	out := make([]store.ThumbnailReselectCandidate, 0, len(rows))
	for _, row := range rows {
		c := store.ThumbnailReselectCandidate{
			RecordingID:           row.RecordingID,
			Trashed:               row.Trashed,
			ProgramDurationMs:     row.ProgramDurationMs,
			Detected:              row.Detected,
			Owned:                 row.Owned,
			OriginalMediaAssetID:  row.OriginalMediaAssetID,
			OriginalStore:         row.OriginalStore,
			OriginalRelPath:       row.OriginalRelPath,
			ThumbnailMediaAssetID: &row.ThumbnailMediaAssetID,
			ThumbnailRelPath:      &row.ThumbnailRelPath,
			ThumbnailGeneration:   row.ThumbnailGeneration,
			SeekMs:                row.SeekMs,
		}
		var err error
		if c.CMRanges, c.UserSpans, err = decodeThumbnailTimeline(row.CmRanges, row.UserSpans); err != nil {
			return nil, fmt.Errorf("decoding thumbnail candidate for recording %d: %w", row.RecordingID, err)
		}
		if c.EncodedAssets, err = decodeThumbnailEncodedAssets(row.EncodedAssets); err != nil {
			return nil, fmt.Errorf("decoding thumbnail candidate for recording %d: %w", row.RecordingID, err)
		}
		out = append(out, c)
	}
	return out, nil
}

func decodeThumbnailTimeline(cmRanges, userSpans []byte) ([]chapters.Range, []chapters.Span, error) {
	var cm []chapters.Range
	if err := json.Unmarshal(cmRanges, &cm); err != nil {
		return nil, nil, err
	}
	var spans []chapters.Span
	if err := json.Unmarshal(userSpans, &spans); err != nil {
		return nil, nil, err
	}
	return cm, spans, nil
}

func decodeThumbnailEncodedAssets(raw []byte) ([]store.ThumbnailEncodedAsset, error) {
	var rows []struct {
		MediaAssetID int64            `json:"mediaAssetId"`
		Store        string           `json:"store"`
		Profile      string           `json:"profile"`
		RelPath      string           `json:"relPath"`
		Cut          bool             `json:"cut"`
		KeepRanges   []chapters.Range `json:"keepRanges"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]store.ThumbnailEncodedAsset, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.ThumbnailEncodedAsset{
			MediaAssetID: r.MediaAssetID, Store: r.Store, Profile: r.Profile, RelPath: r.RelPath,
			Cut: r.Cut, KeepRanges: r.KeepRanges,
		})
	}
	return out, nil
}

// ListMissingSeekTilesRecordings はシークタイルが無い録画をページ単位で返す。
func (s *Media) ListMissingSeekTilesRecordings(ctx context.Context, page store.MediaReconcilePage) ([]int64, error) {
	rows, err := sqlcgen.New(s.pool).ListMissingSeekTilesRecordings(ctx, sqlcgen.ListMissingSeekTilesRecordingsParams{
		AfterRecordingID: page.AfterRecordingID,
		RecordingID:      page.RecordingID,
		RowLimit:         page.RowLimit,
		OriginalStore:    s.originalStoreName(),
		DerivedStore:     s.derivedStoreName(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing missing seek tiles recordings: %w", err)
	}
	return rows, nil
}

const invalidMediaAssetRelPathQuery = `
SELECT a.id, a.kind, c.rel_path
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE c.store = $1
  AND c.state <> 'deleted'
  AND a.kind IN ('original', 'encoded')
  AND c.rel_path !~ '^sites/[^/]+/'
ORDER BY a.id
LIMIT 1
`

// FindInvalidMediaAssetRelPath は予約されたサイト名空間の外にある原本または encoded を返す。
func (s *Media) FindInvalidMediaAssetRelPath(ctx context.Context) (*store.MediaAssetRelPathViolation, error) {
	var violation store.MediaAssetRelPathViolation
	if err := s.pool.QueryRow(ctx, invalidMediaAssetRelPathQuery, s.storeName).Scan(&violation.ID, &violation.Kind, &violation.RelPath); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("checking media asset rel_path namespace: %w", err)
	}
	return &violation, nil
}

// ListEncodeQueue は現在の設定で未生成の encoded と実行状態を返す。
func (s *Media) ListEncodeQueue(ctx context.Context, params store.EncodeQueueParams) ([]store.EncodeQueueEntry, error) {
	rows, err := sqlcgen.New(s.pool).ListEncodeQueue(ctx, sqlcgen.ListEncodeQueueParams{
		KnownProfiles: nonNilStrings(params.KnownProfiles),
		CutProfiles:   nonNilStrings(params.CutProfiles),
		AttemptLimit:  params.AttemptLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing encode queue: %w", err)
	}
	out := make([]store.EncodeQueueEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.EncodeQueueEntry{RecordingID: row.RecordingID, Running: row.Running})
	}
	return out, nil
}

// ListRecordingDropStats は録画の PID 別ドロップ統計を返す。
func (s *Media) ListRecordingDropStats(ctx context.Context, recordingID int64) ([]store.RecordingDropStat, error) {
	rows, err := sqlcgen.New(s.pool).ListRecordingDropStats(ctx, recordingID)
	if err != nil {
		return nil, fmt.Errorf("listing drop stats for recording %d: %w", recordingID, err)
	}
	out := make([]store.RecordingDropStat, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.RecordingDropStat{
			Pid:       row.Pid,
			Packets:   row.Packets,
			Drops:     row.Drops,
			Errors:    row.Errors,
			Scrambled: row.Scrambled,
			PidType:   row.PidType,
		})
	}
	return out, nil
}

// ListRecordingDropPositions は録画のドロップ観測位置を PID 別に返す。
func (s *Media) ListRecordingDropPositions(ctx context.Context, recordingID int64) ([]store.RecordingDropPosition, error) {
	rows, err := sqlcgen.New(s.pool).ListRecordingDropPositions(ctx, recordingID)
	if err != nil {
		return nil, fmt.Errorf("listing drop positions for recording %d: %w", recordingID, err)
	}
	out := make([]store.RecordingDropPosition, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.RecordingDropPosition{Pid: row.Pid, ByteOffset: row.ByteOffset, ElapsedMs: row.ElapsedMs})
	}
	return out, nil
}

// DeleteStorageSyncExcept は今回の config が要求しない root の観測行を削除する。
func (s *Media) DeleteStorageSyncExcept(ctx context.Context, roots []string) error {
	if err := sqlcgen.New(s.pool).DeleteStorageSyncExcept(ctx, sqlcgen.DeleteStorageSyncExceptParams{
		Store: s.storeName, Roots: nonNilStrings(roots),
	}); err != nil {
		return fmt.Errorf("deleting stale storage roots: %w", err)
	}
	return nil
}

// UpsertStorageSync は 1 root の最新観測値を保存する。
func (s *Media) UpsertStorageSync(ctx context.Context, observation store.StorageObservationInput) error {
	if err := sqlcgen.New(s.pool).UpsertStorageSync(ctx, sqlcgen.UpsertStorageSyncParams{
		Store:          s.storeName,
		Root:           observation.Root,
		Path:           observation.Path,
		TotalBytes:     observation.TotalBytes,
		UsedBytes:      observation.UsedBytes,
		AvailableBytes: observation.AvailableBytes,
	}); err != nil {
		return fmt.Errorf("upserting storage root %q: %w", observation.Root, err)
	}
	return nil
}

// ListStorageSync は現在の root 別観測値を返す。store 名未設定時の全件読み取りは
// TestListStorageSyncWithoutConfiguredStoreIncludesNamedObservations で固定する。
func (s *Media) ListStorageSync(ctx context.Context) ([]store.StorageSync, error) {
	queries := sqlcgen.New(s.pool)
	var rows []sqlcgen.StorageSync
	var err error
	if s.storeName == "" {
		rows, err = queries.ListAllStorageSync(ctx)
	} else {
		rows, err = queries.ListStorageSync(ctx, s.storeName)
	}
	if err != nil {
		return nil, fmt.Errorf("listing storage sync: %w", err)
	}
	out := make([]store.StorageSync, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.StorageSync{
			Root:           row.Root,
			Path:           row.Path,
			TotalBytes:     row.TotalBytes,
			UsedBytes:      row.UsedBytes,
			AvailableBytes: row.AvailableBytes,
			ObservedAt:     row.ObservedAt,
		})
	}
	return out, nil
}

// ListAllMediaAssetRelPaths は孤児との差集合に使う、この置き場の全コピーのパスを返す。
func (s *Media) ListAllMediaAssetRelPaths(ctx context.Context) ([]string, error) {
	rows, err := sqlcgen.New(s.pool).ListAllMediaAssetRelPaths(ctx, s.storeName)
	if err != nil {
		return nil, fmt.Errorf("listing known media asset rel paths: %w", err)
	}
	return rows, nil
}

// UpsertOrphanFile は孤児候補を記録し、既存候補の first_seen を保持する。
func (s *Media) UpsertOrphanFile(ctx context.Context, relPath string) error {
	if err := sqlcgen.New(s.pool).UpsertOrphanFile(ctx, sqlcgen.UpsertOrphanFileParams{Store: s.storeName, RelPath: relPath}); err != nil {
		return fmt.Errorf("recording orphan path %q: %w", relPath, err)
	}
	return nil
}

// DeleteOrphanFile は解消した孤児候補を消す。
func (s *Media) DeleteOrphanFile(ctx context.Context, relPath string) error {
	if err := sqlcgen.New(s.pool).DeleteOrphanFile(ctx, sqlcgen.DeleteOrphanFileParams{Store: s.storeName, RelPath: relPath}); err != nil {
		return fmt.Errorf("deleting orphan path %q: %w", relPath, err)
	}
	return nil
}

// ListAllOrphanFiles は記録中の孤児候補を返す。
func (s *Media) ListAllOrphanFiles(ctx context.Context) ([]store.OrphanFile, error) {
	rows, err := sqlcgen.New(s.pool).ListAllOrphanFiles(ctx, s.storeName)
	if err != nil {
		return nil, fmt.Errorf("listing orphan candidates: %w", err)
	}
	out := make([]store.OrphanFile, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.OrphanFile{RelPath: row.RelPath, FirstSeen: row.FirstSeen})
	}
	return out, nil
}

// ListAgedOrphanFiles は指定期間より前から記録されている孤児候補を返す。
func (s *Media) ListAgedOrphanFiles(ctx context.Context, age time.Duration) ([]string, error) {
	rows, err := sqlcgen.New(s.pool).ListAgedOrphanFiles(ctx, sqlcgen.ListAgedOrphanFilesParams{
		Store: s.storeName, Age: age.Microseconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing aged orphan candidates: %w", err)
	}
	return rows, nil
}

// ListActiveMediaAssets は実体無しの差集合に使う、この置き場の全 active コピーを返す。
func (s *Media) ListActiveMediaAssets(ctx context.Context) ([]store.ActiveMediaAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListActiveMediaAssets(ctx, s.storeName)
	if err != nil {
		return nil, fmt.Errorf("listing active media assets: %w", err)
	}
	out := make([]store.ActiveMediaAsset, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.ActiveMediaAsset{
			ID: row.CopyID, MediaAssetID: row.MediaAssetID, RecordingID: row.RecordingID,
			RelPath: row.RelPath, Kind: row.Kind,
		})
	}
	return out, nil
}

// UpsertMissingMediaAsset は実体無し候補を記録し、既存候補の first_seen を保持する。
func (s *Media) UpsertMissingMediaAsset(ctx context.Context, copyID int64) error {
	if err := sqlcgen.New(s.pool).UpsertMissingMediaAsset(ctx, copyID); err != nil {
		return fmt.Errorf("recording missing media asset copy %d: %w", copyID, err)
	}
	return nil
}

// DeleteMissingMediaAsset は実体が再び観測された候補を消す。
func (s *Media) DeleteMissingMediaAsset(ctx context.Context, copyID int64) error {
	if err := sqlcgen.New(s.pool).DeleteMissingMediaAsset(ctx, copyID); err != nil {
		return fmt.Errorf("deleting missing media asset copy %d: %w", copyID, err)
	}
	return nil
}

// ListAllMissingMediaAssetIDs は記録中の実体無し候補 ID を返す。
func (s *Media) ListAllMissingMediaAssetIDs(ctx context.Context) ([]int64, error) {
	rows, err := sqlcgen.New(s.pool).ListAllMissingMediaAssetIDs(ctx, s.storeName)
	if err != nil {
		return nil, fmt.Errorf("listing missing media asset candidates: %w", err)
	}
	return rows, nil
}

// ListAgedMissingMediaAssets は指定期間より前から見つからない active コピーを返す。
func (s *Media) ListAgedMissingMediaAssets(ctx context.Context, age time.Duration) ([]store.AgedMissingMediaAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListAgedMissingMediaAssets(ctx, sqlcgen.ListAgedMissingMediaAssetsParams{
		Store: s.storeName, Age: age.Microseconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing aged missing media assets: %w", err)
	}
	out := make([]store.AgedMissingMediaAsset, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.AgedMissingMediaAsset{
			ID:           row.CopyID,
			MediaAssetID: row.MediaAssetID,
			RecordingID:  row.RecordingID,
			RelPath:      row.RelPath,
			Kind:         row.Kind,
			FirstSeen:    row.FirstSeen,
		})
	}
	return out, nil
}

func mediaQueryError(err error, context string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", context, store.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", context, err)
}

func nonNilStrings(values []string) []string {
	return append([]string{}, values...)
}
