package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// Media はメディア配信・reconcile・API・ストレージ観測の契約を PostgreSQL で実装する。
// pool を持ち、利用側の用途ごとに分けた interface を満たす。
type Media struct {
	pool *pgxpool.Pool
}

// NewMedia は PostgreSQL を使う Media を作る。
func NewMedia(pool *pgxpool.Pool) *Media {
	return &Media{pool: pool}
}

var (
	_ store.MediaServing       = (*Media)(nil)
	_ store.MediaReconcile     = (*Media)(nil)
	_ store.MediaReader        = (*Media)(nil)
	_ store.StorageObservation = (*Media)(nil)
)

func servingAsset(row sqlcgen.GetOriginalMediaAssetForServingRow) store.MediaAsset {
	return store.MediaAsset{ID: row.ID, RelPath: row.RelPath, SizeBytes: row.SizeBytes}
}

// GetOriginalMediaAssetForServing は配信可能な原本を返す。
func (s *Media) GetOriginalMediaAssetForServing(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetOriginalMediaAssetForServing(ctx, recordingID)
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
	})
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading encoded media asset for recording %d profile %q", recordingID, profile))
	}
	return store.MediaAsset{ID: row.ID, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// GetThumbnailMediaAssetForServing は配信可能なサムネイルを返す。
func (s *Media) GetThumbnailMediaAssetForServing(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetThumbnailMediaAssetForServing(ctx, recordingID)
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading thumbnail media asset for recording %d", recordingID))
	}
	return store.MediaAsset{ID: row.ID, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// GetSeekTilesMediaAssetForServing は配信可能なシークタイルを返す。
func (s *Media) GetSeekTilesMediaAssetForServing(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetSeekTilesMediaAssetForServing(ctx, recordingID)
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading seek tiles media asset for recording %d", recordingID))
	}
	return store.MediaAsset{ID: row.ID, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// GetActiveOriginalMediaAsset は追っかけ配信に使う active な原本を返す。
func (s *Media) GetActiveOriginalMediaAsset(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetActiveOriginalMediaAsset(ctx, recordingID)
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading active original media asset for recording %d", recordingID))
	}
	return store.MediaAsset{ID: row.ID, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// HasActiveThumbnailMediaAsset は active なサムネイルがあれば true を返す。
func (s *Media) HasActiveThumbnailMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	_, err := sqlcgen.New(s.pool).GetActiveThumbnailMediaAssetID(ctx, recordingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking active thumbnail media asset for recording %d: %w", recordingID, err)
	}
	return true, nil
}

// GetEncodedAssetForPlan はエンコード計画の比較に使う encoded 行を、凍結された keep 区間を含めて state を問わず返す。
// 行と区間は読み取り専用の RepeatableRead tx で同じスナップショットから読む。
func (s *Media) GetEncodedAssetForPlan(ctx context.Context, recordingID int64, profile string) (store.EncodedAssetObservation, error) {
	desc := fmt.Sprintf("loading encoded media asset for recording %d profile %q", recordingID, profile)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return store.EncodedAssetObservation{}, fmt.Errorf("beginning %s: %w", desc, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	row, err := q.GetEncodedMediaAssetForProfile(ctx, sqlcgen.GetEncodedMediaAssetForProfileParams{
		RecordingID: recordingID,
		Profile:     &profile,
	})
	if err != nil {
		return store.EncodedAssetObservation{}, mediaQueryError(err, desc)
	}
	raw, err := q.GetMediaAssetKeepRangesJSON(ctx, row.ID)
	if err != nil {
		return store.EncodedAssetObservation{}, fmt.Errorf("loading frozen cut ranges for media asset %d: %w", row.ID, err)
	}
	var keep []chapters.Range
	if err := json.Unmarshal(raw, &keep); err != nil {
		return store.EncodedAssetObservation{}, fmt.Errorf("decoding frozen cut ranges for media asset %d: %w", row.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.EncodedAssetObservation{}, fmt.Errorf("committing %s: %w", desc, err)
	}
	return store.EncodedAssetObservation{ID: row.ID, RelPath: row.RelPath, State: row.State, KeepRanges: keep}, nil
}

// EncodedAssetRebuildRequestMatches は要求が残り、要求時の keep 区間が一致するかを返す。
func (s *Media) EncodedAssetRebuildRequestMatches(ctx context.Context, recordingID int64, profile string, keep []chapters.Range) (bool, error) {
	requested, err := sqlcgen.New(s.pool).GetEncodedAssetRebuildRequestForShare(ctx, sqlcgen.GetEncodedAssetRebuildRequestForShareParams{
		RecordingID: recordingID,
		Profile:     profile,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("loading rebuild request for recording %d profile %q: %w", recordingID, profile, err)
	}
	requestedKeep, err := db.KeepRangesFromMultirange(requested)
	if err != nil {
		return false, fmt.Errorf("decoding rebuild request for recording %d profile %q: %w", recordingID, profile, err)
	}
	return chapters.SameRanges(requestedKeep, keep), nil
}

// GetOriginalVODTarget はサイトに属する完了済み録画の配信可能な原本を返す。
func (s *Media) GetOriginalVODTarget(ctx context.Context, recordingID int64, site string) (store.OriginalVODTarget, error) {
	row, err := sqlcgen.New(s.pool).GetOriginalVODTarget(ctx, sqlcgen.GetOriginalVODTargetParams{
		RecordingID: recordingID,
		Site:        site,
	})
	if err != nil {
		return store.OriginalVODTarget{}, mediaQueryError(err, fmt.Sprintf("loading original VOD target for recording %d site %q", recordingID, site))
	}
	return store.OriginalVODTarget{
		MediaAsset: store.MediaAsset{ID: row.ID, RelPath: row.RelPath, SizeBytes: row.SizeBytes},
		Site:       row.Site,
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

// ListMissingEncodeProfiles は未生成 encoded の候補をページ単位で返す。
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
		out = append(out, store.MissingEncodeProfile{RecordingID: row.RecordingID, Profile: row.Profile})
	}
	return out, nil
}

// ListEncodedAssetRebuildRequests は満たされていない作り直し要求を返す。
func (s *Media) ListEncodedAssetRebuildRequests(ctx context.Context, recordingID *int64) ([]store.EncodeAssetRebuildRequest, error) {
	rows, err := sqlcgen.New(s.pool).ListEncodedAssetRebuildRequests(ctx, recordingID)
	if err != nil {
		return nil, fmt.Errorf("listing encoded asset rebuild requests: %w", err)
	}
	out := make([]store.EncodeAssetRebuildRequest, 0, len(rows))
	for _, row := range rows {
		keep, err := db.KeepRangesFromMultirange(row.RequestedKeep)
		out = append(out, store.EncodeAssetRebuildRequest{
			RecordingID:   row.RecordingID,
			Profile:       row.Profile,
			RequestedKeep: keep,
			DecodeErr:     err,
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
	rows, err := sqlcgen.New(s.pool).ListEncodedMeaningEvidence(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing encoded meaning evidence: %w", err)
	}
	out := make([]store.EncodedMeaningEvidence, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.EncodedMeaningEvidence{Profile: row.Profile, RelPath: row.RelPath})
	}
	return out, nil
}

// ListMissingThumbnailRecordings はサムネイルが無い録画をページ単位で返す。
func (s *Media) ListMissingThumbnailRecordings(ctx context.Context, page store.MediaReconcilePage) ([]int64, error) {
	rows, err := sqlcgen.New(s.pool).ListMissingThumbnailRecordings(ctx, sqlcgen.ListMissingThumbnailRecordingsParams{
		AfterRecordingID: page.AfterRecordingID,
		RecordingID:      page.RecordingID,
		RowLimit:         page.RowLimit,
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
			OriginalRelPath:       row.OriginalRelPath,
			ThumbnailMediaAssetID: row.ThumbnailMediaAssetID,
			ThumbnailRelPath:      row.ThumbnailRelPath,
			SeekMs:                row.SeekMs,
		}
		// タイムラインの無い録画は、利用側が JSON を読まずに飛ばす。同じ行で読まない。
		if row.Detected || row.Owned {
			c.CMRanges, c.UserSpans, c.TimelineDecodeErr = decodeThumbnailTimeline(row.CmRanges, row.UserSpans)
			if c.TimelineDecodeErr == nil {
				c.EncodedAssets, c.InputsDecodeErr = decodeThumbnailEncodedAssets(row.EncodedAssets)
			}
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
		out = append(out, store.ThumbnailEncodedAsset{MediaAssetID: r.MediaAssetID, Profile: r.Profile, RelPath: r.RelPath, Cut: r.Cut, KeepRanges: r.KeepRanges})
	}
	return out, nil
}

// ListMissingSeekTilesRecordings はシークタイルが無い録画をページ単位で返す。
func (s *Media) ListMissingSeekTilesRecordings(ctx context.Context, page store.MediaReconcilePage) ([]int64, error) {
	rows, err := sqlcgen.New(s.pool).ListMissingSeekTilesRecordings(ctx, sqlcgen.ListMissingSeekTilesRecordingsParams{
		AfterRecordingID: page.AfterRecordingID,
		RecordingID:      page.RecordingID,
		RowLimit:         page.RowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing missing seek tiles recordings: %w", err)
	}
	return rows, nil
}

// GetActiveOriginalForTSScan は TS scan の対象になる active 原本を返す。
func (s *Media) GetActiveOriginalForTSScan(ctx context.Context, recordingID int64) (store.MediaAsset, error) {
	row, err := sqlcgen.New(s.pool).GetActiveOriginalForTSScan(ctx, recordingID)
	if err != nil {
		return store.MediaAsset{}, mediaQueryError(err, fmt.Sprintf("loading active original for TS scan on recording %d", recordingID))
	}
	return store.MediaAsset{ID: row.ID, RelPath: row.RelPath, SizeBytes: row.SizeBytes}, nil
}

// ListMissingTSScanRecordings は TS scan が必要な録画をページ単位で返す。
func (s *Media) ListMissingTSScanRecordings(ctx context.Context, afterRecordingID int64, rowLimit int32) ([]int64, error) {
	rows, err := sqlcgen.New(s.pool).ListMissingTSScanRecordings(ctx, sqlcgen.ListMissingTSScanRecordingsParams{
		AfterRecordingID: afterRecordingID,
		RowLimit:         rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing missing TS scan recordings: %w", err)
	}
	return rows, nil
}

const invalidMediaAssetRelPathQuery = `
SELECT id, kind, rel_path
FROM media_assets
WHERE state <> 'deleted'
  AND kind IN ('original', 'encoded')
  AND rel_path !~ '^sites/[^/]+/'
ORDER BY id
LIMIT 1
`

// FindInvalidMediaAssetRelPath は予約されたサイト名空間の外にある原本または encoded を返す。
func (s *Media) FindInvalidMediaAssetRelPath(ctx context.Context) (*store.MediaAssetRelPathViolation, error) {
	var violation store.MediaAssetRelPathViolation
	if err := s.pool.QueryRow(ctx, invalidMediaAssetRelPathQuery).Scan(&violation.ID, &violation.Kind, &violation.RelPath); err != nil {
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
	if err := sqlcgen.New(s.pool).DeleteStorageSyncExcept(ctx, nonNilStrings(roots)); err != nil {
		return fmt.Errorf("deleting stale storage roots: %w", err)
	}
	return nil
}

// UpsertStorageSync は 1 root の最新観測値を保存する。
func (s *Media) UpsertStorageSync(ctx context.Context, observation store.StorageObservationInput) error {
	if err := sqlcgen.New(s.pool).UpsertStorageSync(ctx, sqlcgen.UpsertStorageSyncParams{
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

// ListStorageSync は現在の root 別観測値を返す。
func (s *Media) ListStorageSync(ctx context.Context) ([]store.StorageSync, error) {
	rows, err := sqlcgen.New(s.pool).ListStorageSync(ctx)
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

// ListAllMediaAssetRelPaths は孤児との差集合に使う全 media_assets パスを返す。
func (s *Media) ListAllMediaAssetRelPaths(ctx context.Context) ([]string, error) {
	rows, err := sqlcgen.New(s.pool).ListAllMediaAssetRelPaths(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing known media asset rel paths: %w", err)
	}
	return rows, nil
}

// UpsertOrphanFile は孤児候補を記録し、既存候補の first_seen を保持する。
func (s *Media) UpsertOrphanFile(ctx context.Context, relPath string) error {
	if err := sqlcgen.New(s.pool).UpsertOrphanFile(ctx, relPath); err != nil {
		return fmt.Errorf("recording orphan path %q: %w", relPath, err)
	}
	return nil
}

// DeleteOrphanFile は解消した孤児候補を消す。
func (s *Media) DeleteOrphanFile(ctx context.Context, relPath string) error {
	if err := sqlcgen.New(s.pool).DeleteOrphanFile(ctx, relPath); err != nil {
		return fmt.Errorf("deleting orphan path %q: %w", relPath, err)
	}
	return nil
}

// ListAllOrphanFiles は記録中の孤児候補を返す。
func (s *Media) ListAllOrphanFiles(ctx context.Context) ([]store.OrphanFile, error) {
	rows, err := sqlcgen.New(s.pool).ListAllOrphanFiles(ctx)
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
	rows, err := sqlcgen.New(s.pool).ListAgedOrphanFiles(ctx, age.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("listing aged orphan candidates: %w", err)
	}
	return rows, nil
}

// ListActiveMediaAssets は実体無しの差集合に使う全 active 行を返す。
func (s *Media) ListActiveMediaAssets(ctx context.Context) ([]store.ActiveMediaAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListActiveMediaAssets(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing active media assets: %w", err)
	}
	out := make([]store.ActiveMediaAsset, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.ActiveMediaAsset{ID: row.ID, RecordingID: row.RecordingID, RelPath: row.RelPath, Kind: row.Kind})
	}
	return out, nil
}

// UpsertMissingMediaAsset は実体無し候補を記録し、既存候補の first_seen を保持する。
func (s *Media) UpsertMissingMediaAsset(ctx context.Context, mediaAssetID int64) error {
	if err := sqlcgen.New(s.pool).UpsertMissingMediaAsset(ctx, mediaAssetID); err != nil {
		return fmt.Errorf("recording missing media asset %d: %w", mediaAssetID, err)
	}
	return nil
}

// DeleteMissingMediaAsset は実体が再び観測された候補を消す。
func (s *Media) DeleteMissingMediaAsset(ctx context.Context, mediaAssetID int64) error {
	if err := sqlcgen.New(s.pool).DeleteMissingMediaAsset(ctx, mediaAssetID); err != nil {
		return fmt.Errorf("deleting missing media asset %d: %w", mediaAssetID, err)
	}
	return nil
}

// ListAllMissingMediaAssetIDs は記録中の実体無し候補 ID を返す。
func (s *Media) ListAllMissingMediaAssetIDs(ctx context.Context) ([]int64, error) {
	rows, err := sqlcgen.New(s.pool).ListAllMissingMediaAssetIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing missing media asset candidates: %w", err)
	}
	return rows, nil
}

// ListAgedMissingMediaAssets は指定期間より前から見つからない active 行を返す。
func (s *Media) ListAgedMissingMediaAssets(ctx context.Context, age time.Duration) ([]store.AgedMissingMediaAsset, error) {
	rows, err := sqlcgen.New(s.pool).ListAgedMissingMediaAssets(ctx, age.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("listing aged missing media assets: %w", err)
	}
	out := make([]store.AgedMissingMediaAsset, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.AgedMissingMediaAsset{
			ID:          row.ID,
			RecordingID: row.RecordingID,
			RelPath:     row.RelPath,
			Kind:        row.Kind,
			FirstSeen:   row.FirstSeen,
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
