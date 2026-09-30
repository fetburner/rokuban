package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
)

type activeMediaAssetCheck func(context.Context, *sqlcgen.Queries) (bool, error)
type mediaAssetUpsert func(context.Context, *sqlcgen.Queries, int64) error

type stagedMediaAsset struct {
	tempPath  string
	finalPath string
	size      int64
}

func newWorkerScratchDir(scratchRoot, kind string, jobID int64, attempt int) (string, error) {
	if scratchRoot == "" {
		return "", fmt.Errorf("scratch dir is empty")
	}
	root := filepath.Join(scratchRoot, kind)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating %s scratch root: %w", kind, err)
	}
	return os.MkdirTemp(root, fmt.Sprintf("%d-%d-", jobID, attempt))
}

func stageMediaAsset(src, finalPath string) (stagedMediaAsset, error) {
	dir := filepath.Dir(finalPath)
	temp, err := os.CreateTemp(dir, mediapath.GeneratedAssetTempFilePrefix+"*")
	if err != nil {
		return stagedMediaAsset{}, fmt.Errorf("creating staged media asset: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return stagedMediaAsset{}, fmt.Errorf("closing staged media asset: %w", err)
	}
	// CreateTemp uses 0600; published media is readable by the streamer and other
	// worker roles, so match the canonical asset mode before publication.
	if err := os.Chmod(tempPath, 0o644); err != nil {
		_ = os.Remove(tempPath)
		return stagedMediaAsset{}, fmt.Errorf("chmod staged media asset: %w", err)
	}
	size, err := streamCopyFile(src, tempPath)
	if err != nil {
		_ = os.Remove(tempPath)
		return stagedMediaAsset{}, fmt.Errorf("staging media asset: %w", err)
	}
	return stagedMediaAsset{tempPath: tempPath, finalPath: finalPath, size: size}, nil
}

func (s stagedMediaAsset) publish() error {
	if err := os.Rename(s.tempPath, s.finalPath); err != nil {
		return fmt.Errorf("renaming staged media asset: %w", err)
	}
	if err := syncIngestDirectory(s.finalPath); err != nil {
		return fmt.Errorf("syncing media asset parent directory: %w", err)
	}
	return nil
}

func (s stagedMediaAsset) discard() {
	if s.tempPath != "" {
		_ = os.Remove(s.tempPath)
	}
}

// publishGeneratedMediaAsset makes a completed thumbnail or seek-tile sheet
// visible using the same rel_path protocol as ingest and encode. The row is
// upserted inside an uncommitted transaction first to reserve its path; the
// staged file is then renamed and its parent synced before commit.
func publishGeneratedMediaAsset(
	ctx context.Context,
	pool *pgxpool.Pool,
	mediaDir, relPath, scratchPath string,
	isActive activeMediaAssetCheck,
	upsert mediaAssetUpsert,
) (size int64, published bool, err error) {
	finalPath, err := mediapath.Resolve(mediaDir, relPath)
	if err != nil {
		return 0, false, fmt.Errorf("resolving generated media asset: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return 0, false, fmt.Errorf("creating media asset directory: %w", err)
	}
	staged, err := stageMediaAsset(scratchPath, finalPath)
	if err != nil {
		return 0, false, err
	}
	defer staged.discard()

	fileLock, err := lockMediaRelPathFile(ctx, mediaDir, relPath)
	if err != nil {
		return 0, false, fmt.Errorf("locking generated media asset: %w", err)
	}
	defer func() { _ = fileLock.Close() }()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("beginning generated media asset commit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockMediaRelPathInTransaction(ctx, tx, relPath); err != nil {
		return 0, false, err
	}
	q := sqlcgen.New(tx)
	active, err := isActive(ctx, q)
	if err != nil {
		return 0, false, fmt.Errorf("checking active generated media asset: %w", err)
	}
	if active {
		return 0, false, nil
	}
	if err := upsert(ctx, q, staged.size); err != nil {
		return 0, false, fmt.Errorf("upserting generated media asset: %w", err)
	}
	if err := staged.publish(); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("committing generated media asset: %w", err)
	}
	return staged.size, true, nil
}

func activeThumbnailExists(ctx context.Context, q *sqlcgen.Queries, recordingID int64) (bool, error) {
	_, err := q.GetActiveThumbnailMediaAssetID(ctx, recordingID)
	if errors.Is(err, pgx5.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func activeSeekTilesExist(ctx context.Context, q *sqlcgen.Queries, recordingID int64) (bool, error) {
	_, err := q.GetActiveSeekTilesMediaAssetID(ctx, recordingID)
	if errors.Is(err, pgx5.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
