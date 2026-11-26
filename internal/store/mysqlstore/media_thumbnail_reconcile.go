package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// ListThumbnailReselectCandidates は、サムネイルが active で CM 検出かユーザー所有のチャプターを持つ録画の
// 候補窓を返す。候補の ID の問い合わせと、各録画の詳細の読みは、1 つの snapshot tx の中で行う。
// そのため、ID を読んだ後に録画が purge・削除されても、同じ時点の状態だけを返す（ページ全体は失敗しない）。
// JSON の集約（JSON_ARRAYAGG）は並び順を指定できないため、詳細は Go 側で組み立てて並べる。
// ごみ箱と purge 済みは外す。
func (s *Media) ListThumbnailReselectCandidates(ctx context.Context, page store.MediaReconcilePage) ([]store.ThumbnailReselectCandidate, error) {
	stagingStores := s.derivedStagingStores()
	encodedStagingStores := stagingStores
	if s.originalStore != "" && len(encodedStagingStores) > 0 {
		// 置き場で修飾した worker の入力は、自分の FS または遠隔の宛先から読む。
		encodedStagingStores = []string{s.originalStore}
	}
	readable := append([]string{s.derivedStoreName()}, stagingStores...)
	readableIn, readableArgs := inSQL("c.store", readable)
	rec := nullableRecordingID(page.RecordingID)

	args := []any{page.AfterRecordingID, rec, rec}
	args = append(args, readableArgs...)
	args = append(args, page.RowLimit)
	query := `
SELECT r.id
FROM recordings r
WHERE r.purged_at IS NULL
  AND r.deleted_at IS NULL
  AND r.id > ?
  AND (? IS NULL OR r.id = ?)
  AND (EXISTS (SELECT 1 FROM recording_cm_detections d WHERE d.recording_id = r.id)
       OR EXISTS (SELECT 1 FROM recording_chapter_ownership o WHERE o.recording_id = r.id))
  AND EXISTS (
      SELECT 1
      FROM media_assets t
      JOIN media_asset_copies c ON c.media_asset_id = t.id
      WHERE t.recording_id = r.id AND t.kind = 'thumbnail'
        AND c.state = 'active' AND c.generation = t.derived_generation
        AND ` + readableIn + `
  )
ORDER BY r.id
LIMIT ?`

	var out []store.ThumbnailReselectCandidate
	err := runSnapshotTx(ctx, s.db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("listing thumbnail reselection candidates: %w", err)
		}
		ids, err := scanRecordingIDs(rows, "thumbnail reselection candidate ids")
		if err != nil {
			return fmt.Errorf("listing thumbnail reselection candidates: %w", err)
		}
		out = make([]store.ThumbnailReselectCandidate, 0, len(ids))
		for _, id := range ids {
			c, err := thumbnailReselectCandidateTx(ctx, tx, id, s.derivedStoreName(), s.originalStore, stagingStores, encodedStagingStores)
			if err != nil {
				return fmt.Errorf("listing thumbnail reselection candidates: %w", err)
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// thumbnailReselectCandidateTx は録画 1 件の詳細を、呼び出し側の snapshot tx の中で読んで組み立てる。
func thumbnailReselectCandidateTx(ctx context.Context, tx *sql.Tx, recordingID int64, derivedStore, originalStore string, stagingStores, encodedStagingStores []string) (store.ThumbnailReselectCandidate, error) {
	var c store.ThumbnailReselectCandidate
	c.RecordingID = recordingID
	if err := tx.QueryRowContext(ctx, `SELECT program_duration_ms FROM recordings WHERE id = ?`, recordingID).Scan(&c.ProgramDurationMs); err != nil {
		return c, mediaQueryError(err, fmt.Sprintf("loading program duration for recording %d", recordingID))
	}

	var detectedRanges []byte
	err := tx.QueryRowContext(ctx, `SELECT cm_ranges FROM recording_cm_detections WHERE recording_id = ?`, recordingID).Scan(&detectedRanges)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.CMRanges = []chapters.Range{}
	case err != nil:
		return c, fmt.Errorf("loading CM ranges for recording %d: %w", recordingID, err)
	default:
		if c.CMRanges, err = decodeKeepRanges(detectedRanges); err != nil {
			return c, fmt.Errorf("decoding CM ranges for recording %d: %w", recordingID, err)
		}
	}

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) > 0 FROM recording_chapter_ownership WHERE recording_id = ?`, recordingID).Scan(&c.Owned); err != nil {
		return c, fmt.Errorf("loading chapter ownership for recording %d: %w", recordingID, err)
	}
	c.UserSpans, err = readUserSpans(ctx, tx, recordingID)
	if err != nil {
		return c, fmt.Errorf("loading chapter spans for recording %d: %w", recordingID, err)
	}

	if err := loadOriginalForReselect(ctx, tx, recordingID, originalStore, &c); err != nil {
		return c, err
	}
	c.EncodedAssets, err = loadEncodedForReselect(ctx, tx, recordingID, derivedStore, encodedStagingStores)
	if err != nil {
		return c, err
	}
	if err := loadThumbnailForReselect(ctx, tx, recordingID, derivedStore, stagingStores, &c); err != nil {
		return c, err
	}
	return c, nil
}

// loadOriginalForReselect は、置き場に現行で実体のある原本を 1 つ読む。無ければ何も書かない（0 と空の値のまま）。
func loadOriginalForReselect(ctx context.Context, tx *sql.Tx, recordingID int64, originalStore string, c *store.ThumbnailReselectCandidate) error {
	err := tx.QueryRowContext(ctx, `
SELECT a.id, copy.store, copy.rel_path
FROM media_assets a
JOIN media_asset_copies copy ON copy.media_asset_id = a.id
WHERE a.recording_id = ? AND a.kind = 'original'
  AND copy.state = 'active' AND copy.generation = a.derived_generation
  AND (? IS NULL OR copy.store = ?)
  AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = copy.id)
ORDER BY copy.store
LIMIT 1`, recordingID, nullableString(originalStore), originalStore).Scan(&c.OriginalMediaAssetID, &c.OriginalStore, &c.OriginalRelPath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading original for recording %d: %w", recordingID, err)
	}
	return nil
}

// loadEncodedForReselect は録画の encoded 版を profile 順に読む。実体のある現行のコピーが無い版は含めない。
func loadEncodedForReselect(ctx context.Context, tx *sql.Tx, recordingID int64, derivedStore string, encodedStaging []string) ([]store.ThumbnailEncodedAsset, error) {
	staging, stagingArgs := inSQL("copy.store", encodedStaging)
	rows, err := tx.QueryContext(ctx, `
SELECT a.id, a.profile
FROM media_assets a
WHERE a.recording_id = ? AND a.kind = 'encoded'
ORDER BY a.profile`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("loading encoded assets for recording %d: %w", recordingID, err)
	}
	type encodedRow struct {
		id      int64
		profile string
	}
	var list []encodedRow
	for rows.Next() {
		var r encodedRow
		if err := rows.Scan(&r.id, &r.profile); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning encoded assets for recording %d: %w", recordingID, err)
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("reading encoded assets for recording %d: %w", recordingID, err)
	}
	_ = rows.Close()

	out := []store.ThumbnailEncodedAsset{}
	for _, r := range list {
		var copyStore, relPath string
		err := tx.QueryRowContext(ctx, `
SELECT copy.store, copy.rel_path
FROM media_asset_copies copy
WHERE copy.media_asset_id = ? AND copy.state = 'active' AND copy.generation = (
          SELECT a.derived_generation FROM media_assets a WHERE a.id = copy.media_asset_id)
  AND (copy.store = ? OR `+staging+`)
  AND NOT EXISTS (SELECT 1 FROM missing_media_assets m WHERE m.media_asset_copy_id = copy.id)
ORDER BY (copy.store = ?) DESC, copy.store
LIMIT 1`, append(append([]any{r.id, derivedStore}, stagingArgs...), derivedStore)...).Scan(&copyStore, &relPath)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("loading encoded copy of asset %d: %w", r.id, err)
		}
		asset := store.ThumbnailEncodedAsset{MediaAssetID: r.id, Store: copyStore, Profile: r.profile, RelPath: relPath, KeepRanges: []chapters.Range{}}
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT keep_ranges FROM media_asset_cuts WHERE media_asset_id = ?`, r.id).Scan(&raw)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("loading cut ranges of asset %d: %w", r.id, err)
		default:
			asset.Cut = true
			if asset.KeepRanges, err = decodeKeepRanges(raw); err != nil {
				return nil, fmt.Errorf("decoding cut ranges of asset %d: %w", r.id, err)
			}
		}
		out = append(out, asset)
	}
	return out, nil
}

