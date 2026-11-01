// Package fsink は、再開可能なファイル書き込みに必要な POSIX 操作をまとめる。
package fsink

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// File は temp へ書き込み、明示的に同期して閉じるための最小インターフェース。
type File interface {
	io.Writer
	Sync() error
	Close() error
}

// DuplicateFile は close-on-exec を原子的に設定して fd を複製する。
func DuplicateFile(file *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dup := os.NewFile(uintptr(fd), file.Name())
	if dup == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("creating duplicate temp fd")
	}
	return dup, nil
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

// RemoveTemp は ingest temp を削除する。
func RemoveTemp(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing temp file %q: %w", path, err)
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
