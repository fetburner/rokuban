package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// newWorkerScratchDir は指定されたキーに乱数接尾辞を付けた scratch directory を作る。
func newWorkerScratchDir(scratchRoot, kind, prefix string) (string, error) {
	if scratchRoot == "" {
		return "", fmt.Errorf("scratch dir is empty")
	}
	root := filepath.Join(scratchRoot, kind)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating %s scratch root: %w", kind, err)
	}
	return os.MkdirTemp(root, prefix)
}

// encodeScratchScope は profile の固定長 digest を含む filesystem prefix を返す。
func encodeScratchScope(recordingID int64, profile string) string {
	profileDigest := sha256.Sum256([]byte(profile))
	return fmt.Sprintf("%d-%x", recordingID, profileDigest)
}

// openEncodeScratchGate は scratch root の作成・掃除の直列化に使う lock file を開く。
func openEncodeScratchGate(scratchRoot string) (*os.File, error) {
	if scratchRoot == "" {
		return nil, fmt.Errorf("scratch dir is empty")
	}
	if err := os.MkdirAll(scratchRoot, 0o700); err != nil {
		return nil, fmt.Errorf("creating scratch root: %w", err)
	}
	gate, err := os.OpenFile(filepath.Join(scratchRoot, ".encode-scratch.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening encode scratch gate: %w", err)
	}
	return gate, nil
}

func flockScratch(lock *os.File, operation int) error {
	for {
		err := syscall.Flock(int(lock.Fd()), operation)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// closeEncodeScratchLock は flock を解いて閉じる。dir lock と gate の両方に使う。
func closeEncodeScratchLock(lock *os.File) error {
	unlockErr := flockScratch(lock, syscall.LOCK_UN)
	closeErr := lock.Close()
	return errors.Join(unlockErr, closeErr)
}

// newEncodeScratchDir は (recording_id, profile, attempt_count) ごとの scratch を作り、
// その directory 自体に LOCK_EX を取って返す。encode は終了まで返り値の lock を持ち続ける。
//
// MkdirTemp から dir lock 取得までの間は「誰も保持していない dir」に見えるので、
// 同じ scope の removeStaleEncodeScratch に消されうる。この区間だけ gate
// （.encode-scratch.lock の LOCK_EX）で掃除と直列化して塞ぐ。gate は dir lock を
// 取ったら放し、encode 実行中は持たない（TestRemoveStaleEncodeScratchWaitsForScratchCreationGate）。
func newEncodeScratchDir(scratchRoot string, recordingID int64, profile string, attemptCount int32) (string, *os.File, error) {
	gate, err := openEncodeScratchGate(scratchRoot)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = closeEncodeScratchLock(gate) }()
	if err := flockScratch(gate, syscall.LOCK_EX); err != nil {
		return "", nil, fmt.Errorf("locking encode scratch gate: %w", err)
	}
	prefix := fmt.Sprintf("%s-%d-", encodeScratchScope(recordingID, profile), attemptCount)
	dir, err := newWorkerScratchDir(scratchRoot, "encode", prefix)
	if err != nil {
		return "", nil, fmt.Errorf("creating encode scratch directory: %w", err)
	}
	lock, err := os.Open(dir)
	if err == nil {
		err = flockScratch(lock, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			_ = lock.Close()
		}
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("locking encode scratch directory: %w", err)
	}
	return dir, lock, nil
}

// removeStaleEncodeScratch は同じ (recording_id, profile) の死んだ試行の scratch だけを
// best-effort で消す。dir lock を取れた（どのプロセスも保持していない）dir だけを消し、
// 同じ scope で並走中の試行と、他の scope の encode の有無には影響されない
// （TestRemoveStaleEncodeScratchKeepsLiveAttemptOfSameScope /
// TestRemoveStaleEncodeScratchIgnoresLiveEncodeOfOtherScope）。
// 作成中の dir を消さないよう gate で newEncodeScratchDir と直列化する。
func removeStaleEncodeScratch(scratchRoot string, recordingID int64, profile string, log *slog.Logger) {
	gate, err := openEncodeScratchGate(scratchRoot)
	if err != nil {
		return
	}
	defer func() { _ = gate.Close() }()
	if err := flockScratch(gate, syscall.LOCK_EX); err != nil {
		log.Warn("encode: stale scratch gate failed", "err", err)
		return
	}
	defer func() { _ = flockScratch(gate, syscall.LOCK_UN) }()

	root := filepath.Join(scratchRoot, "encode")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	prefix := encodeScratchScope(recordingID, profile) + "-"
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		dir, err := os.Open(path)
		if err != nil {
			continue
		}
		if err := flockScratch(dir, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = dir.Close()
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				log.Warn("encode: stale scratch lock failed", "dir", path, "err", err)
			}
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			log.Warn("encode: stale scratch cleanup failed", "dir", path, "err", err)
		}
		_ = closeEncodeScratchLock(dir)
	}
}

