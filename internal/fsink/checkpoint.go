package fsink

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

const (
	checkpointSuffix       = ".checkpoint"
	checkpointHeader       = 17
	checkpointMaxHashState = 8192
)

var checkpointMagic = [4]byte{'R', 'K', 'I', 'B'}

// CheckpointSuffix は temp と対応する SHA-256 checkpoint の拡張子。
const CheckpointSuffix = checkpointSuffix

// CheckpointPath は temp に対応する SHA-256 checkpoint の path を返す。
func CheckpointPath(tempPath string) string {
	return tempPath + CheckpointSuffix
}

// WriteCheckpoint は SHA-256 の状態を atomic に置き換える。
// 呼び出し元は temp の flock を保持し、先に temp を Sync 済みにする。
// check は一時ファイルの作成前に一度呼ぶ。nil なら呼ばない。
func WriteCheckpoint(tempPath string, offset int64, hasher hash.Hash, check func() error) error {
	return writeCheckpointPath(CheckpointPath(tempPath), offset, hasher, check)
}

func writeCheckpointPath(checkpointPath string, offset int64, hasher hash.Hash, check func() error) error {
	if offset < 0 {
		return fmt.Errorf("checkpoint offset must be non-negative, got %d", offset)
	}
	marshaler, ok := hasher.(encoding.BinaryMarshaler)
	if !ok {
		return fmt.Errorf("hash %T cannot be marshaled", hasher)
	}
	state, err := marshaler.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshaling SHA-256 state: %w", err)
	}
	if len(state) == 0 || len(state) > checkpointMaxHashState {
		return fmt.Errorf("unexpected SHA-256 state size %d", len(state))
	}

	payload := make([]byte, checkpointHeader+len(state)+sha256.Size)
	copy(payload[:4], checkpointMagic[:])
	payload[4] = 1
	binary.BigEndian.PutUint64(payload[5:13], uint64(offset))
	binary.BigEndian.PutUint32(payload[13:17], uint32(len(state)))
	copy(payload[checkpointHeader:], state)
	checksum := sha256.Sum256(payload[:checkpointHeader+len(state)])
	copy(payload[checkpointHeader+len(state):], checksum[:])

	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(checkpointPath), filepath.Base(checkpointPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating checkpoint: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := writeAll(tmp, payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing checkpoint: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing checkpoint: %w", err)
	}
	if err := os.Rename(tmpPath, checkpointPath); err != nil {
		return fmt.Errorf("replacing checkpoint: %w", err)
	}
	return nil
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// RestoreCheckpoint は有効な checkpoint を hasher に復元する。
// 欠落・破損・temp より先を指す状態は無効扱いにし、全量 replay へ戻す。
func RestoreCheckpoint(tempPath string, tempSize int64, hasher hash.Hash) (int64, bool) {
	return restoreCheckpointPath(CheckpointPath(tempPath), tempSize, hasher)
}

func restoreCheckpointPath(checkpointPath string, tempSize int64, hasher hash.Hash) (int64, bool) {
	data, err := os.ReadFile(checkpointPath)
	if err != nil || len(data) < checkpointHeader+sha256.Size {
		return 0, false
	}
	if !equalBytes(data[:4], checkpointMagic[:]) || data[4] != 1 {
		return 0, false
	}
	stateSize := int(binary.BigEndian.Uint32(data[13:17]))
	if stateSize == 0 || stateSize > checkpointMaxHashState || len(data) != checkpointHeader+stateSize+sha256.Size {
		return 0, false
	}
	offset := binary.BigEndian.Uint64(data[5:13])
	if offset > uint64(tempSize) {
		return 0, false
	}
	checksumAt := checkpointHeader + stateSize
	wantChecksum := sha256.Sum256(data[:checksumAt])
	if !equalBytes(data[checksumAt:], wantChecksum[:]) {
		return 0, false
	}
	unmarshaler, ok := hasher.(encoding.BinaryUnmarshaler)
	if !ok {
		return 0, false
	}
	if err := unmarshaler.UnmarshalBinary(data[checkpointHeader:checksumAt]); err != nil {
		hasher.Reset()
		return 0, false
	}
	return int64(offset), true
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ReplayTempFileWithCheckpoint は checkpoint の hash 状態を復元し、offset より後の byte だけ読む。
// context が終わった場合は temp を同期し、途中位置を checkpoint に保存してから返す。
func ReplayTempFileWithCheckpoint(ctx context.Context, tempPath string, temp File, hasher hash.Hash, check func() error) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return 0, false, fmt.Errorf("stating temp file: %w", err)
	}
	want := info.Size()
	if want < 0 {
		return 0, false, fmt.Errorf("temp file has negative size %d", want)
	}
	offset, restored := RestoreCheckpoint(tempPath, want, hasher)
	if !restored {
		hasher.Reset()
		offset = 0
	}

	f, err := os.Open(tempPath)
	if err != nil {
		return offset, false, fmt.Errorf("opening temp file for replay: %w", err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return offset, false, fmt.Errorf("seeking temp file for replay: %w", err)
	}
	reader := &replayReader{ctx: ctx, r: io.LimitReader(f, want-offset)}
	n, copyErr := io.Copy(hasher, reader)
	closeErr := f.Close()
	total := offset + n

	if ctx.Err() != nil && total < want {
		if err := temp.Sync(); err != nil {
			return total, false, fmt.Errorf("syncing temp after interrupted replay: %w", err)
		}
		if err := WriteCheckpoint(tempPath, total, hasher, check); err != nil {
			return total, false, fmt.Errorf("saving checkpoint after interrupted replay: %w", err)
		}
		return total, false, ctx.Err()
	}
	if copyErr != nil {
		return total, false, fmt.Errorf("replaying temp file: %w", copyErr)
	}
	if closeErr != nil {
		return total, false, fmt.Errorf("closing temp file after replay: %w", closeErr)
	}
	if total != want {
		return total, false, fmt.Errorf("temp file changed during replay: read=%d size=%d", total, want)
	}
	return total, true, nil
}

// ReplayFileWithCheckpoint は読み取り専用ファイルをチェックポイントからハッシュする。
// filesystem identity が前回から変わった場合は checkpoint を使わず先頭から読む。
// context が完了した場合は途中位置を保存してから返す。
func ReplayFileWithCheckpoint(ctx context.Context, path, checkpointBase string, hasher hash.Hash, check func() error) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, false, fmt.Errorf("stating file for replay: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, false, fmt.Errorf("file for replay must be regular, got %s", info.Mode())
	}
	want := info.Size()
	if want < 0 {
		return 0, false, fmt.Errorf("file for replay has negative size %d", want)
	}
	identity := fileIdentity(path, info)

	f, err := os.Open(path)
	if err != nil {
		return 0, false, fmt.Errorf("opening file for replay: %w", err)
	}
	openedInfo, statErr := f.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) {
		_ = f.Close()
		if statErr != nil {
			return 0, false, fmt.Errorf("stating opened file for replay: %w", statErr)
		}
		return 0, false, fmt.Errorf("file changed before replay")
	}
	offset, complete, replayErr := replayReaderWithCheckpoint(ctx, f, want, CheckpointPath(checkpointBase), identity, hasher, check)
	closeErr := f.Close()
	if replayErr != nil {
		return offset, false, errors.Join(replayErr, closeErr)
	}
	if closeErr != nil {
		return offset, false, fmt.Errorf("closing file after replay: %w", closeErr)
	}
	return offset, complete, nil
}

