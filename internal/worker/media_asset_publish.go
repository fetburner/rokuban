package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	pgx5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mediapath"
)

type skipMediaAssetPublish func(context.Context, *sqlcgen.Queries) (bool, error)
type mediaAssetUpsert func(context.Context, *sqlcgen.Queries, int64) error

// newWorkerScratchDir は試行ごとに一意な scratch directory を作る。River の Timeout が
// 正のジョブはこちらを使う。timeout した試行がまだ動いている間に River の rescuer が
// 同じ job ID を再実行しうるので、試行どうしで directory を共有させない。
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

// removeStaleEncodeScratch は同じ job の前の試行が残した encode scratch を消す
// （<scratch>/encode/<jobID>-*）。best-effort。
func removeStaleEncodeScratch(scratchRoot string, jobID int64, log *slog.Logger) {
	matches, err := filepath.Glob(filepath.Join(scratchRoot, "encode", strconv.FormatInt(jobID, 10)+"-*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			log.Warn("encode: stale scratch cleanup failed", "dir", m, "err", err)
		}
	}
}

// newJobScratchDir は job ID で固定した scratch directory を、前回の残骸を消してから作る。
// River の Timeout が -1 のジョブはこちらを使う。rescuer は Timeout が負のジョブを
// 再実行しないので同じ ID の試行は重ならず、開始時の RemoveAll が異常終了した前回の
// 試行の残骸を回収する。Timeout を -1 から正の値に変えたジョブは newWorkerScratchDir へ移す。
func newJobScratchDir(scratchRoot, kind string, jobID int64) (string, error) {
	if scratchRoot == "" {
		return "", fmt.Errorf("scratch dir is empty")
	}
	root := filepath.Join(scratchRoot, kind)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating %s scratch root: %w", kind, err)
	}
	jobDir := filepath.Join(root, strconv.FormatInt(jobID, 10))
	if err := os.RemoveAll(jobDir); err != nil {
		return "", fmt.Errorf("cleaning previous %s scratch directory: %w", kind, err)
	}
	if err := os.Mkdir(jobDir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s scratch directory: %w", kind, err)
	}
	return jobDir, nil
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
func stageMediaFile(ctx context.Context, src, finalPath, prefix string) (stagedMediaFile, error) {
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
	size, err := streamCopyFile(ctx, src, tempPath)
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
	staged, err := stageMediaFile(ctx, scratchPath, finalPath, mediapath.GeneratedAssetTempFilePrefix)
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
