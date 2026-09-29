package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	pgx5 "github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/mediapath"
)

const (
	mediaRelPathLockKeyPrefix     = "rokuban:media:rel-path:"
	mediaRelPathLockRetryInterval = 25 * time.Millisecond
	mediaRelPathLockGateFile      = ".gate.lock"
)

var errMediaRelPathLockGateBusy = errors.New("media rel_path lock gate is busy")

type mediaRelPathFileLock struct {
	file     *os.File
	mediaDir string
	path     string

	closeOnce sync.Once
	closeErr  error
}

// mediaRelPathLockPath は media root の専用 lock directory に、rel_path から安定して
// 決まる lock file のパスを返す。canonical file の rename / unlink と同じディレクトリ
// entry を共有しない。rel_path は hash 化し、DB 由来の区切りや長さを file 名に持ち込まない。
func mediaRelPathLockPath(mediaDir, relPath string) string {
	digest := sha256.Sum256([]byte(relPath))
	return filepath.Join(mediaDir, mediapath.MediaRelPathLockDirName,
		mediapath.MediaRelPathLockFilePrefix+hex.EncodeToString(digest[:])+".lock")
}

func mediaRelPathLockGatePath(mediaDir string) string {
	return filepath.Join(mediaDir, mediapath.MediaRelPathLockDirName, mediaRelPathLockGateFile)
}

func ensureMediaRelPathLockDir(mediaDir string) (string, error) {
	info, err := os.Stat(mediaDir)
	if err != nil {
		return "", fmt.Errorf("stating media directory for rel_path lock: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("media directory for rel_path lock %q is not a directory", mediaDir)
	}
	lockDir := filepath.Join(mediaDir, mediapath.MediaRelPathLockDirName)
	if err := os.Mkdir(lockDir, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("creating media rel_path lock directory %q: %w", lockDir, err)
	}
	lockInfo, err := os.Lstat(lockDir)
	if err != nil {
		return "", fmt.Errorf("stating media rel_path lock directory %q: %w", lockDir, err)
	}
	if !lockInfo.IsDir() {
		return "", fmt.Errorf("media rel_path lock path %q is not a directory", lockDir)
	}
	return lockDir, nil
}

func acquireMediaRelPathLockGate(ctx context.Context, mediaDir string, exclusive, wait bool) (*os.File, error) {
	lockDir, err := ensureMediaRelPathLockDir(mediaDir)
	if err != nil {
		return nil, err
	}
	gatePath := filepath.Join(lockDir, mediaRelPathLockGateFile)
	gate, err := os.OpenFile(gatePath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o666)
	if err != nil {
		return nil, fmt.Errorf("opening media rel_path lock gate %q: %w", gatePath, err)
	}
	if err := requireRegularMediaRelPathLockFile(gate, gatePath); err != nil {
		_ = gate.Close()
		return nil, err
	}

	operation := syscall.LOCK_SH
	if exclusive {
		operation = syscall.LOCK_EX
	}
	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				_ = gate.Close()
				return nil, fmt.Errorf("waiting for media rel_path lock gate: %w", err)
			}
		}
		flags := operation | syscall.LOCK_NB
		if err := syscall.Flock(int(gate.Fd()), flags); err == nil {
			return gate, nil
		} else if errors.Is(err, syscall.EINTR) {
			continue
		} else if mediaRelPathLockBusy(err) {
			if !wait {
				_ = gate.Close()
				return nil, errMediaRelPathLockGateBusy
			}
			timer := time.NewTimer(mediaRelPathLockRetryInterval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				_ = gate.Close()
				return nil, fmt.Errorf("waiting for media rel_path lock gate: %w", ctx.Err())
			case <-timer.C:
			}
		} else {
			_ = gate.Close()
			return nil, fmt.Errorf("locking media rel_path lock gate: %w", err)
		}
	}
}

func releaseMediaRelPathLockGate(gate *os.File) error {
	if gate == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(gate.Fd()), syscall.LOCK_UN)
	closeErr := gate.Close()
	return errors.Join(unlockErr, closeErr)
}

func validMediaRelPathLockFileName(name string) bool {
	base := filepath.Base(name)
	if !mediapath.IsMediaRelPathLockFile(base) || !strings.HasSuffix(base, ".lock") {
		return false
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(base, mediapath.MediaRelPathLockFilePrefix), ".lock")
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func requireRegularMediaRelPathLockFile(file *os.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stating media rel_path lock file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("media rel_path lock path %q is not a regular file", path)
	}
	return nil
}

