package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/catalogdoc"
	"github.com/fetburner/rokuban/internal/store"
)

// copyParams は catalog の 1 物理コピー行を書くための値。
type copyParams struct {
	MediaAssetID int64
	Store        string
	RelPath      string
	Generation   int64
	State        string
	CreatedAt    time.Time
	DeletedAt    *time.Time
}

// liveCopy は、パスを使っている生きた物理コピーの行。
type liveCopy struct {
	ID           int64
	MediaAssetID int64
	Generation   int64
	State        string
}

// applyMediaAssets は media_assets と物理コピーを復元し、件数を res.MediaAssets に書く。
// 欠損として渡された生きたコピーは deleted として復元し、論理資産の統計は残す。
func applyMediaAssets(ctx context.Context, tx *sql.Tx, assets []catalogdoc.MediaAsset, res *store.RestoreResult, missingAssetCopyKeys map[store.MediaAssetCopyKey]struct{}) error {
	var missingAt time.Time
	for _, a := range assets {
		assetStore := a.Store
		_, observedMissing := missingAssetCopyKeys[store.MediaAssetCopyKey{MediaAssetID: a.ID, Store: assetStore, RelPath: a.RelPath}]
		copyState, copyDeletedAt := a.State, a.DeletedAt
		fileVerifiedMissing := observedMissing && a.State != "deleted"
		if fileVerifiedMissing {
			if missingAt.IsZero() {
				now, err := transactionTime(ctx, tx)
				if err != nil {
					return fmt.Errorf("reading database time for missing catalog assets: %w", err)
				}
				missingAt = now
			}
			// 履歴のコピーと従属する統計は残し、削除後に実ファイルが消えた状態を active として公開しない。
			copyState = "deleted"
			copyDeletedAt = &missingAt
			res.MissingMediaFiles++
		}
		sha, err := decodeMediaAssetSHA256(a.SHA256)
		if err != nil {
			return fmt.Errorf("decoding media_asset %d sha256: %w", a.ID, err)
		}
		var derivedGeneration, copyGeneration int64
		if a.Kind != "original" {
			derivedGeneration = *a.DerivedGeneration
			copyGeneration = *a.Generation
		}
		if err := upsertMediaAsset(ctx, tx, a, derivedGeneration, copyGeneration, sha); err != nil {
			return fmt.Errorf("upserting media_asset %d: %w", a.ID, err)
		}
		if err := upsertCatalogMediaAssetCopy(ctx, tx, copyParams{
			MediaAssetID: a.ID, Store: assetStore, RelPath: a.RelPath,
			Generation: copyGeneration, State: copyState, CreatedAt: a.CreatedAt, DeletedAt: copyDeletedAt,
		}, fileVerifiedMissing); err != nil {
			return fmt.Errorf("upserting media_asset_copy for asset %d in store %q: %w", a.ID, assetStore, err)
		}
	}
	res.MediaAssets = len(assets)
	return nil
}

// upsertMediaAsset は論理資産の行を id で書く。media_assets は identity（recording_id, kind, profile）の
// 一意キーも持つので ON DUPLICATE KEY UPDATE は使わず、行の有無を先に読んで UPDATE か INSERT を選ぶ。
// 現行世代の判定は Postgres 版と同じ: 物理コピーの世代が既存と文書の派生世代の大きい方に等しいときだけ、
// サイズと内容を差し替える。内容が変わったので、original の計測は捨てる。
func upsertMediaAsset(ctx context.Context, tx *sql.Tx, a catalogdoc.MediaAsset, derivedGeneration, copyGeneration int64, sha []byte) error {
	var derived, size int64
	var oldSHA []byte
	err := tx.QueryRowContext(ctx, `SELECT derived_generation, size_bytes, sha256 FROM media_assets WHERE id = ? FOR UPDATE`,
		a.ID).Scan(&derived, &size, &oldSHA)
	if errors.Is(err, sql.ErrNoRows) {
		_, err := tx.ExecContext(ctx, `
 INSERT INTO media_assets (id, recording_id, kind, profile, size_bytes, created_at, updated_at, derived_generation, sha256)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.RecordingID, a.Kind, optionalValue(a.Profile), a.SizeBytes, a.CreatedAt, a.UpdatedAt,
			derivedGeneration, optionalBytes(sha))
		return err
	}
	if err != nil {
		return fmt.Errorf("reading media_asset %d: %w", a.ID, err)
	}
	newDerived := max(derived, derivedGeneration)
	if a.Kind == "original" {
		newDerived = 0
	}
	newSize, newSHA := size, oldSHA
	if copyGeneration == max(derived, derivedGeneration) {
		newSize, newSHA = a.SizeBytes, sha
	}
	if _, err := tx.ExecContext(ctx, `
 UPDATE media_assets SET recording_id = ?, kind = ?, profile = ?, size_bytes = ?, created_at = ?, updated_at = ?,
     derived_generation = ?, sha256 = ?, content_revision = content_revision + 1
 WHERE id = ?`,
		a.RecordingID, a.Kind, optionalValue(a.Profile), newSize, a.CreatedAt, a.UpdatedAt,
		newDerived, optionalBytes(newSHA), a.ID); err != nil {
		return err
	}
	if a.Kind == "original" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM media_asset_ts_scans WHERE media_asset_id = ?`, a.ID); err != nil {
			return fmt.Errorf("deleting stale ts scan of asset %d: %w", a.ID, err)
		}
	}
	return nil
}

