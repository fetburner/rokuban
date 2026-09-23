package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	pgx5 "github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/mediapath"
)

const (
	mediaRelPathLockKeyPrefix     = "rokuban:media:rel-path:"
	mediaRelPathLockRetryInterval = 25 * time.Millisecond
)

// mediaRelPathLockPath は canonical と同じディレクトリに、rel_path から安定して
// 決まる lock file のパスを返す。canonical 自体を lock file にすると orphan 回収の
// unlink で lock 対象が消えるため、削除・置換しない別名を使う。rel_path は hash 化
// するので、DB 由来の区切りや長さが filesystem の lock file 名に影響しない。
func mediaRelPathLockPath(canonicalPath, relPath string) string {
	digest := sha256.Sum256([]byte(relPath))
	return filepath.Join(filepath.Dir(canonicalPath),
		mediapath.MediaRelPathLockFilePrefix+hex.EncodeToString(digest[:])+".lock")
}

func openMediaRelPathLock(canonicalPath, relPath string) (*os.File, error) {
	path := mediaRelPathLockPath(canonicalPath, relPath)
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, fmt.Errorf("opening media rel_path lock %q: %w", path, err)
	}
	return lock, nil
}

func mediaRelPathLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

// lockMediaRelPathFile は DB セッションの寿命に依存しない canonical のファイル
// 排他を取得する。Flock を non-blocking で繰り返すことで、DB lock 待ちと同じく
// 呼び出し元の context を尊重する。返した fd を閉じるまで lock は保持される。
func lockMediaRelPathFile(ctx context.Context, canonicalPath, relPath string) (*os.File, error) {
	lock, err := openMediaRelPathLock(canonicalPath, relPath)
	if err != nil {
		return nil, err
	}

	for {
		if err := ctx.Err(); err != nil {
			_ = lock.Close()
			return nil, fmt.Errorf("waiting for media rel_path lock: %w", err)
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return lock, nil
		} else if !mediaRelPathLockBusy(err) && !errors.Is(err, syscall.EINTR) {
			_ = lock.Close()
			return nil, fmt.Errorf("locking media rel_path file: %w", err)
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
			_ = lock.Close()
			return nil, fmt.Errorf("waiting for media rel_path lock: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// tryLockMediaRelPathFile は canonical orphan 回収用の non-blocking 版。別の ingest
// または cleanup が同じ rel_path を確定中なら false を返し、次の reconcile pass に
// 委ねる。DB advisory lock と違い、DB 接続が切れてもファイル操作中のプロセスが
// lock を保持し続ける。
func tryLockMediaRelPathFile(canonicalPath, relPath string) (*os.File, bool, error) {
	lock, err := openMediaRelPathLock(canonicalPath, relPath)
	if err != nil {
		return nil, false, err
	}
	for {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			_ = lock.Close()
			if mediaRelPathLockBusy(err) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("trying media rel_path file lock: %w", err)
		}
		return lock, true, nil
	}
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