func replayReaderWithCheckpoint(ctx context.Context, source io.ReadSeeker, size int64, checkpointPath, identity string, hasher hash.Hash, check func() error) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	identified := &identifiedHash{Hash: hasher, identity: identity}
	offset, restored := restoreCheckpointPath(checkpointPath, size, identified)
	if !restored {
		hasher.Reset()
		offset = 0
	}
	if _, err := source.Seek(offset, io.SeekStart); err != nil {
		return offset, false, fmt.Errorf("seeking file for replay: %w", err)
	}
	reader := &replayReader{ctx: ctx, r: io.LimitReader(source, size-offset)}
	n, copyErr := io.Copy(identified, reader)
	total := offset + n
	if ctx.Err() != nil && total < size {
		if err := writeCheckpointPath(checkpointPath, total, identified, check); err != nil {
			return total, false, fmt.Errorf("saving checkpoint after interrupted file replay: %w", err)
		}
		return total, false, ctx.Err()
	}
	if copyErr != nil {
		return total, false, fmt.Errorf("replaying file: %w", copyErr)
	}
	if total != size {
		return total, false, fmt.Errorf("file changed during replay: read=%d size=%d", total, size)
	}
	return total, true, nil
}

// ReplayTempFile は checkpoint を使わず、既存 temp 全体を hasher に通す。
func ReplayTempFile(ctx context.Context, path string, hasher hash.Hash) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("replaying temp file: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stating temp file: %w", err)
	}
	want := info.Size()
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("opening temp file for replay: %w", err)
	}
	n, copyErr := io.Copy(hasher, &replayReader{ctx: ctx, r: f})
	closeErr := f.Close()
	if copyErr != nil {
		return n, fmt.Errorf("replaying temp file: %w", copyErr)
	}
	if closeErr != nil {
		return n, fmt.Errorf("closing temp file after replay: %w", closeErr)
	}
	if n != want {
		return n, fmt.Errorf("temp file changed during replay: read=%d size=%d", n, want)
	}
	return n, nil
}

type replayReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *replayReader) Read(p []byte) (int, error) {
	return ReadContext(r.ctx, r.r, p)
}

// ReadContext は context が完了済みなら reader を進めずにその error を返す。
func ReadContext(ctx context.Context, reader io.Reader, p []byte) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	return reader.Read(p)
}

// PersistCheckpoint は temp を同期してから hash 状態と offset を checkpoint に保存する。
func PersistCheckpoint(tempPath string, temp File, offset int64, hasher hash.Hash, check func() error) error {
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("syncing temp before checkpoint: %w", err)
	}
	if err := WriteCheckpoint(tempPath, offset, hasher, check); err != nil {
		return fmt.Errorf("saving checkpoint: %w", err)
	}
	return nil
}

// IsSliceDeadline は slice context だけが deadline で終わり、親 context は生きているかを返す。
func IsSliceDeadline(err error, sliceCtx, workCtx context.Context) bool {
	return errors.Is(err, context.DeadlineExceeded) && sliceCtx.Err() == context.DeadlineExceeded && workCtx.Err() == nil
}
