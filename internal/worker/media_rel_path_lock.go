package worker

import (
	"context"

	"github.com/fetburner/rokuban/internal/medialock"
)

type mediaRelPathFileLock = medialock.FileLock

func lockMediaRelPathFile(ctx context.Context, mediaDir, relPath string) (*mediaRelPathFileLock, error) {
	return medialock.Lock(ctx, mediaDir, relPath)
}

func tryLockMediaRelPathFile(mediaDir, relPath string) (*mediaRelPathFileLock, bool, error) {
	return medialock.TryLock(mediaDir, relPath)
}