// upsertCatalogMediaAssetCopy は catalog の 1 コピー行を復元する。Postgres 版の同名の関数と同じ判定を行う。
//
// 墓石（state='deleted'）は (store, rel_path) を予約するので、パス P が別の行に使われていると P に置けない。
// その墓石は catalogTombstoneRelPath の合成パスに置く。同じ identity の墓石が既にあるかは、
// 原パス P と合成パスの両方で判定する。
func upsertCatalogMediaAssetCopy(ctx context.Context, tx *sql.Tx, p copyParams, fileVerifiedMissing bool) error {
	if p.State != "deleted" {
		rows, err := upsertMediaAssetCopy(ctx, tx, p)
		if err != nil {
			return err
		}
		if rows == 1 {
			return nil
		}
		if rows != 0 {
			return fmt.Errorf("upsert affected %d rows, want zero or one", rows)
		}
		// 行が統計の後に削除されたかもしれないので、古い生きたスナップショットで復活させない。
		// P の墓石、または同じ identity の合成パスの墓石があれば、そのコピーは既に無い。
		tombstone, err := hasDeletedCopyAtPhysicalPath(ctx, tx, p.Store, p.RelPath)
		if err != nil {
			return err
		}
		if tombstone {
			return nil
		}
		moved, err := hasDeletedCopy(ctx, tx, p.MediaAssetID, p.Store, p.Generation, []string{catalogTombstoneRelPath(p)})
		if err != nil {
			return err
		}
		if moved {
			return nil
		}
		return fmt.Errorf("upsert affected %d rows, want one", rows)
	}

	live, found, err := lockMediaAssetCopyByPhysicalPath(ctx, tx, p.Store, p.RelPath)
	if err != nil {
		return err
	}
	// 文書の墓石は履歴であり、今のパスの持ち主が欠けていることの証拠ではない。
	// 持ち主が同じ identity で、欠損と確認されたときだけ墓石に変える。
	if found && fileVerifiedMissing && live.MediaAssetID == p.MediaAssetID && live.Generation == p.Generation {
		rows, err := setMediaAssetCopyState(ctx, tx, live.ID, p.Store, "deleted", live.State, p.DeletedAt)
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("deleting copy %d affected %d rows, want one", live.ID, rows)
		}
		return nil
	}

	synthetic := catalogTombstoneRelPath(p)
	exists, err := hasDeletedCopy(ctx, tx, p.MediaAssetID, p.Store, p.Generation, []string{p.RelPath, synthetic})
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	rows, err := upsertMediaAssetCopy(ctx, tx, p)
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	if rows != 0 {
		return fmt.Errorf("insert affected %d rows, want zero or one", rows)
	}

	// P が別の行に使われている。合成パスは (asset, 世代, P) で一意なので、ここで 0 行になるのは並行した復元が先に置いた場合だけ。
	p.RelPath = synthetic
	rows, err = upsertMediaAssetCopy(ctx, tx, p)
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	if rows != 0 {
		return fmt.Errorf("insert at reserved path affected %d rows, want zero or one", rows)
	}
	return fmt.Errorf("insert at reserved path affected %d rows, want one", rows)
}

