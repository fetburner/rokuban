package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

var _ store.MediaReconcile = (*Media)(nil)

// inList は col が values のどれかに等しい述語を返す。空の並びは偽（Postgres の = ANY('{}') と同じ）。
func inSQL(col string, values []string) (string, []any) {
	if len(values) == 0 {
		return "FALSE", nil
	}
	return col + " IN (" + qmarks(len(values)) + ")", anyArgs(values)
}

// notInList は col が values のどれにも等しくない述語を返す。空の並びは真。
func notInSQL(col string, values []string) (string, []any) {
	if len(values) == 0 {
		return "TRUE", nil
	}
	return "NOT (" + col + " IN (" + qmarks(len(values)) + "))", anyArgs(values)
}

// nonNilProfiles は nil の並びを空の並びに写す。
func nonNilProfiles(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// currentCutKeepTx は録画の確認済みチャプターから現在の keep 区間を導出する。
// 録画が無ければ store.ErrNotFound、チャプターを所有していなければ owned は false。
func currentCutKeepTx(ctx context.Context, tx *sql.Tx, recordingID int64) ([]chapters.Range, bool, error) {
	var duration int64
	if err := tx.QueryRowContext(ctx, `SELECT program_duration_ms FROM recordings WHERE id = ?`, recordingID).Scan(&duration); err != nil {
		return nil, false, mediaQueryError(err, fmt.Sprintf("recording %d", recordingID))
	}
	var owned int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM recording_chapter_ownership WHERE recording_id = ?`, recordingID).Scan(&owned); err != nil {
		return nil, false, fmt.Errorf("loading chapter ownership for recording %d: %w", recordingID, err)
	}
	if owned == 0 {
		return nil, false, nil
	}
	spans, err := readUserSpans(ctx, tx, recordingID)
	if err != nil {
		return nil, false, fmt.Errorf("loading chapter spans for recording %d: %w", recordingID, err)
	}
	return chapters.KeepRanges(chapters.Derive(true, spans, nil, duration)), true, nil
}

// GetCurrentCutKeep は確認済みチャプターから現在の keep 区間を導出する。
func (s *Media) GetCurrentCutKeep(ctx context.Context, recordingID int64) ([]chapters.Range, bool, error) {
	var keep []chapters.Range
	var owned bool
	err := runSnapshotTx(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		keep, owned, err = currentCutKeepTx(ctx, tx, recordingID)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return keep, owned, nil
}

// GetRecordingEncodeTimes はエンコード締切の計算に使う録画時刻を返す。
func (s *Media) GetRecordingEncodeTimes(ctx context.Context, recordingID int64) (store.RecordingEncodeTimes, error) {
	var startedAt, endedAt sql.NullTime
	var duration int64
	err := s.db.QueryRowContext(ctx, `SELECT started_at, ended_at, program_duration_ms FROM recordings WHERE id = ?`, recordingID).
		Scan(&startedAt, &endedAt, &duration)
	if err != nil {
		return store.RecordingEncodeTimes{}, mediaQueryError(err, fmt.Sprintf("loading recording times for recording %d", recordingID))
	}
	return store.RecordingEncodeTimes{StartedAt: nullTimePtr(startedAt), EndedAt: nullTimePtr(endedAt), ProgramDurationMs: duration}, nil
}

// GetRecordingNotifySubject は通知に載せる局名と番組名を返す。purge 後の録画も対象に含む。
func (s *Media) GetRecordingNotifySubject(ctx context.Context, recordingID int64) (store.RecordingNotifySubject, error) {
	var subject store.RecordingNotifySubject
	err := s.db.QueryRowContext(ctx, `SELECT site, title FROM recordings WHERE id = ?`, recordingID).Scan(&subject.Site, &subject.Title)
	if err != nil {
		return store.RecordingNotifySubject{}, mediaQueryError(err, fmt.Sprintf("loading recording notify subject for recording %d", recordingID))
	}
	return subject, nil
}

// HasActiveThumbnailMediaAsset は active なサムネイルがあれば true を返す。
// 派生物の置き場か一時置き場（FS の staging）に現行のコピーがあることを見る。
func (s *Media) HasActiveThumbnailMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	return s.hasActiveDerived(ctx, recordingID, "thumbnail")
}

// HasActiveSeekTilesMediaAsset は active なシークタイルがあれば true を返す。
func (s *Media) HasActiveSeekTilesMediaAsset(ctx context.Context, recordingID int64) (bool, error) {
	return s.hasActiveDerived(ctx, recordingID, "seek_tiles")
}

func (s *Media) hasActiveDerived(ctx context.Context, recordingID int64, kind string) (bool, error) {
	storeCond, storeArgs := s.derivedStoreCondition("copy.store")
	args := append([]any{recordingID, kind}, storeArgs...)
	var found int
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM media_assets asset
    JOIN media_asset_copies copy ON copy.media_asset_id = asset.id
    WHERE asset.recording_id = ? AND asset.kind = ?
      AND copy.state = 'active' AND copy.generation = asset.derived_generation
      AND `+storeCond+`
)`, args...).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("checking active %s media asset for recording %d: %w", kind, recordingID, err)
	}
	return found == 1, nil
}

