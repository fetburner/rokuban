// Package fsink は、再開可能なファイル書き込みに必要な POSIX 操作をまとめる。
package fsink

import (
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

// ErrForeignDestination は PublishLink の宛先に、src と別の file が既にあることを表す。
// 宛先が既存であることは os.ErrExist でも判定できる。
var ErrForeignDestination = errors.New("destination is occupied by a different file")

// SameFile は a と b がどちらも存在し、同じ inode を指すかを返す。
// symlink は辿らない。どちらかを stat できなければ false を返す。
func SameFile(a, b string) bool {
	infoA, errA := os.Lstat(a)
	infoB, errB := os.Lstat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// PublishLink は src の inode を dst の名前でも見えるようにし、src は消さない。
// 呼び出し側が DB commit の成功後に src を unlink する。
//
// 公開手段に link を選んだ理由は 2 つある。os.Rename は宛先が既存でも黙って置き換えるので、
// DB に行の無い canonical 名のファイルを失わせる。renameat2(RENAME_NOREPLACE) は Linux の
// NFS クライアントが flag 付きの rename を EINVAL で断るため、NFS 上の置き場で必ず失敗する。
// link(2) は宛先が既存なら EEXIST で失敗するので上書きしない。
//
// link(2) の BUGS 節は、NFS では link が成功していても戻り値が失敗になりうると書く。
// そのため、どの失敗でも src と dst が同じ inode かを確かめ、同じなら公開済みとして nil を返す。
// 同じ inode が既にある場合（前回の試行が link した後に commit に失敗した場合）もここで成功になる。
// 別の inode なら他人のファイルなので、何も変えずに ErrForeignDestination と os.ErrExist を包んで返す。
// JuiceFS での hard link の動作は未検証。
func PublishLink(src, dst string) error {
	if filepath.Clean(src) == filepath.Clean(dst) {
		return fmt.Errorf("source and destination paths are identical: %q", src)
	}
	err := os.Link(src, dst)
	if err == nil || SameFile(src, dst) {
		return nil
	}
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("publishing %q: destination %q already exists: %w: %w", src, dst, ErrForeignDestination, os.ErrExist)
	}
	return fmt.Errorf("linking %q to %q: %w", src, dst, err)
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
