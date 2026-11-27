package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// RegisterInPlace は既存ファイルを指す録画とアセットの行を一つの tx で登録する。
// 同じパスの active な再登録は何も変えずに通す。既存の行の検査は store.PlanInPlaceRegistration が行い、
// 拒否する場合は ErrConflict を返す（pgstore の同名の関数と同じ規則）。
func (s *Media) RegisterInPlace(ctx context.Context, in store.InPlaceInput) (store.InPlaceResult, error) {
	if len(in.Assets) == 0 {
		return store.InPlaceResult{}, errors.New("in-place registration requires at least one asset")
	}
	var result store.InPlaceResult
	err := runTx(ctx, s.db, func(tx *sql.Tx) error {
		now, err := transactionTime(ctx, tx)
		if err != nil {
			return err
		}
		observed := make([]*store.InPlaceCopyObservation, len(in.Assets))
		for i, asset := range in.Assets {
			row, err := lookupInPlaceAsset(ctx, tx, s.storeName, asset.RelPath)
			if err != nil {
				return err
			}
			observed[i] = row
		}
		plan, err := store.PlanInPlaceRegistration(s.storeName, in.Assets, observed)
		if err != nil {
			return err
		}

		recordingID := plan.RecordingID
		if recordingID == 0 {
			recordingID, err = upsertInPlaceRecording(ctx, tx, in.Recording)
			if err != nil {
				return err
			}
		}

		result = store.InPlaceResult{RecordingID: recordingID, AssetIDs: make([]int64, len(in.Assets))}
		for i, asset := range in.Assets {
			if reused := plan.ReusedAssetIDs[i]; reused != 0 {
				result.AssetIDs[i] = reused
				continue
			}
			id, generation, err := upsertInPlaceMediaAsset(ctx, tx, now, recordingID, asset, s.storeName)
			if err != nil {
				return err
			}
			result.AssetIDs[i] = id
			if err := recordCopyActive(ctx, tx, now, id, s.storeName, asset.RelPath, generation); err != nil {
				return fmt.Errorf("recording in-place media asset copy %q: %w", asset.RelPath, err)
			}
		}
		return nil
	})
	if err != nil {
		return store.InPlaceResult{}, err
	}
	return result, nil
}

