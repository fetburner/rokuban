package worker

import (
	"context"

	"github.com/fetburner/rokuban/internal/medialock"
)

type mediaRelPathFileLock = medialock.FileLock

func lockMediaRelPathFile(ctx context.Context, mediaDir, relPath string) (*mediaRelPathFileLock, error) {
	return medialock.Lock(ctx, mediaDir, relPath)
}
