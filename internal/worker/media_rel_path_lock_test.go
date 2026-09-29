package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMediaRelPathFileLock_SerializesAndHonorsContext(t *testing.T) {
	mediaDir := t.TempDir()
	const relPath = "sites/default/recording.m2ts"
	lockPath := mediaRelPathLockPath(mediaDir, relPath)

	first, err := lockMediaRelPathFile(context.Background(), mediaDir, relPath)
	if err != nil {
		t.Fatalf("locking first rel_path file: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("stat active rel_path lock: %v", err)
	}

	second, acquired, err := tryLockMediaRelPathFile(mediaDir, relPath)
	if err != nil {
		_ = first.Close()
		t.Fatalf("trying second rel_path file lock: %v", err)
	}
	if acquired || second != nil {
		if second != nil {
			_ = second.Close()
		}
		_ = first.Close()
		t.Fatal("second rel_path file lock was acquired while first lock was held")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	blocked, err := lockMediaRelPathFile(ctx, mediaDir, relPath)
	if blocked != nil {
		_ = blocked.Close()
		_ = first.Close()
		t.Fatal("blocking rel_path lock returned a file after context cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		_ = first.Close()
		t.Fatalf("blocking rel_path lock error = %v, want context deadline exceeded", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("closing first rel_path file lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock path after release: stat error = %v, want not exist", err)
	}
	second, acquired, err = tryLockMediaRelPathFile(mediaDir, relPath)
	if err != nil {
		t.Fatalf("trying rel_path file lock after release: %v", err)
	}
	if !acquired || second == nil {
		t.Fatal("rel_path file lock was not available after first lock was released")
	}
	if err := second.Close(); err != nil {
		t.Fatalf("closing second rel_path file lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock path after second release: stat error = %v, want not exist", err)
	}
}

func TestMediaRelPathFileLock_RemovesSequentialAndParallelLockFiles(t *testing.T) {
	mediaDir := t.TempDir()
	for i := 0; i < 12; i++ {
		relPath := filepath.ToSlash(filepath.Join("sequential", fmt.Sprintf("%02d", i), "recording.m2ts"))
		lock, err := lockMediaRelPathFile(context.Background(), mediaDir, relPath)
		if err != nil {
			t.Fatalf("locking sequential rel_path %q: %v", relPath, err)
		}
		lockPath := mediaRelPathLockPath(mediaDir, relPath)
		if _, err := os.Stat(lockPath); err != nil {
			t.Fatalf("stat sequential active lock %q: %v", lockPath, err)
		}
		if err := lock.Close(); err != nil {
			t.Fatalf("closing sequential lock %q: %v", relPath, err)
		}
		if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sequential lock %q remains after close: stat error = %v", lockPath, err)
		}
	}

	const parallel = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, parallel)
	for i := 0; i < parallel; i++ {
		relPath := filepath.ToSlash(filepath.Join("parallel", fmt.Sprintf("%02d", i), "recording.m2ts"))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lock, err := lockMediaRelPathFile(ctx, mediaDir, relPath)
			if err != nil {
				errCh <- err
				return
			}
			if err := lock.Close(); err != nil {
				errCh <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("parallel lock lifecycle: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(mediaDir, ".rokuban-locks"))
	if err != nil {
		t.Fatalf("reading lock directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != mediaRelPathLockGateFile {
		t.Errorf("lock directory entries after sequential and parallel lifecycles = %v, want only persistent gate %q", entryNames(entries), mediaRelPathLockGateFile)
	}
}

func TestMediaRelPathFileLock_WaiterReopensAfterOwnerUnlinks(t *testing.T) {
	mediaDir := t.TempDir()
	const relPath = "race/reused.m2ts"
	lockPath := mediaRelPathLockPath(mediaDir, relPath)
	first, err := lockMediaRelPathFile(context.Background(), mediaDir, relPath)
	if err != nil {
		t.Fatalf("locking first owner: %v", err)
	}

	started := make(chan struct{})
	secondDone := make(chan *mediaRelPathFileLock, 1)
	secondErr := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		close(started)
		second, err := lockMediaRelPathFile(ctx, mediaDir, relPath)
		if err != nil {
			secondErr <- err
			return
		}
		secondDone <- second
	}()
	<-started
	select {
	case lock := <-secondDone:
		_ = lock.Close()
		t.Fatal("second owner acquired while first owner still held the lock")
	case err := <-secondErr:
		t.Fatalf("second owner failed before first release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := first.Close(); err != nil {
		t.Fatalf("closing first owner: %v", err)
	}
	var second *mediaRelPathFileLock
	select {
	case second = <-secondDone:
	case err := <-secondErr:
		t.Fatalf("second owner after first release: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("second owner did not acquire after first release")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("stat second owner's current lock path: %v", err)
	}
	third, acquired, err := tryLockMediaRelPathFile(mediaDir, relPath)
	if err != nil {
		_ = second.Close()
		t.Fatalf("trying third owner while second holds lock: %v", err)
	}
	if acquired {
		_ = third.Close()
		_ = second.Close()
		t.Fatal("third owner acquired a second inode while second owner held the lock")
	}
	if err := second.Close(); err != nil {
		t.Fatalf("closing second owner: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock path after final owner: stat error = %v, want not exist", err)
	}
}

func TestMediaRelPathLockGC_RemovesOnlyUnlockedEntries(t *testing.T) {
	mediaDir := t.TempDir()
	const activeRelPath = "active/recording.m2ts"
	const staleRelPath = "stale/recording.m2ts"
	active, err := lockMediaRelPathFile(context.Background(), mediaDir, activeRelPath)
	if err != nil {
		t.Fatalf("locking active rel_path: %v", err)
	}
	defer func() { _ = active.Close() }()
	activePath := mediaRelPathLockPath(mediaDir, activeRelPath)
	stalePath := mediaRelPathLockPath(mediaDir, staleRelPath)
	if err := os.WriteFile(stalePath, nil, 0o666); err != nil {
		t.Fatalf("creating simulated crash residue: %v", err)
	}

	collected, err := gcMediaRelPathLockFiles(context.Background(), mediaDir, true)
	if err != nil {
		t.Fatalf("collecting stale rel_path locks: %v", err)
	}
	if !collected {
		t.Fatal("GC did not acquire its exclusive gate")
	}
	if _, err := os.Stat(stalePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale lock path after GC: stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Errorf("active lock path after GC: %v", err)
	}
	if _, err := os.Stat(mediaRelPathLockGatePath(mediaDir)); err != nil {
		t.Errorf("persistent GC gate after collection: %v", err)
	}
}

func TestWalkMediaFiles_IgnoresRelPathLockDirectoryAndLegacyFiles(t *testing.T) {
	mediaDir := t.TempDir()
	canonicalPath := filepath.Join(mediaDir, "recording.m2ts")
	const relPath = "recording.m2ts"
	if err := os.WriteFile(canonicalPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("writing canonical file: %v", err)
	}
	lockPath := mediaRelPathLockPath(mediaDir, relPath)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("creating lock directory: %v", err)
	}
	if err := os.WriteFile(mediaRelPathLockGatePath(mediaDir), nil, 0o666); err != nil {
		t.Fatalf("writing GC gate: %v", err)
	}
	if err := os.WriteFile(lockPath, nil, 0o666); err != nil {
		t.Fatalf("writing rel_path lock file: %v", err)
	}
	legacyPath := filepath.Join(mediaDir, ".rokuban-rel-path-lock-legacy.lock")
	if err := os.WriteFile(legacyPath, nil, 0o666); err != nil {
		t.Fatalf("writing legacy rel_path lock file: %v", err)
	}

	var got []string
	if err := walkMediaFiles(mediaDir, func(path string, _ os.FileInfo) {
		got = append(got, path)
	}); err != nil {
		t.Fatalf("walking media files: %v", err)
	}
	if len(got) != 1 || got[0] != relPath {
		t.Fatalf("walked files = %v, want only %q", got, relPath)
	}
}

func TestMediaRelPathFileLock_ParallelSamePathIsSerialized(t *testing.T) {
	mediaDir := t.TempDir()
	const relPath = "parallel/shared.m2ts"
	const workers = 6
	var active atomic.Int32
	var maxActive atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lock, err := lockMediaRelPathFile(ctx, mediaDir, relPath)
			if err != nil {
				errCh <- err
				return
			}
			current := active.Add(1)
			for previous := maxActive.Load(); current > previous && !maxActive.CompareAndSwap(previous, current); previous = maxActive.Load() {
			}
			time.Sleep(10 * time.Millisecond)
			active.Add(-1)
			if err := lock.Close(); err != nil {
				errCh <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("parallel same-path lock: %v", err)
	}
	if got := maxActive.Load(); got != 1 {
		t.Errorf("maximum same-path critical sections = %d, want 1", got)
	}
	if _, err := os.Stat(mediaRelPathLockPath(mediaDir, relPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file after parallel lifecycle: stat error = %v, want not exist", err)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
