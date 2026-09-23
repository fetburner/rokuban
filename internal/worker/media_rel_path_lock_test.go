package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMediaRelPathFileLock_SerializesAndHonorsContext(t *testing.T) {
	mediaDir := t.TempDir()
	canonicalPath := filepath.Join(mediaDir, "recording.m2ts")
	const relPath = "sites/default/recording.m2ts"

	first, err := lockMediaRelPathFile(context.Background(), canonicalPath, relPath)
	if err != nil {
		t.Fatalf("locking first rel_path file: %v", err)
	}

	second, acquired, err := tryLockMediaRelPathFile(canonicalPath, relPath)
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

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	blocked, err := lockMediaRelPathFile(ctx, canonicalPath, relPath)
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
	second, acquired, err = tryLockMediaRelPathFile(canonicalPath, relPath)
	if err != nil {
		t.Fatalf("trying rel_path file lock after release: %v", err)
	}
	if !acquired || second == nil {
		t.Fatal("rel_path file lock was not available after first lock was released")
	}
	if err := second.Close(); err != nil {
		t.Fatalf("closing second rel_path file lock: %v", err)
	}
}

func TestWalkMediaFiles_IgnoresRelPathLockFiles(t *testing.T) {
	mediaDir := t.TempDir()
	canonicalPath := filepath.Join(mediaDir, "recording.m2ts")
	const relPath = "recording.m2ts"
	if err := os.WriteFile(canonicalPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("writing canonical file: %v", err)
	}
	lockPath := mediaRelPathLockPath(canonicalPath, relPath)
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("writing rel_path lock file: %v", err)
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