// gcMediaRelPathLockFiles は gate を排他して stale lock file を消す。全取得側は
// gate 共有中だけ lock file を開いて non-blocking flock し、競合時は fd を閉じてから
// 待つため、GC が始まる時点で active owner 以外の古い inode を握る waiter は存在しない。
// active lock は flock が取れず残る。gate を取れなかった場合は (false, nil) を返す。
func gcMediaRelPathLockFiles(ctx context.Context, mediaDir string, wait bool) (collected bool, resultErr error) {
	gate, err := acquireMediaRelPathLockGate(ctx, mediaDir, true, wait)
	if err != nil {
		if errors.Is(err, errMediaRelPathLockGateBusy) {
			return false, nil
		}
		return false, err
	}
	collected = true
	defer func() { resultErr = errors.Join(resultErr, releaseMediaRelPathLockGate(gate)) }()

	lockDir := filepath.Dir(gate.Name())
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		return true, fmt.Errorf("reading media rel_path lock directory %q: %w", lockDir, err)
	}
	for _, entry := range entries {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return true, fmt.Errorf("collecting media rel_path lock files: %w", err)
			}
		}
		if entry.IsDir() || !validMediaRelPathLockFileName(entry.Name()) {
			continue
		}
		path := filepath.Join(lockDir, entry.Name())
		lock, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return true, fmt.Errorf("opening stale media rel_path lock %q: %w", path, err)
		}
		if err := requireRegularMediaRelPathLockFile(lock, path); err != nil {
			_ = lock.Close()
			return true, err
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lock.Close()
			if mediaRelPathLockBusy(err) || errors.Is(err, syscall.EINTR) {
				continue
			}
			return true, fmt.Errorf("locking stale media rel_path lock %q: %w", path, err)
		}
		removeErr := os.Remove(path)
		unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		closeErr := lock.Close()
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return true, fmt.Errorf("removing stale media rel_path lock %q: %w", path, removeErr)
		}
		if err := errors.Join(unlockErr, closeErr); err != nil {
			return true, fmt.Errorf("releasing stale media rel_path lock %q: %w", path, err)
		}
	}
	return true, nil
}

func openMediaRelPathLock(mediaDir, relPath string) (*os.File, error) {
	path := mediaRelPathLockPath(mediaDir, relPath)
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o666)
	if err != nil {
		return nil, fmt.Errorf("opening media rel_path lock %q: %w", path, err)
	}
	if err := requireRegularMediaRelPathLockFile(lock, path); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func mediaRelPathLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

