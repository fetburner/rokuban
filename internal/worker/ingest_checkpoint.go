package worker

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
	"strings"

	"github.com/fetburner/rokuban/internal/mediapath"
)

const (
	ingestCheckpointSuffix       = ".checkpoint"
	ingestCheckpointHeader       = 17
	ingestCheckpointMaxHashState = 256
)

var ingestCheckpointMagic = [4]byte{'R', 'K', 'I', 'B'}

// ingestCheckpointPath は temp と同じ寿命を持つ SHA-256 中間状態の保存先を返す。
func ingestCheckpointPath(tempPath string) string {
	return tempPath + ingestCheckpointSuffix
}

// isIngestCheckpointFile は孤児回収が checkpoint を temp と同じ flock に結び付けるための判定。
func isIngestCheckpointFile(name string) bool {
	base := filepath.Base(name)
	return mediapath.IsIngestTempFile(base) && strings.HasSuffix(base, ingestCheckpointSuffix)
}

func ingestTempPathForCheckpoint(path string) string {
	return strings.TrimSuffix(path, ingestCheckpointSuffix)
}

// writeIngestCheckpoint は SHA-256 の状態を atomic に置き換える。
// 呼び出し元は temp の flock を保持し、先に temp を Sync 済みにする。
func writeIngestCheckpoint(tempPath string, offset int64, hasher hash.Hash) error {
	if offset < 0 {
		return fmt.Errorf("ingest checkpoint offset must be non-negative, got %d", offset)
	}
	marshaler, ok := hasher.(encoding.BinaryMarshaler)
	if !ok {
		return fmt.Errorf("ingest hash %T cannot be marshaled", hasher)
	}
	state, err := marshaler.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshaling ingest SHA-256 state: %w", err)
	}
	if len(state) == 0 || len(state) > ingestCheckpointMaxHashState {
		return fmt.Errorf("unexpected ingest SHA-256 state size %d", len(state))
	}

	payload := make([]byte, ingestCheckpointHeader+len(state)+sha256.Size)
	copy(payload[:4], ingestCheckpointMagic[:])
	payload[4] = 1
	binary.BigEndian.PutUint64(payload[5:13], uint64(offset))
	binary.BigEndian.PutUint32(payload[13:17], uint32(len(state)))
	copy(payload[ingestCheckpointHeader:], state)
	checksum := sha256.Sum256(payload[:ingestCheckpointHeader+len(state)])
	copy(payload[ingestCheckpointHeader+len(state):], checksum[:])

	checkpointPath := ingestCheckpointPath(tempPath)
	tmp, err := os.CreateTemp(filepath.Dir(checkpointPath), filepath.Base(checkpointPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating ingest checkpoint: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if err := writeAll(tmp, payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing ingest checkpoint: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing ingest checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing ingest checkpoint: %w", err)
	}
	if err := os.Rename(tmpPath, checkpointPath); err != nil {
		return fmt.Errorf("replacing ingest checkpoint: %w", err)
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

// restoreIngestCheckpoint は有効な checkpoint を hasher に復元する。
// 欠落・破損・temp より先を指す状態は通常経路として無効扱いにし、全量 replay へ戻す。
func restoreIngestCheckpoint(tempPath string, tempSize int64, hasher hash.Hash) (int64, bool) {
	data, err := os.ReadFile(ingestCheckpointPath(tempPath))
	if err != nil || len(data) < ingestCheckpointHeader+sha256.Size {
		return 0, false
	}
	if !equalBytes(data[:4], ingestCheckpointMagic[:]) || data[4] != 1 {
		return 0, false
	}
	stateSize := int(binary.BigEndian.Uint32(data[13:17]))
	if stateSize == 0 || stateSize > ingestCheckpointMaxHashState || len(data) != ingestCheckpointHeader+stateSize+sha256.Size {
		return 0, false
	}
	offset := binary.BigEndian.Uint64(data[5:13])
	if offset > uint64(tempSize) {
		return 0, false
	}
	checksumAt := ingestCheckpointHeader + stateSize
	wantChecksum := sha256.Sum256(data[:checksumAt])
	if !equalBytes(data[checksumAt:], wantChecksum[:]) {
		return 0, false
	}
	unmarshaler, ok := hasher.(encoding.BinaryUnmarshaler)
	if !ok {
		return 0, false
	}
	if err := unmarshaler.UnmarshalBinary(data[ingestCheckpointHeader:checksumAt]); err != nil {
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

// replayIngestTempFileWithCheckpoint restores the hash state and reads only bytes past its offset.
// If the slice context ends, it syncs the temp and saves the partial replay position before returning.
func replayIngestTempFileWithCheckpoint(ctx context.Context, tempPath string, temp ingestFile, hasher hash.Hash) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return 0, false, fmt.Errorf("stating ingest temporary file: %w", err)
	}
	want := info.Size()
	if want < 0 {
		return 0, false, fmt.Errorf("ingest temporary file has negative size %d", want)
	}
	offset, restored := restoreIngestCheckpoint(tempPath, want, hasher)
	if !restored {
		hasher.Reset()
		offset = 0
	}

	f, err := os.Open(tempPath)
	if err != nil {
		return offset, false, fmt.Errorf("opening ingest temporary file for replay: %w", err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return offset, false, fmt.Errorf("seeking ingest temporary file for replay: %w", err)
	}
	reader := &ingestReplayReader{ctx: ctx, r: io.LimitReader(f, want-offset)}
	n, copyErr := io.Copy(hasher, reader)
	closeErr := f.Close()
	total := offset + n

	if ctx.Err() != nil && total < want {
		if err := temp.Sync(); err != nil {
			return total, false, fmt.Errorf("syncing ingest temp after interrupted replay: %w", err)
		}
		if err := writeIngestCheckpoint(tempPath, total, hasher); err != nil {
			return total, false, fmt.Errorf("saving ingest checkpoint after interrupted replay: %w", err)
		}
		return total, false, ctx.Err()
	}
	if copyErr != nil {
		return total, false, fmt.Errorf("replaying ingest temporary file: %w", copyErr)
	}
	if closeErr != nil {
		return total, false, fmt.Errorf("closing ingest temporary file after replay: %w", closeErr)
	}
	if total != want {
		return total, false, fmt.Errorf("ingest temporary file changed during replay: read=%d size=%d", total, want)
	}
	return total, true, nil
}

func persistIngestCheckpoint(tempPath string, temp ingestFile, offset int64, hasher hash.Hash) error {
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("syncing ingest temp before checkpoint: %w", err)
	}
	if err := writeIngestCheckpoint(tempPath, offset, hasher); err != nil {
		return fmt.Errorf("saving ingest checkpoint: %w", err)
	}
	return nil
}

func isIngestSliceDeadline(err error, sliceCtx, workCtx context.Context) bool {
	return errors.Is(err, context.DeadlineExceeded) && sliceCtx.Err() == context.DeadlineExceeded && workCtx.Err() == nil
}
