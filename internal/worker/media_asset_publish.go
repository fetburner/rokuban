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

type skipMediaAssetPublish func(context.Context, *sqlcgen.Queries) (bool, error)
type mediaAssetUpsert func(context.Context, *sqlcgen.Queries, int64) error

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

// stagedMediaFile は media ディレクトリへ置く前の一時ファイル。finalPath と同じ
// ディレクトリに置くので rename は同一 FS 内で完結する（scratch からの rename は
// `EXDEV` になる。docs/storage/contract.md §3 ルール 2）。
type stagedMediaFile struct {
	tempPath  string
	finalPath string
	size      int64
}

// stageMediaFile は src（scratch）を finalPath と同じディレクトリの一時ファイルへ
// ストリームコピー + fsync する。名前は予約接頭辞 prefix を付け、拡張子は付けない
// （rescueAssetKind は媒体拡張子でしか ok を返さないので、孤児 rescue には拾われない）。
// プロセス死で rel_path lock が残っても、次の lock 取得時の GC が回収する。
func stageMediaFile(src, finalPath, prefix string) (stagedMediaFile, error) {
	dir := filepath.Dir(finalPath)
	// CreateTemp は 0600 で作る。公開後のファイルは canonical なので、コピー元の
	// streamCopyFile と同じ 0644 に揃える（別 UID の streamer が読む構成がある）。
	temp, err := os.CreateTemp(dir, prefix+"*")
	if err != nil {
		return stagedMediaFile{}, fmt.Errorf("creating staged output in media dir: %w", err)
	}
	tempPath := temp.Name()
	_ = temp.Close()
	if err := os.Chmod(tempPath, 0o644); err != nil {
		_ = os.Remove(tempPath)
		return stagedMediaFile{}, fmt.Errorf("chmod staged output: %w", err)
	}
	size, err := streamCopyFile(src, tempPath)
	if err != nil {
		_ = os.Remove(tempPath)
		return stagedMediaFile{}, fmt.Errorf("staging output in media dir: %w", err)
	}
	return stagedMediaFile{tempPath: tempPath, finalPath: finalPath, size: size}, nil
}

// publish は staged 出力を canonical へ rename で公開し、親ディレクトリを fsync する。
func (s stagedMediaFile) publish() error {
	if err := os.Rename(s.tempPath, s.finalPath); err != nil {
		return fmt.Errorf("publishing staged output to canonical path: %w", err)
	}
	if err := syncIngestDirectory(s.finalPath); err != nil {
		return fmt.Errorf("syncing canonical parent directory: %w", err)
	}
	return nil
}

// discard は未公開の temp を消す。rename 済みなら temp は無いので何もしない。
func (s stagedMediaFile) discard() {
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
	shouldSkip skipMediaAssetPublish,
	upsert mediaAssetUpsert,
) (size int64, published bool, err error) {
	finalPath, err := mediapath.Resolve(mediaDir, relPath)
	if err != nil {
		return 0, false, fmt.Errorf("resolving generated media asset: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return 0, false, fmt.Errorf("creating media asset directory: %w", err)
	}
	staged, err := stageMediaFile(scratchPath, finalPath, mediapath.GeneratedAssetTempFilePrefix)
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
	skip, err := shouldSkip(ctx, q)
	if err != nil {
		return 0, false, fmt.Errorf("rechecking generated media asset publication: %w", err)
	}
	if skip {
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

// skipSeekTilesPublish は commit tx 内で、seek tiles の公開を飛ばすべきかを返す。
// active な seek tiles が既にあるか、原本が active でなくなったときに true。
// thumbnail は skipThumbnailPlanPublish が担う。
func skipSeekTilesPublish(ctx context.Context, q *sqlcgen.Queries, recordingID int64) (bool, error) {
	return skipGeneratedPublish(ctx, q, recordingID, q.GetActiveSeekTilesMediaAssetID)
}

func skipGeneratedPublish(
	ctx context.Context, q *sqlcgen.Queries, recordingID int64,
	activeDerived func(context.Context, int64) (int64, error),
) (bool, error) {
	if _, err := activeDerived(ctx, recordingID); err == nil {
		return true, nil
	} else if !errors.Is(err, pgx5.ErrNoRows) {
		return false, err
	}
	if _, err := q.GetActiveOriginalMediaAsset(ctx, recordingID); errors.Is(err, pgx5.ErrNoRows) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}