// lookupInPlaceAsset は置き場の rel_path を使う行を 1 つ読む（墓石を含む）。無ければ nil を返す。
func lookupInPlaceAsset(ctx context.Context, tx *sql.Tx, storeName, relPath string) (*store.InPlaceCopyObservation, error) {
	var row store.InPlaceCopyObservation
	var profile sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT a.id, a.recording_id, a.kind, a.profile, c.state,
       (r.deleted_at IS NOT NULL OR r.purged_at IS NOT NULL)
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
JOIN recordings r ON r.id = a.recording_id
WHERE c.rel_path_key = UNHEX(SHA2(?, 256)) AND c.store = ?
LIMIT 1`, relPath, storeName).Scan(&row.AssetID, &row.RecordingID, &row.Kind, &profile, &row.CopyState, &row.RecordingRemoved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up in-place asset %q: %w", relPath, err)
	}
	if profile.Valid {
		p := profile.String
		row.Profile = &p
	}
	return &row, nil
}

// upsertInPlaceRecording は録画の行を、同じ放送イベントの生きた行があれば更新し、無ければ作る。
// 生きた行の同一性は active_program_start_at（ごみ箱・superseded で NULL になる生成列）で決める。
// series_key は題名から（seriesKeyValue）、INSERT と UPDATE の両方で書く。
func upsertInPlaceRecording(ctx context.Context, tx *sql.Tx, r store.InPlaceRecording) (int64, error) {
	seriesKey := seriesKeyValue(r.Title)
	var id int64
	err := tx.QueryRowContext(ctx, `
SELECT id FROM recordings
WHERE site = ? AND network_id = ? AND service_id = ? AND event_id = ? AND active_program_start_at = ?
FOR UPDATE`, r.Site, r.NetworkID, r.ServiceID, r.EventID, r.ProgramStartAt).Scan(&id)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `
UPDATE recordings
SET source = ?, service_name = ?, channel_type = ?, channel = ?, title = ?, program_duration_ms = ?, status = ?,
    started_at = COALESCE(started_at, ?), ended_at = COALESCE(ended_at, ?), series_key = ?, updated_at = CURRENT_TIMESTAMP(6)
WHERE id = ?`, r.Source, r.ServiceName, r.ChannelType, r.Channel, r.Title, r.ProgramDurationMs, r.Status,
			r.StartedAt, r.EndedAt, seriesKey, id); err != nil {
			return 0, fmt.Errorf("updating in-place recording: %w", translateError(err))
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("locking in-place recording: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, program_start_at, program_duration_ms, status, started_at, ended_at, series_key)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Source, r.Site, r.NetworkID, r.ServiceID, r.EventID, r.ServiceName, r.ChannelType, r.Channel,
		r.Title, r.ProgramStartAt, r.ProgramDurationMs, r.Status, r.StartedAt, r.EndedAt, seriesKey)
	if err != nil {
		return 0, fmt.Errorf("upserting in-place recording: %w", translateError(err))
	}
	return res.LastInsertId()
}

// upsertInPlaceMediaAsset は論理行を登録し、そのコピーの世代を返す。
// 論理行が既にあり、この置き場に生きたコピーが無く、世代が現行と同じなら、内容を書き換える。
// 録画がごみ箱か purge 済みなら、論理行は作らず ErrConflict を返す。
func upsertInPlaceMediaAsset(ctx context.Context, tx *sql.Tx, now time.Time, recordingID int64, asset store.InPlaceAsset, storeName string) (int64, int64, error) {
	var trashed, purged bool
	if err := tx.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL, purged_at IS NOT NULL FROM recordings WHERE id = ? FOR UPDATE`,
		recordingID).Scan(&trashed, &purged); err != nil {
		return 0, 0, fmt.Errorf("locking in-place recording %d: %w", recordingID, err)
	}
	if trashed || purged {
		return 0, 0, fmt.Errorf("in-place asset %q: recording %d is removed: %w", asset.RelPath, recordingID, store.ErrConflict)
	}
	generation := int64(0)
	if asset.Kind != store.AssetKindOriginal {
		generation = generationFromRelPath(asset.RelPath)
	}
	profileKey := ""
	if asset.Profile != nil {
		profileKey = *asset.Profile
	}
	var id, current int64
	err := tx.QueryRowContext(ctx, `
SELECT id, derived_generation FROM media_assets
WHERE recording_id = ? AND kind = ? AND profile_key = ? FOR UPDATE`, recordingID, asset.Kind, profileKey).Scan(&id, &current)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := tx.ExecContext(ctx, `
INSERT INTO media_assets (recording_id, kind, profile, size_bytes, derived_generation, updated_at)
VALUES (?, ?, ?, ?, ?, ?)`, recordingID, asset.Kind, optionalValue(asset.Profile), asset.SizeBytes, generation, now)
		if err != nil {
			return 0, 0, fmt.Errorf("upserting in-place asset %q: %w", asset.RelPath, translateError(err))
		}
		id, err = res.LastInsertId()
		if err != nil {
			return 0, 0, fmt.Errorf("reading in-place asset ID: %w", err)
		}
		return id, generation, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("locking in-place asset %q: %w", asset.RelPath, err)
	}
	var live int
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM media_asset_copies WHERE media_asset_id = ? AND store = ? AND state <> 'deleted')`,
		id, storeName).Scan(&live); err != nil {
		return 0, 0, fmt.Errorf("checking live copies of in-place asset %q: %w", asset.RelPath, err)
	}
	if live == 1 || generation != current {
		return 0, 0, fmt.Errorf("upserting in-place asset %q: the logical asset is in use or at another generation: %w", asset.RelPath, store.ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE media_assets SET content_revision = content_revision + 1, size_bytes = ?, sha256 = NULL, updated_at = ?
WHERE id = ?`, asset.SizeBytes, now, id); err != nil {
		return 0, 0, fmt.Errorf("updating in-place asset %q: %w", asset.RelPath, translateError(err))
	}
	if asset.Kind == store.AssetKindOriginal {
		// 原本の内容が変わったので、凍結した TS の走査結果を捨てる。
		if _, err := tx.ExecContext(ctx, `DELETE FROM media_asset_ts_scans WHERE media_asset_id = ?`, id); err != nil {
			return 0, 0, fmt.Errorf("deleting stale ts scan of in-place asset %q: %w", asset.RelPath, err)
		}
	}
	return id, generation, nil
}
