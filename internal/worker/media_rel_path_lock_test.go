package worker

import (
	"context"
	"errors"
	"time"
)

func tryLockMediaRelPathFile(mediaDir, relPath string) (*mediaRelPathFileLock, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	lock, err := lockMediaRelPathFile(ctx, mediaDir, relPath)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return lock, true, nil
}