// upsertMediaAssetCopy は (store, rel_path) で物理コピーを書き、Postgres 版の ON CONFLICT と同じ戻り値を返す。
// 1 は行を書いた、0 は既存の行が別の identity か墓石で、書けなかったことを表す。
//
// MySQL の ON DUPLICATE KEY UPDATE は (media_asset_id, store, live_generation) の衝突でも発火するので使わない。
// 先に (store, rel_path) の行を読み、その行に対する条件だけで書く。他の一意キーの衝突は INSERT のエラーになる。
func upsertMediaAssetCopy(ctx context.Context, tx *sql.Tx, p copyParams) (int64, error) {
	var existingID, existingAsset, existingGeneration int64
	var existingState string
	err := tx.QueryRowContext(ctx, `
 SELECT id, media_asset_id, generation, state FROM media_asset_copies
 WHERE store = ? AND rel_path_key = UNHEX(SHA2(?, 256)) FOR UPDATE`,
		p.Store, p.RelPath).Scan(&existingID, &existingAsset, &existingGeneration, &existingState)
	if err == nil {
		if p.State != "deleted" && existingState != "deleted" && existingAsset == p.MediaAssetID && existingGeneration == p.Generation {
			if _, err := tx.ExecContext(ctx, `UPDATE media_asset_copies SET created_at = ? WHERE id = ?`, p.CreatedAt, existingID); err != nil {
				return 0, fmt.Errorf("updating media_asset_copy %d: %w", existingID, err)
			}
			return 1, nil
		}
		return 0, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("reading media_asset_copy at %q: %w", p.RelPath, err)
	}
	res, err := tx.ExecContext(ctx, `
 INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at, deleted_at)
 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.MediaAssetID, p.Store, p.RelPath, p.Generation, p.State, p.CreatedAt, optionalValue(p.DeletedAt))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// setMediaAssetCopyState は id のコピーの状態を expected からに変える。戻り値は変わった行数。
func setMediaAssetCopyState(ctx context.Context, tx *sql.Tx, id int64, storeName, state, expected string, deletedAt *time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `
 UPDATE media_asset_copies
 SET state = ?,
     deleted_at = CASE WHEN ? = 'deleted' THEN COALESCE(?, deleted_at, CURRENT_TIMESTAMP(6)) ELSE NULL END
 WHERE id = ? AND store = ? AND state = ?`,
		state, state, optionalValue(deletedAt), id, storeName, expected)
	if err != nil {
		return 0, fmt.Errorf("setting media_asset_copy %d state: %w", id, err)
	}
	return res.RowsAffected()
}

// lockMediaAssetCopyByPhysicalPath は、パスを使っている生きたコピーを FOR UPDATE で取る。無ければ found は false。
// 論理資産の行もロックするので、同じ資産の別の書き込みと直列化する。
func lockMediaAssetCopyByPhysicalPath(ctx context.Context, tx *sql.Tx, storeName, relPath string) (liveCopy, bool, error) {
	var c liveCopy
	err := tx.QueryRowContext(ctx, `
 SELECT copy.id, copy.media_asset_id, copy.generation, copy.state
 FROM media_assets asset
 JOIN media_asset_copies copy ON copy.media_asset_id = asset.id
 WHERE copy.store = ? AND copy.rel_path_key = UNHEX(SHA2(?, 256)) AND copy.state <> 'deleted'
 FOR UPDATE`,
		storeName, relPath).Scan(&c.ID, &c.MediaAssetID, &c.Generation, &c.State)
	if errors.Is(err, sql.ErrNoRows) {
		return liveCopy{}, false, nil
	}
	if err != nil {
		return liveCopy{}, false, fmt.Errorf("locking media_asset_copy at %q: %w", relPath, err)
	}
	return c, true, nil
}

// hasDeletedCopy は、資産・置き場・世代が一致する墓石が relPaths のどれかにあるかを返す。
func hasDeletedCopy(ctx context.Context, tx *sql.Tx, assetID int64, storeName string, generation int64, relPaths []string) (bool, error) {
	args := []any{assetID, storeName, generation}
	placeholders := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		placeholders = append(placeholders, "UNHEX(SHA2(?, 256))")
		args = append(args, relPath)
	}
	var exists int
	err := tx.QueryRowContext(ctx, `
 SELECT EXISTS (SELECT 1 FROM media_asset_copies
 WHERE media_asset_id = ? AND store = ? AND generation = ? AND state = 'deleted'
   AND rel_path_key IN (`+strings.Join(placeholders, ", ")+`))`, args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking tombstones of asset %d: %w", assetID, err)
	}
	return exists == 1, nil
}

// hasDeletedCopyAtPhysicalPath は、パス relPath に墓石（どの identity でも）があるかを返す。
// 墓石は (store, rel_path) を予約するので、パスだけで判定する。
func hasDeletedCopyAtPhysicalPath(ctx context.Context, tx *sql.Tx, storeName, relPath string) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx, `
 SELECT EXISTS (SELECT 1 FROM media_asset_copies
 WHERE store = ? AND rel_path_key = UNHEX(SHA2(?, 256)) AND state = 'deleted')`,
		storeName, relPath).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking tombstone at %q: %w", relPath, err)
	}
	return exists == 1, nil
}

// catalogTombstoneRelPath は catalog の墓石を、原パス p.RelPath が別の行に使われているときに置く合成パスを返す。
// (asset, 世代, 原パス) から決まるので、同じ墓石は何度復元しても同じパスになる。
// 長い単一成分なので、ファイルシステムの成分にも S3 のキーにもならない（Postgres 版と同じ形）。
func catalogTombstoneRelPath(p copyParams) string {
	sum := sha256.Sum256([]byte(p.RelPath))
	return strings.Repeat("t", 1025) + fmt.Sprintf("-catalog-%d-%d-%x", p.MediaAssetID, p.Generation, sum[:16])
}

func decodeMediaAssetSHA256(value *string) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	decoded, err := hex.DecodeString(*value)
	if err != nil {
		return nil, err
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("got %d bytes, want 32", len(decoded))
	}
	return decoded, nil
}
