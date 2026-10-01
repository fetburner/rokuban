package medialock

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/fetburner/rokuban/internal/mediapath"
)

// LockPaths locks distinct canonical relative paths in sorted order, before any
// database transaction is opened. Callers must hold the result through commit.
func LockPaths(ctx context.Context, mediaDir string, paths []string) (func() error, error) {
	paths = slices.Clone(paths)
	for _, path := range paths {
		if _, err := mediapath.Resolve(mediaDir, path); err != nil {
			return nil, err
		}
		if path == "." || path != filepath.ToSlash(filepath.Clean(path)) {
			return nil, fmt.Errorf("media lock path %q is not canonical", path)
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	locks := make([]*FileLock, 0, len(paths))
	release := func() error {
		var err error
		for i := len(locks) - 1; i >= 0; i-- {
			err = errors.Join(err, locks[i].Close())
		}
		return err
	}
	// GC は 1 回だけ。保持済みの lock は GC が flock に失敗して残すので、取得ごとに回す必要がない。
	if _, err := gcMediaRelPathLockFiles(ctx, mediaDir, true); err != nil {
		return nil, fmt.Errorf("collecting stale media rel_path locks: %w", err)
	}
	for _, path := range paths {
		lock, err := lockNoGC(ctx, mediaDir, path)
		if err != nil {
			return nil, errors.Join(err, release())
		}
		locks = append(locks, lock)
	}
	return release, nil
}
