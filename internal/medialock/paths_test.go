package medialock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLockPathsDeduplicatesAndReleasesOnCancellation(t *testing.T) {
	mediaDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := LockPaths(ctx, mediaDir, []string{"b.m2ts", "a.m2ts", "a.m2ts"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	for _, path := range []string{"a.m2ts", "b.m2ts"} {
		lock, acquired, err := tryLockForTest(mediaDir, path)
		if lock != nil {
			_ = lock.Close()
		}
		if err != nil || acquired {
			t.Fatalf("path %q was not locked: acquired=%v err=%v", path, acquired, err)
		}
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	held, err := Lock(ctx, mediaDir, "b.m2ts")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	canceledCtx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	_, err = LockPaths(canceledCtx, mediaDir, []string{"a.m2ts", "b.m2ts"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled acquisition = %v", err)
	}
	lock, acquired, err := tryLockForTest(mediaDir, "a.m2ts")
	if lock != nil {
		defer func() { _ = lock.Close() }()
	}
	if err != nil || !acquired {
		t.Fatalf("partial acquisition leaked: acquired=%v err=%v", acquired, err)
	}
}
