package worker

import (
	"context"
	"hash"
	"path/filepath"
	"strings"

	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/mediapath"
)

const ingestCheckpointSuffix = fsink.CheckpointSuffix

// ingestCheckpointPath は temp と同じ寿命を持つ SHA-256 中間状態の保存先を返す。
func ingestCheckpointPath(tempPath string) string {
	return fsink.CheckpointPath(tempPath)
}

// isIngestCheckpointFile は孤児回収が checkpoint を temp と同じ flock に結び付けるための判定。
func isIngestCheckpointFile(name string) bool {
	base := filepath.Base(name)
	return mediapath.IsIngestTempFile(base) && strings.HasSuffix(base, ingestCheckpointSuffix)
}

func ingestTempPathForCheckpoint(path string) string {
	return strings.TrimSuffix(path, ingestCheckpointSuffix)
}

// writeIngestCheckpoint は fsink の checkpoint writer への互換 shim。
func writeIngestCheckpoint(tempPath string, offset int64, hasher hash.Hash, check func() error) error {
	return fsink.WriteCheckpoint(tempPath, offset, hasher, check)
}

// restoreIngestCheckpoint は fsink の checkpoint reader への互換 shim。
func restoreIngestCheckpoint(tempPath string, tempSize int64, hasher hash.Hash) (int64, bool) {
	return fsink.RestoreCheckpoint(tempPath, tempSize, hasher)
}

// replayIngestTempFileWithCheckpoint は fsink の再開処理への互換 shim。
func replayIngestTempFileWithCheckpoint(ctx context.Context, tempPath string, temp ingestFile, hasher hash.Hash, check func() error) (int64, bool, error) {
	return fsink.ReplayTempFileWithCheckpoint(ctx, tempPath, temp, hasher, check)
}
