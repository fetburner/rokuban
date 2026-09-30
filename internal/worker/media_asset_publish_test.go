package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pgx5 "github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

func TestStageMediaAssetDoesNotModifyCanonicalWhileRelPathIsLocked(t *testing.T) {
	mediaDir := t.TempDir()
	const relPath = "thumbnails/925.jpg"
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	priorBytes := []byte("previous canonical bytes")
	if err := os.WriteFile(canonicalPath, priorBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	scratchPath := filepath.Join(t.TempDir(), "thumbnail.jpg")
	wantBytes := []byte("new complete thumbnail")
	if err := os.WriteFile(scratchPath, wantBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := lockMediaRelPathFile(context.Background(), mediaDir, relPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()

	staged, err := stageMediaAsset(scratchPath, canonicalPath)
	if err != nil {
		t.Fatalf("stageMediaAsset: %v", err)
	}
	defer staged.discard()
	got, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priorBytes) {
		t.Fatalf("canonical bytes changed before publication lock: got %q, want %q", got, priorBytes)
	}
	if staged.tempPath == canonicalPath {
		t.Fatalf("staging path aliases canonical path %q", canonicalPath)
	}
	got, err = os.ReadFile(staged.tempPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantBytes) {
		t.Fatalf("staged bytes = %q, want %q", got, wantBytes)
	}
}

func TestPublishGeneratedMediaAssetDoesNotTouchCanonicalBeforeRelPathLock(t *testing.T) {
	pool := setupTestPool(t)
	mediaDir := t.TempDir()
	recordingID := seedRecordingWithOriginal(t, pool, mediaDir, "sites/default/source.m2ts", nil, []byte("original"))
	const relPath = "thumbnails/925.jpg"
	canonicalPath := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(canonicalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	priorBytes := []byte("previous orphan bytes")
	if err := os.WriteFile(canonicalPath, priorBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	scratchPath := filepath.Join(t.TempDir(), "thumbnail.jpg")
	wantBytes := []byte("new complete thumbnail")
	if err := os.WriteFile(scratchPath, wantBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	lock, err := lockMediaRelPathFile(context.Background(), mediaDir, relPath)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_ = lock.Close()
		}
	}()
	type publishResult struct {
		size      int64
		published bool
		err       error
	}
	done := make(chan publishResult, 1)
	go func() {
		size, published, err := publishGeneratedMediaAsset(context.Background(), pool, mediaDir, relPath, scratchPath,
			func(ctx context.Context, q *sqlcgen.Queries) (bool, error) {
				return activeThumbnailExists(ctx, q, recordingID)
			},
			func(ctx context.Context, q *sqlcgen.Queries, size int64) error {
				_, err := q.UpsertThumbnailMediaAsset(ctx, sqlcgen.UpsertThumbnailMediaAssetParams{
					RecordingID: recordingID, RelPath: relPath, SizeBytes: size,
				})
				return err
			})
		done <- publishResult{size: size, published: published, err: err}
	}()

	stageFound := false
	deadline := time.After(10 * time.Second)
poll:
	for {
		entries, readErr := os.ReadDir(filepath.Dir(canonicalPath))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".rokuban-media-asset-") {
				stageFound = true
				break poll
			}
		}
		select {
		case result := <-done:
			t.Fatalf("publisher returned while rel_path lock was held: %+v", result)
		case <-deadline:
			t.Fatal("publisher did not finish staging while waiting for rel_path lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !stageFound {
		t.Fatal("staged file was not observed")
	}
	gotCanonical, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCanonical, priorBytes) {
		t.Fatalf("canonical bytes changed before rel_path lock: got %q, want %q", gotCanonical, priorBytes)
	}
	if _, err := sqlcgen.New(pool).GetActiveThumbnailMediaAssetID(context.Background(), recordingID); !errors.Is(err, pgx5.ErrNoRows) {
		t.Fatalf("active thumbnail became visible before publication lock released: %v", err)
	}

	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	locked = false
	var result publishResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publisher did not finish after rel_path lock was released")
	}
	if result.err != nil {
		t.Fatalf("publishGeneratedMediaAsset: %v", result.err)
	}
	if !result.published || result.size != int64(len(wantBytes)) {
		t.Fatalf("publish result = %+v, want published size %d", result, len(wantBytes))
	}
	gotCanonical, err = os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCanonical, wantBytes) {
		t.Fatalf("canonical bytes = %q, want %q", gotCanonical, wantBytes)
	}
	var size int64
	if err := pool.QueryRow(context.Background(), `SELECT size_bytes FROM media_assets WHERE recording_id = $1 AND kind = 'thumbnail' AND state = 'active'`, recordingID).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size != int64(len(wantBytes)) {
		t.Errorf("committed size_bytes = %d, want %d", size, len(wantBytes))
	}
	entries, err := os.ReadDir(filepath.Dir(canonicalPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rokuban-media-asset-") {
			t.Errorf("staged file %q remains after publication", entry.Name())
		}
	}
}

func TestNewWorkerScratchDirIsUniqueForSameJobAndAttempt(t *testing.T) {
	root := t.TempDir()
	first, err := newWorkerScratchDir(root, "thumbnail", 42, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newWorkerScratchDir(root, "thumbnail", 42, 3)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("scratch directory reused: %q", first)
	}
	for _, path := range []string{first, second} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Errorf("scratch path %q is not a directory: info=%v err=%v", path, info, err)
		}
	}
}
