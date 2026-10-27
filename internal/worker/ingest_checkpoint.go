package worker

import (
	"context"
	"errors"
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

// isIngestSliceDeadline は slice context だけが deadline で終わり、親 context は生きているかを返す。
// 4 分の区切りは ingest 固有なので fsink ではなくここに置く。
func isIngestSliceDeadline(err error, sliceCtx, workCtx context.Context) bool {
	return errors.Is(err, context.DeadlineExceeded) && sliceCtx.Err() == context.DeadlineExceeded && workCtx.Err() == nil
}