// derivedStoreCondition は派生物の置き場の述語を返す。派生物の置き場、または一時置き場のいずれかに絞る。
// 両方とも空なら置き場では絞らない（pgstore の mediaStoreNameArg と staging の NULL の扱いと同じ）。
func (s *Media) derivedStoreCondition(column string) (string, []any) {
	name := s.derivedStoreName()
	staging := s.derivedStagingStores()
	if name == "" && staging == nil {
		return "TRUE", nil
	}
	parts := []string{column + " = ?"}
	args := []any{name}
	if len(staging) > 0 {
		in, inArgs := inSQL(column, staging)
		parts = append(parts, in)
		args = append(args, inArgs...)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// FindInvalidMediaAssetRelPath は名前空間の規則（sites/{site}/ で始まる）に反する原本・encoded のパスを 1 つ返す。
func (s *Media) FindInvalidMediaAssetRelPath(ctx context.Context) (*store.MediaAssetRelPathViolation, error) {
	var v store.MediaAssetRelPathViolation
	err := s.db.QueryRowContext(ctx, `
SELECT a.id, a.kind, c.rel_path
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
WHERE (? IS NULL OR c.store = ?)
  AND c.state <> 'deleted'
  AND a.kind IN ('original', 'encoded')
  AND c.rel_path NOT REGEXP '^sites/[^/]+/'
ORDER BY a.id
LIMIT 1`, nullableString(s.storeName), s.storeName).Scan(&v.ID, &v.Kind, &v.RelPath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checking media asset rel_path namespace: %w", err)
	}
	return &v, nil
}

// ListUnsatisfiableEncodeProfiles は現在の設定に無い desired profile と録画数を返す。
// ListMissingEncodeProfiles が落とした側で、改名や削除で永久に満たせない凍結済みの profile を数える。
func (s *Media) ListUnsatisfiableEncodeProfiles(ctx context.Context, knownProfiles []string) ([]store.EncodeProfileCount, error) {
	known, knownArgs := notInSQL("u.profile", nonNilProfiles(knownProfiles))
	rows, err := s.db.QueryContext(ctx, `
SELECT u.profile, COUNT(*)
FROM unencoded_desired_profiles u
JOIN encodable_recordings er ON er.recording_id = u.recording_id
WHERE `+known+`
GROUP BY u.profile
ORDER BY u.profile`, knownArgs...)
	if err != nil {
		return nil, fmt.Errorf("listing unsatisfiable encode profiles: %w", err)
	}
	return scanProfileCounts(rows, "unsatisfiable encode profiles")
}

// ListCutAwaitingReview は確認待ちの cut profile と録画数を返す。
func (s *Media) ListCutAwaitingReview(ctx context.Context, profiles []string) ([]store.EncodeProfileCount, error) {
	in, args := inSQL("a.profile", nonNilProfiles(profiles))
	rows, err := s.db.QueryContext(ctx, `
SELECT a.profile, COUNT(*)
FROM awaiting_review_candidates a
WHERE `+in+`
GROUP BY a.profile
ORDER BY a.profile`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing cut profiles awaiting review: %w", err)
	}
	return scanProfileCounts(rows, "cut profiles awaiting review")
}

func scanProfileCounts(rows *sql.Rows, what string) ([]store.EncodeProfileCount, error) {
	defer func() { _ = rows.Close() }()
	out := []store.EncodeProfileCount{}
	for rows.Next() {
		var c store.EncodeProfileCount
		if err := rows.Scan(&c.Profile, &c.Recordings); err != nil {
			return nil, fmt.Errorf("scanning %s: %w", what, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	return out, nil
}

// ListEncodedMeaningEvidence は、プロファイル名が過去に持った「成果物の意味」の証拠を返す。
// active な encoded の rel_path と、作り直し要求の行（rel_path は空）から集める。
func (s *Media) ListEncodedMeaningEvidence(ctx context.Context) ([]store.EncodedMeaningEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT a.profile, copy.rel_path
FROM media_assets a
JOIN media_asset_copies copy ON copy.media_asset_id = a.id
WHERE a.kind = 'encoded'
  AND copy.state = 'active' AND copy.generation = a.derived_generation
  AND (? IS NULL OR copy.store = ?)
UNION ALL
SELECT q.profile, ''
FROM encoded_asset_rebuild_requests q`, nullableString(s.derivedStoreName()), s.derivedStoreName())
	if err != nil {
		return nil, fmt.Errorf("listing encoded meaning evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.EncodedMeaningEvidence{}
	for rows.Next() {
		var e store.EncodedMeaningEvidence
		if err := rows.Scan(&e.Profile, &e.RelPath); err != nil {
			return nil, fmt.Errorf("scanning encoded meaning evidence: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading encoded meaning evidence: %w", err)
	}
	return out, nil
}

// ListMissingThumbnailRecordings は原本が実体ありで、サムネイルが active でない録画を返す。
// missing_media_assets に載っている原本は、ファイルが戻るまで定期パスから外す。
func (s *Media) ListMissingThumbnailRecordings(ctx context.Context, page store.MediaReconcilePage) ([]int64, error) {
	return s.listMissingDerivedRecordings(ctx, page, "thumbnail")
}

// ListMissingSeekTilesRecordings は原本が実体ありで、シークタイルが active でない録画を返す。
func (s *Media) ListMissingSeekTilesRecordings(ctx context.Context, page store.MediaReconcilePage) ([]int64, error) {
	return s.listMissingDerivedRecordings(ctx, page, "seek_tiles")
}

func (s *Media) listMissingDerivedRecordings(ctx context.Context, page store.MediaReconcilePage, kind string) ([]int64, error) {
	staging := s.derivedStagingStores()
	stagingIn, stagingArgs := inSQL("copy.store", staging)
	args := []any{page.AfterRecordingID, nullableRecordingID(page.RecordingID), nullableRecordingID(page.RecordingID),
		nullableString(s.originalStore), s.originalStore}
	args = append(args, kind, s.derivedStoreName())
	args = append(args, stagingArgs...)
	args = append(args, page.RowLimit)
	rows, err := s.db.QueryContext(ctx, `
SELECT o.recording_id
FROM media_assets o
JOIN recordings r ON r.id = o.recording_id
WHERE o.recording_id > ?
  AND (? IS NULL OR o.recording_id = ?)
  AND o.kind = 'original'
  AND r.deleted_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM media_asset_copies c
      WHERE c.media_asset_id = o.id AND c.state = 'active' AND c.generation = o.derived_generation
        AND (? IS NULL OR c.store = ?)
        AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = c.id)
  )
  AND NOT EXISTS (
      SELECT 1
      FROM media_assets t
      JOIN media_asset_copies copy ON copy.media_asset_id = t.id
      WHERE t.recording_id = o.recording_id AND t.kind = ?
        AND copy.state = 'active' AND copy.generation = t.derived_generation
        AND (copy.store = ? OR `+stagingIn+`)
  )
ORDER BY o.recording_id
LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing missing %s recordings: %w", kind, err)
	}
	return scanRecordingIDs(rows, "missing "+kind+" recordings")
}

func nullableRecordingID(id *int64) any {
	if id == nil {
		return nil
	}
	return *id
}

func scanRecordingIDs(rows *sql.Rows, what string) ([]int64, error) {
	defer func() { _ = rows.Close() }()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning %s: %w", what, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	return out, nil
}

// GetDerivedMediaAssetGeneration は thumbnail・seek_tiles の使用済みの最大の世代を返す。回収済みを含む。
func (s *Media) GetDerivedMediaAssetGeneration(ctx context.Context, recordingID int64, kind string) (int64, bool, error) {
	var generation int64
	err := s.db.QueryRowContext(ctx, `
SELECT GREATEST(asset.derived_generation, COALESCE(MAX(copy.generation), -1))
FROM media_assets asset
LEFT JOIN media_asset_copies copy ON copy.media_asset_id = asset.id
WHERE asset.recording_id = ? AND asset.kind = ? AND asset.profile IS NULL
GROUP BY asset.id`, recordingID, kind).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("getting %s generation for recording %d: %w", kind, recordingID, err)
	}
	return generation, true, nil
}

// ListMissingEncodeProfiles は不足と満たされていない作り直しを合わせた候補をページ単位で返す。
// 候補の録画は、この条件を満たす profile を持つ録画を順に row_limit 件まで取る。
// GetEncodedPublishState は版の公開の計画に要る状態を、1 つのスナップショットから読む。
func (s *Media) GetEncodedPublishState(ctx context.Context, recordingID int64, profile string, cut, includeCurrentCut bool) (store.EncodedPublishState, error) {
	var state store.EncodedPublishState
	err := runSnapshotTx(ctx, s.db, func(tx *sql.Tx) error {
		var desired sql.NullBool
		err := tx.QueryRowContext(ctx, `
SELECT JSON_CONTAINS(encode_profiles, JSON_QUOTE(?))
FROM recording_processing_policy WHERE recording_id = ?`, profile, recordingID).Scan(&desired)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("loading desired encode profiles for recording %d: %w", recordingID, err)
		}
		state.Desired = desired.Valid && desired.Bool

		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT requested_keep FROM encoded_asset_rebuild_requests WHERE recording_id = ? AND profile = ?`,
			recordingID, profile).Scan(&raw)
		if err == nil {
			state.HasRebuildRequest = true
			state.RebuildRequestKeep, err = decodeKeepRanges(raw)
			if err != nil {
				return fmt.Errorf("decoding rebuild request for recording %d profile %q: %w", recordingID, profile, err)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("loading rebuild request for recording %d profile %q: %w", recordingID, profile, err)
		}

		if includeCurrentCut && state.Desired {
			state.CurrentCutKeep, state.ChapterOwned, err = currentCutKeepTx(ctx, tx, recordingID)
			if err != nil {
				return fmt.Errorf("loading current cut state for recording %d: %w", recordingID, err)
			}
		}

		var assetID int64
		var active bool
		var derived int64
		err = tx.QueryRowContext(ctx, `
SELECT a.id,
       EXISTS (SELECT 1 FROM active_media_asset_copies ac WHERE ac.media_asset_id = a.id),
       a.derived_generation
FROM media_assets a
WHERE a.recording_id = ? AND a.kind = 'encoded' AND a.profile = ?`, recordingID, profile).Scan(&assetID, &active, &derived)
		if err == nil {
			observation := store.EncodedAssetObservation{ID: assetID, Active: active, DerivedGeneration: derived}
			if cut {
				observation.KeepRanges, err = assetKeepRangesTx(ctx, tx, assetID)
				if err != nil {
					return err
				}
			}
			state.EncodedAsset = &observation
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("loading existing encoded asset for recording %d profile %q: %w", recordingID, profile, err)
		}

		var maxGeneration int64
		err = tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(copy.generation), -1)
FROM media_assets a
LEFT JOIN media_asset_copies copy ON copy.media_asset_id = a.id
WHERE a.recording_id = ? AND a.kind = 'encoded' AND a.profile = ?`, recordingID, profile).Scan(&maxGeneration)
		if err != nil {
			return fmt.Errorf("loading used encoded generations for recording %d profile %q: %w", recordingID, profile, err)
		}
		if maxGeneration >= 0 {
			state.MaxCopyGeneration = &maxGeneration
		}
		return nil
	})
	if err != nil {
		return store.EncodedPublishState{}, err
	}
	return state, nil
}

// decodeKeepRanges は {"startMs","endMs"} の JSON 配列を区間の並びにする。
func decodeKeepRanges(raw []byte) ([]chapters.Range, error) {
	var ranges []chapters.Range
	if err := json.Unmarshal(raw, &ranges); err != nil {
		return nil, err
	}
	return ranges, nil
}

// assetKeepRangesTx は凍結した区間を読む。行が無ければ空の並び（cut でない版）。
func assetKeepRangesTx(ctx context.Context, tx *sql.Tx, assetID int64) ([]chapters.Range, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT keep_ranges FROM media_asset_cuts WHERE media_asset_id = ?`, assetID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []chapters.Range{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading frozen cut ranges for media asset %d: %w", assetID, err)
	}
	ranges, err := decodeKeepRanges(raw)
	if err != nil {
		return nil, fmt.Errorf("decoding frozen cut ranges for media asset %d: %w", assetID, err)
	}
	return ranges, nil
}