// loadThumbnailForReselect は現行のサムネイル（派生物の置き場と一時置き場の順）と、その位置を読む。
func loadThumbnailForReselect(ctx context.Context, tx *sql.Tx, recordingID int64, derivedStore string, stagingStores []string, c *store.ThumbnailReselectCandidate) error {
	readable := append([]string{derivedStore}, stagingStores...)
	in, inArgs := inSQL("copy.store", readable)
	args := []any{recordingID}
	args = append(args, inArgs...)
	args = append(args, derivedStore)
	var id, generation int64
	var relPath, storeName string
	var seek sql.NullInt64
	err := tx.QueryRowContext(ctx, `
SELECT a.id, copy.rel_path, copy.store, a.derived_generation, seeks.seek_ms
FROM media_assets a
JOIN media_asset_copies copy ON copy.media_asset_id = a.id
LEFT JOIN media_asset_thumbnail_seeks seeks ON seeks.media_asset_id = a.id
WHERE a.recording_id = ? AND a.kind = 'thumbnail'
  AND copy.state = 'active' AND copy.generation = a.derived_generation
  AND `+in+`
ORDER BY (copy.store = ?) DESC, copy.store
LIMIT 1`, args...).Scan(&id, &relPath, &storeName, &generation, &seek)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading thumbnail of recording %d: %w", recordingID, err)
	}
	c.ThumbnailMediaAssetID = &id
	c.ThumbnailRelPath = &relPath
	c.ThumbnailStore = storeName
	c.ThumbnailGeneration = generation
	if seek.Valid {
		c.SeekMs = &seek.Int64
	}
	return nil
}
