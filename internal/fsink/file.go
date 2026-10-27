// Package fsink は、再開可能なファイル書き込みに必要な POSIX 操作をまとめる。
package fsink

import (
	"bytes"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// File は temp へ書き込み、明示的に同期して閉じるための最小インターフェース。
type File interface {
	io.Writer
	Sync() error
	Close() error
}

// HashingWriter は下流 writer が受理したバイトだけを hash に流す。
//
// 下流が (n > 0, err != nil) の部分書き込みを返しても、次の再開位置はその n バイトの直後になる。
// io.MultiWriter と違い、受理された prefix は下流エラーがあっても hash に記録される。
type HashingWriter struct {
	w io.Writer
	h hash.Hash
}

// NewHashingWriter は、受理したバイトを hash に反映する writer を作る。
func NewHashingWriter(w io.Writer, h hash.Hash) *HashingWriter {
	return &HashingWriter{w: w, h: h}
}

// Write は下流が受理した prefix だけを hash に書く。
func (w *HashingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		_, hashErr := w.h.Write(p[:n])
		if err == nil {
			err = hashErr
		}
	}
	return n, err
}

// OpenFileFromLock は flock 済み temp fd と同じ open file description を複製して返す。
// dup により writer の Close 後も元 fd が flock を保持する。
func OpenFileFromLock(locked *os.File) (File, error) {
	return DuplicateFile(locked)
}

// DuplicateFile は close-on-exec を原子的に設定して fd を複製する。
func DuplicateFile(locked *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(locked.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dup := os.NewFile(uintptr(fd), locked.Name())
	if dup == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("creating duplicate temp fd")
	}
	return dup, nil
}

// LockTempFile は O_APPEND の temp を作成または開き、非待機の排他 flock を取る。
// 返す bool はこの呼び出しが temp を作成したかを示す。
func LockTempFile(path string) (*os.File, bool, error) {
	return lockTempFile(path, true)
}

// LockExistingTempFile は既存 temp だけを O_APPEND で開き、非待機の排他 flock を取る。
// 存在しないときに空ファイルを作らないため、回収側が未作成 temp を誤認しない。
func LockExistingTempFile(path string) (*os.File, error) {
	lock, _, err := lockTempFile(path, false)
	return lock, err
}

func lockTempFile(path string, create bool) (*os.File, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		flags := os.O_RDWR | os.O_APPEND
		created := false
		var lock *os.File
		var err error
		if create {
			lock, err = os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, 0o666)
			if err == nil {
				created = true
			} else if errors.Is(err, os.ErrExist) {
				lock, err = os.OpenFile(path, flags, 0o666)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
			}
		} else {
			lock, err = os.OpenFile(path, flags, 0o666)
		}
		if err != nil {
			return nil, false, err
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lock.Close()
			return nil, false, err
		}

		lockedInfo, err := lock.Stat()
		pathInfo, pathErr := os.Stat(path)
		if err == nil && pathErr == nil && os.SameFile(lockedInfo, pathInfo) {
			return lock, created, nil
		}

		// open と flock の間に path が unlink / rename された場合、見えない inode を返さず取り直す。
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) {
			return nil, false, pathErr
		}
		if err != nil {
			return nil, false, err
		}
	}
	return nil, false, fmt.Errorf("temp file %q was replaced while acquiring its lock", path)
}

// TempLockBusy は err が non-blocking flock の競合を表すか返す。
func TempLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

// RemoveTemp は temp と対応する checkpoint を削除し、両方の削除エラーを返す。
func RemoveTemp(path string) error {
	var errs []error
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("removing temp file %q: %w", path, err))
	}
	if err := RemoveCheckpoint(path); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// RemoveCheckpoint は temp に対応する SHA-256 checkpoint を削除する。
func RemoveCheckpoint(tempPath string) error {
	checkpointPath := CheckpointPath(tempPath)
	if err := os.Remove(checkpointPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing checkpoint %q: %w", checkpointPath, err)
	}
	return nil
}

// RenameNoReplace は dst を既存ファイルで上書きせずに公開する。
// os.Rename は既存 dst を置き換えるため、予約済みの canonical file を失わせる可能性がある。
// dst が存在し src と内容が一致する場合は、前回の rename 後に DB commit が失敗した再試行として src を削除し成功する。
// Linux では renameat2(RENAME_NOREPLACE) を使い、対象 filesystem が flag を実装しない場合は失敗する。
// NFS での flag 対応は未検証。非 Linux の開発環境では hard link と unlink を使う。
func RenameNoReplace(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return fmt.Errorf("source and destination paths are identical: %q", src)
	}
	if err := renameNoReplace(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}

	same, compareErr := filesEqual(src, dst)
	if compareErr != nil {
		return errors.Join(errDestinationExists(src, dst), fmt.Errorf("comparing existing destination: %w", compareErr))
	}
	if !same {
		return errDestinationExists(src, dst)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("removing duplicate temp after matching destination: %w", err)
	}
	return nil
}

func errDestinationExists(src, dst string) error {
	return fmt.Errorf("publishing %q: destination %q already exists: %w", src, dst, os.ErrExist)
}

func filesEqual(src, dst string) (bool, error) {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return false, fmt.Errorf("stating source %q: %w", src, err)
	}
	dstInfo, err := os.Lstat(dst)
	if err != nil {
		return false, fmt.Errorf("stating destination %q: %w", dst, err)
	}
	if !srcInfo.Mode().IsRegular() || !dstInfo.Mode().IsRegular() {
		return false, fmt.Errorf("source and destination must be regular files: source=%s destination=%s", srcInfo.Mode(), dstInfo.Mode())
	}
	if srcInfo.Size() != dstInfo.Size() {
		return false, nil
	}
	if os.SameFile(srcInfo, dstInfo) {
		return true, nil
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return false, fmt.Errorf("opening source %q for comparison: %w", src, err)
	}
	dstFile, err := os.Open(dst)
	if err != nil {
		return false, errors.Join(fmt.Errorf("opening destination %q for comparison: %w", dst, err), srcFile.Close())
	}

	const chunkSize = 64 * 1024
	srcBuf := make([]byte, chunkSize)
	dstBuf := make([]byte, chunkSize)
	remaining := srcInfo.Size()
	equal := true
	var compareErr error
	for remaining > 0 {
		n := min(remaining, int64(chunkSize))
		if _, err := io.ReadFull(srcFile, srcBuf[:n]); err != nil {
			compareErr = fmt.Errorf("reading source %q for comparison: %w", src, err)
			break
		}
		if _, err := io.ReadFull(dstFile, dstBuf[:n]); err != nil {
			compareErr = fmt.Errorf("reading destination %q for comparison: %w", dst, err)
			break
		}
		if !bytes.Equal(srcBuf[:n], dstBuf[:n]) {
			equal = false
			break
		}
		remaining -= n
	}
	closeErr := errors.Join(srcFile.Close(), dstFile.Close())
	if err := errors.Join(compareErr, closeErr); err != nil {
		return false, err
	}
	return equal, nil
}

// SyncParentDir は path の親 directory entry を fsync する。
func SyncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("opening parent directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("syncing parent directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("closing parent directory: %w", err)
	}
	return nil
}