// newDomainScratchDir はドメインキーに対応する固定 scratch directory を、同じキーの
// 残骸があれば消して作り直す。プロセス死の残骸を回収する仕組みではない。
// CM 検出の key は attempt_count を含み、死後の次の開始で番号が進むので、消せるのは
// 停止キャンセルで同じ番号に戻った場合だけである。
func newDomainScratchDir(scratchRoot, kind, key string) (string, error) {
	if scratchRoot == "" {
		return "", fmt.Errorf("scratch dir is empty")
	}
	root := filepath.Join(scratchRoot, kind)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("creating %s scratch root: %w", kind, err)
	}
	jobDir := filepath.Join(root, key)
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
//
// 置き場の目印は呼び出し側が CreateTemp の前に 1 回照合する。
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
	discardTemp := func() {
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("media asset publisher: removing incomplete staged file", "path", tempPath, "err", err)
		}
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		discardTemp()
		return stagedMediaFile{}, fmt.Errorf("chmod staged output: %w", err)
	}
	size, err := streamCopyFile(ctx, src, tempPath)
	if err != nil {
		discardTemp()
		return stagedMediaFile{}, fmt.Errorf("staging output in media dir: %w", err)
	}
	return stagedMediaFile{tempPath: tempPath, finalPath: finalPath, size: size}, nil
}

// publish は staged 出力を canonical へ rename で公開し、親ディレクトリを fsync する。
func (s stagedMediaFile) publish() error {
	if err := os.Rename(s.tempPath, s.finalPath); err != nil {
		return fmt.Errorf("publishing staged output to canonical path: %w", err)
	}
	if err := fsink.SyncParentDir(s.finalPath); err != nil {
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

// publishGeneratedMediaAsset は完成した thumbnail / seek tiles を ingest・encode と同じ rel_path の手順で公開する。
// scratchPath を media_dir の同じディレクトリへ stage し、rel_path のファイルロックを取ってから、
// commit に size とファイル公開関数 publish を渡す。commit は store の公開メソッドを呼ぶ。
// store が検査を通せば publish を commit 前に 1 回呼ぶ。公開しないと決めたら呼ばず、published は false になる。
// ファイルロックは commit が返るまで保持する。公開しなかったとき size は 0 を返す。
func publishGeneratedMediaAsset(
	ctx context.Context,
	mediaDir, mediaStoreName, relPath, scratchPath string,
	commit func(size int64, publish func() error) (bool, error),
) (size int64, published bool, err error) {
	if err := requireMediaStoreMarker(mediaDir, mediaStoreName, "publishing generated media asset"); err != nil {
		return 0, false, err
	}
	finalPath, err := mediapath.Resolve(mediaDir, relPath)
	if err != nil {
		return 0, false, fmt.Errorf("resolving generated media asset: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return 0, false, fmt.Errorf("creating media asset directory: %w", err)
	}
	staged, err := stageMediaFile(ctx, scratchPath, finalPath, mediapath.GeneratedAssetTempFilePrefix)
	if err != nil {
		return 0, false, fmt.Errorf("staging generated media asset: %w", err)
	}
	defer staged.discard()
	if err := requireMediaStoreMarker(mediaDir, mediaStoreName, "locking generated media asset"); err != nil {
		return 0, false, err
	}
	fileLock, err := lockMediaRelPathFile(ctx, mediaDir, relPath)
	if err != nil {
		return 0, false, fmt.Errorf("locking generated media asset: %w", err)
	}
	defer func() { _ = fileLock.Close() }()
	published, err = commit(staged.size, staged.publish)
	if err != nil || !published {
		return 0, false, err
	}
	return staged.size, true, nil
}
