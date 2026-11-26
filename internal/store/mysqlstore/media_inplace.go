package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// inPlaceExisting は rel_path で既に登録されている行。
type inPlaceExisting struct {
	id          int64
	recordingID int64
	kind        string
	profile     *string
	state       string
	trashed     bool
}

// RegisterInPlace は既存ファイルを指す録画とアセットの行を一つの tx で登録する。
// 同じパスの active な再登録は何も変えずに通す。拒否する場合は ErrConflict を返す（pgstore の同名の関数と同じ規則）。
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
		existing := make([]*inPlaceExisting, len(in.Assets))
		var recordingID int64
		for i, asset := range in.Assets {
			row, found, err := lookupInPlaceAsset(ctx, tx, s.storeName, asset.RelPath)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			if recordingID != 0 && recordingID != row.recordingID {
				return fmt.Errorf("in-place assets belong to different recordings: %d and %d: %w", recordingID, row.recordingID, store.ErrConflict)
			}
			recordingID = row.recordingID
			// 同じ置き場・パスの行は全体で 1 つなので、deleted 行は墓石である。
			if row.state == "deleted" {
				return fmt.Errorf(
					"in-place asset %q is tombstoned in store %q; move or rename the file before registering it again: %w",
					asset.RelPath, s.storeName, errors.Join(store.ErrConflict, store.ErrInPlacePathTombstoned),
				)
			}
			if row.trashed {
				return fmt.Errorf("in-place asset %q belongs to trashed recording %d: %w", asset.RelPath, row.recordingID, store.ErrConflict)
			}
			if row.state != "active" {
				return fmt.Errorf("in-place asset %q is %s and will be unlinked: %w", asset.RelPath, row.state, store.ErrConflict)
			}
			if row.kind != asset.Kind || !sameStringPointer(row.profile, asset.Profile) {
				return fmt.Errorf("published asset %q is %s/%v, requested %s/%v: %w", asset.RelPath, row.kind, row.profile, asset.Kind, asset.Profile, store.ErrConflict)
			}
			existing[i] = &row
		}

		if recordingID == 0 {
			recordingID, err = upsertInPlaceRecording(ctx, tx, in.Recording)
			if err != nil {
				return err
			}
		}

		result = store.InPlaceResult{RecordingID: recordingID, AssetIDs: make([]int64, len(in.Assets))}
		for i, asset := range in.Assets {
			if existing[i] != nil {
				result.AssetIDs[i] = existing[i].id
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

// lookupInPlaceAsset は置き場の rel_path を使う行を 1 つ読む（墓石を含む）。
func lookupInPlaceAsset(ctx context.Context, tx *sql.Tx, storeName, relPath string) (inPlaceExisting, bool, error) {
	var row inPlaceExisting
	var profile sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT a.id, a.recording_id, a.kind, a.profile, c.state,
       (r.deleted_at IS NOT NULL OR r.purged_at IS NOT NULL)
FROM media_asset_copies c
JOIN media_assets a ON a.id = c.media_asset_id
JOIN recordings r ON r.id = a.recording_id
WHERE c.rel_path_key = UNHEX(SHA2(?, 256)) AND c.store = ?
LIMIT 1`, relPath, storeName).Scan(&row.id, &row.recordingID, &row.kind, &profile, &row.state, &row.trashed)
	if errors.Is(err, sql.ErrNoRows) {
		return inPlaceExisting{}, false, nil
	}
	if err != nil {
		return inPlaceExisting{}, false, fmt.Errorf("looking up in-place asset %q: %w", relPath, err)
	}
	if profile.Valid {
		p := profile.String
		row.profile = &p
	}
	return row, true, nil
}

// upsertInPlaceRecording は録画の行を、同じ放送イベントの生きた行があれば更新し、無ければ作る。
// 生きた行の同一性は active_program_start_at（ごみ箱・superseded で NULL になる生成列）で決める。
func upsertInPlaceRecording(ctx context.Context, tx *sql.Tx, r store.InPlaceRecording) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
SELECT id FROM recordings
WHERE site = ? AND network_id = ? AND service_id = ? AND event_id = ? AND active_program_start_at = ?
FOR UPDATE`, r.Site, r.NetworkID, r.ServiceID, r.EventID, r.ProgramStartAt).Scan(&id)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `
UPDATE recordings
SET source = ?, service_name = ?, channel_type = ?, channel = ?, title = ?, program_duration_ms = ?, status = ?,
    started_at = COALESCE(started_at, ?), ended_at = COALESCE(ended_at, ?), updated_at = CURRENT_TIMESTAMP(6)
WHERE id = ?`, r.Source, r.ServiceName, r.ChannelType, r.Channel, r.Title, r.ProgramDurationMs, r.Status,
			r.StartedAt, r.EndedAt, id); err != nil {
			return 0, fmt.Errorf("updating in-place recording: %w", translateError(err))
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("locking in-place recording: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO recordings (source, site, network_id, service_id, event_id, service_name, channel_type, channel,
    title, program_start_at, program_duration_ms, status, started_at, ended_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Source, r.Site, r.NetworkID, r.ServiceID, r.EventID, r.ServiceName, r.ChannelType, r.Channel,
		r.Title, r.ProgramStartAt, r.ProgramDurationMs, r.Status, r.StartedAt, r.EndedAt)
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

// sameStringPointer は 2 つの *string が同じ値（両方 nil を含む）かを返す。
func sameStringPointer(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