func waitMediaRelPathLockRetry(ctx context.Context) error {
	timer := time.NewTimer(mediaRelPathLockRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// lockMediaRelPathFile は DB セッションの寿命に依存しない canonical のファイル
// 排他を取得する。busy 時は gate 共有中の non-blocking 試行で fd を閉じてから待つ。
// これにより Close/GC が lock file を unlink するとき、古い inode 上の waiter が残らない。
func lockMediaRelPathFile(ctx context.Context, mediaDir, relPath string) (*mediaRelPathFileLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := gcMediaRelPathLockFiles(ctx, mediaDir, true); err != nil {
		return nil, fmt.Errorf("collecting stale media rel_path locks: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("waiting for media rel_path lock: %w", err)
		}
		gate, err := acquireMediaRelPathLockGate(ctx, mediaDir, false, true)
		if err != nil {
			return nil, err
		}
		lock, err := openMediaRelPathLock(mediaDir, relPath)
		if err != nil {
			gateErr := releaseMediaRelPathLockGate(gate)
			return nil, errors.Join(err, gateErr)
		}
		lockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			if err := releaseMediaRelPathLockGate(gate); err != nil {
				unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				closeErr := lock.Close()
				return nil, errors.Join(fmt.Errorf("releasing media rel_path lock gate: %w", err), unlockErr, closeErr)
			}
			return &mediaRelPathFileLock{file: lock, mediaDir: mediaDir, path: mediaRelPathLockPath(mediaDir, relPath)}, nil
		}
		closeErr := lock.Close()
		gateErr := releaseMediaRelPathLockGate(gate)
		if errors.Is(lockErr, syscall.EINTR) {
			continue
		}
		if !mediaRelPathLockBusy(lockErr) {
			return nil, errors.Join(fmt.Errorf("locking media rel_path file: %w", lockErr), closeErr, gateErr)
		}
		if err := errors.Join(closeErr, gateErr); err != nil {
			return nil, fmt.Errorf("closing busy media rel_path lock attempt: %w", err)
		}
		if err := waitMediaRelPathLockRetry(ctx); err != nil {
			return nil, fmt.Errorf("waiting for media rel_path lock: %w", err)
		}
	}
}

// tryLockMediaRelPathFile は canonical orphan 回収用の non-blocking 版。GC gate または
// rel_path lock が競合中なら false を返し、次の reconcile pass に委ねる。
func tryLockMediaRelPathFile(mediaDir, relPath string) (*mediaRelPathFileLock, bool, error) {
	for {
		if _, err := gcMediaRelPathLockFiles(nil, mediaDir, false); err != nil {
			return nil, false, err
		}
		gate, err := acquireMediaRelPathLockGate(nil, mediaDir, false, false)
		if err != nil {
			if errors.Is(err, errMediaRelPathLockGateBusy) {
				return nil, false, nil
			}
			return nil, false, err
		}
		lock, err := openMediaRelPathLock(mediaDir, relPath)
		if err != nil {
			return nil, false, errors.Join(err, releaseMediaRelPathLockGate(gate))
		}
		if lockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
			closeErr := lock.Close()
			gateErr := releaseMediaRelPathLockGate(gate)
			if errors.Is(lockErr, syscall.EINTR) {
				if err := errors.Join(closeErr, gateErr); err != nil {
					return nil, false, err
				}
				continue
			}
			if mediaRelPathLockBusy(lockErr) {
				if combined := errors.Join(closeErr, gateErr); combined != nil {
					return nil, false, fmt.Errorf("closing busy media rel_path lock attempt: %w", combined)
				}
				return nil, false, nil
			}
			return nil, false, errors.Join(fmt.Errorf("trying media rel_path file lock: %w", lockErr), closeErr, gateErr)
		}
		if err := releaseMediaRelPathLockGate(gate); err != nil {
			unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			closeErr := lock.Close()
			return nil, false, errors.Join(fmt.Errorf("releasing media rel_path lock gate: %w", err), unlockErr, closeErr)
		}
		return &mediaRelPathFileLock{file: lock, mediaDir: mediaDir, path: mediaRelPathLockPath(mediaDir, relPath)}, true, nil
	}
}

func (l *mediaRelPathFileLock) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		gate, err := acquireMediaRelPathLockGate(context.Background(), l.mediaDir, true, true)
		if err != nil {
			unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
			closeErr := l.file.Close()
			l.closeErr = errors.Join(fmt.Errorf("acquiring media rel_path lock gate for cleanup: %w", err), unlockErr, closeErr)
			return
		}
		removeErr := os.Remove(l.path)
		unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		closeErr := l.file.Close()
		gateErr := releaseMediaRelPathLockGate(gate)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		l.closeErr = errors.Join(removeErr, unlockErr, closeErr, gateErr)
	})
	return l.closeErr
}

// lockMediaRelPathInTransaction は canonical file の公開と orphan 回収が共有する
// transaction-level advisory lock を取得する。転送全体ではなく、DB の media_asset
// INSERT と temp -> canonical rename / orphan unlink の短い確定区間だけを直列化する。
// 同じ rel_path の ingest commit と orphan unlink が同時に進むと、片方がもう片方の
// canonical file を消せるため、両者は必ず同じキーを使う。
func lockMediaRelPathInTransaction(ctx context.Context, tx pgx5.Tx, relPath string) error {
	key := advisoryLockKey(mediaRelPathLockKeyPrefix, relPath)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		return fmt.Errorf("acquiring media rel_path advisory lock: %w", err)
	}
	return nil
}

// tryLockMediaRelPathInTransaction は orphan 回収用の非 blocking 版。ingest の
// commit が同じ rel_path を公開中なら、回収側は待ち続けず次の reconcile pass に
// 委ねる。
func tryLockMediaRelPathInTransaction(ctx context.Context, tx pgx5.Tx, relPath string) (bool, error) {
	key := advisoryLockKey(mediaRelPathLockKeyPrefix, relPath)
	var acquired bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("trying media rel_path advisory lock: %w", err)
	}
	return acquired, nil
}
