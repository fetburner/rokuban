package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func assertMediaAssetCopy(t *testing.T, pool *pgxpool.Pool, assetID int64, wantRelPath string) {
	t.Helper()
	var gotStore, gotRelPath, gotState string
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), `
SELECT copy.store, copy.rel_path, copy.state
FROM media_asset_copies copy
JOIN media_assets asset ON asset.id = copy.media_asset_id
WHERE copy.media_asset_id = $1 AND copy.store = $2
  AND copy.generation = asset.derived_generation
  AND copy.state = 'active'`, assetID, dbtest.StoreName,
	).Scan(&gotStore, &gotRelPath, &gotState), fmt.Sprintf("reading media asset copy %d in store %q", assetID, dbtest.StoreName))
	if gotRelPath != wantRelPath || gotState != "active" {
		t.Fatalf("media asset copy = %q/%q/%q, want %s/%q/active", gotStore, gotRelPath, gotState, dbtest.StoreName, wantRelPath)
	}
}

func assertMediaAssetSHA256(t *testing.T, pool *pgxpool.Pool, recordingID int64, kind, profile string, content []byte) {
	t.Helper()
	query := `SELECT sha256 FROM media_assets WHERE recording_id = $1 AND kind = $2 AND EXISTS (SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = media_assets.id AND c.state = 'active')`
	args := []any{recordingID, kind}
	if profile == "" {
		query += ` AND profile IS NULL`
	} else {
		query += ` AND profile = $3`
		args = append(args, profile)
	}
	var got []byte
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&got), fmt.Sprintf("reading active %s media asset SHA-256 for recording %d", kind, recordingID))
	want := sha256.Sum256(content)
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("active %s SHA-256 = %x, want %x", kind, got, want)
	}
}

func TestRemoveStaleEncodeScratchScopesByRecordingAndProfile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeDir := func(rel string) string {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		testutil.RequireNoError(t, os.MkdirAll(path, 0o700), "os.MkdirAll")
		return path
	}
	stale1 := makeDir(filepath.Join("encode", encodeScratchScope(7, "h264")+"-1-old"))
	stale2 := makeDir(filepath.Join("encode", encodeScratchScope(7, "h264")+"-2-old"))
	otherProfile := makeDir(filepath.Join("encode", encodeScratchScope(7, "cut")+"-1-other"))
	otherRecording := makeDir(filepath.Join("encode", encodeScratchScope(70, "h264")+"-1-other"))
	otherKind := makeDir("thumbnail/7-other")

	removeStaleEncodeScratch(root, 7, "h264", slog.Default())

	for _, path := range []string{stale1, stale2} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stale scratch %s stat error = %v, want os.ErrNotExist", path, err)
		}
	}
	for _, path := range []string{otherProfile, otherRecording, otherKind} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unrelated scratch %s was removed: %v", path, err)
		}
	}
}

// 生きた試行は同じ scope でも残し、死んだ兄弟は消す。生きた試行は attempt 2 で、
// os.ReadDir の名前順では -1-dead < -2-<random> < -3-dead になる。生きた試行に
// 当たっても走査を打ち切らず、後ろの -3-dead も消えることを見る。
func TestRemoveStaleEncodeScratchKeepsLiveAttemptOfSameScope(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	live, liveLock, err := newEncodeScratchDir(root, 7, "h264", 2)
	testutil.RequireNoError(t, err, "newEncodeScratchDir")
	t.Cleanup(func() { _ = closeEncodeScratchLock(liveLock) })
	var dead []string
	for _, name := range []string{"-1-dead", "-3-dead"} {
		d := filepath.Join(root, "encode", encodeScratchScope(7, "h264")+name)
		testutil.RequireNoError(t, os.MkdirAll(d, 0o700), "os.MkdirAll")
		dead = append(dead, d)
	}

	removeStaleEncodeScratch(root, 7, "h264", slog.Default())

	if _, err := os.Stat(live); err != nil {
		t.Errorf("live attempt of the same scope was removed: %v", err)
	}
	for _, d := range dead {
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("dead sibling %s stat error = %v, want os.ErrNotExist", filepath.Base(d), err)
		}
	}
	testutil.RequireNoError(t, closeEncodeScratchLock(liveLock), "closeEncodeScratchLock")
	removeStaleEncodeScratch(root, 7, "h264", slog.Default())
	if _, err := os.Stat(live); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("released attempt stat error = %v, want os.ErrNotExist", err)
	}
}

// 別の recording / profile の生きた encode があっても、対象 scope の死んだ残骸は消える。
func TestRemoveStaleEncodeScratchIgnoresLiveEncodeOfOtherScope(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	otherRecording, l1, err := newEncodeScratchDir(root, 70, "h264", 1)
	testutil.RequireNoError(t, err, "newEncodeScratchDir")
	t.Cleanup(func() { _ = closeEncodeScratchLock(l1) })
	otherProfile, l2, err := newEncodeScratchDir(root, 7, "cut", 1)
	testutil.RequireNoError(t, err, "newEncodeScratchDir")
	t.Cleanup(func() { _ = closeEncodeScratchLock(l2) })
	dead := filepath.Join(root, "encode", encodeScratchScope(7, "h264")+"-1-dead")
	testutil.RequireNoError(t, os.MkdirAll(dead, 0o700), "os.MkdirAll")

	removeStaleEncodeScratch(root, 7, "h264", slog.Default())

	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dead scratch stat error = %v, want os.ErrNotExist (blocked by another scope's live encode)", err)
	}
	for _, path := range []string{otherRecording, otherProfile} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("live scratch %s was removed: %v", path, err)
		}
	}
}

// 作成（MkdirTemp から dir lock 取得まで）の間は掃除が入れない。gate を外から
// 保持して、掃除が待つこと・解放後に進むことを見る。
func TestRemoveStaleEncodeScratchWaitsForScratchCreationGate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dead := filepath.Join(root, "encode", encodeScratchScope(7, "h264")+"-1-dead")
	testutil.RequireNoError(t, os.MkdirAll(dead, 0o700), "os.MkdirAll")
	gate, err := openEncodeScratchGate(root)
	testutil.RequireNoError(t, err, "openEncodeScratchGate")
	testutil.RequireNoError(t, flockScratch(gate, syscall.LOCK_EX), "flockScratch")
	done := make(chan struct{})
	go func() {
		removeStaleEncodeScratch(root, 7, "h264", slog.Default())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("cleanup ran while the creation gate was held")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(dead); err != nil {
		t.Errorf("dead scratch removed while gate held: %v", err)
	}
	testutil.RequireNoError(t, closeEncodeScratchLock(gate), "closeEncodeScratchLock")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not proceed after the gate was released")
	}
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dead scratch stat error = %v, want os.ErrNotExist", err)
	}
}

// 作成側も gate を取る。掃除側の待ちと合わせて、作成直後で lock 前の dir が消される窓が塞がる。
func TestNewEncodeScratchDirWaitsForScratchCreationGate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gate, err := openEncodeScratchGate(root)
	testutil.RequireNoError(t, err, "openEncodeScratchGate")
	testutil.RequireNoError(t, flockScratch(gate, syscall.LOCK_EX), "flockScratch")
	type result struct {
		lock *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		_, lock, err := newEncodeScratchDir(root, 7, "h264", 1)
		done <- result{lock, err}
	}()
	select {
	case <-done:
		t.Fatal("scratch creation ran while the gate was held")
	case <-time.After(200 * time.Millisecond):
	}
	testutil.RequireNoError(t, closeEncodeScratchLock(gate), "closeEncodeScratchLock")
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		_ = closeEncodeScratchLock(r.lock)
	case <-time.After(5 * time.Second):
		t.Fatal("scratch creation did not proceed after the gate was released")
	}
}

func TestNewEncodeScratchDirUsesRecordingProfileAndAttempt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir, lock, err := newEncodeScratchDir(root, 42, "h264", 3)
	testutil.RequireNoError(t, err, "newEncodeScratchDir")
	t.Cleanup(func() { _ = closeEncodeScratchLock(lock) })

	wantPrefix := filepath.Join(root, "encode", "42-72a6c9135f1b59693330c8ae39e10e2c1fcc87a6c40865ca66b59d24dadacf60-3-")
	if !strings.HasPrefix(dir, wantPrefix) {
		t.Errorf("encode scratch directory = %q, want prefix %q", dir, wantPrefix)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("encode scratch directory stat = (%v, %v), want directory", info, err)
	}
}

func TestNewEncodeScratchDirSupportsLongProfileNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	profile := strings.Repeat("profile", 128)
	dir, lock, err := newEncodeScratchDir(root, 42, profile, 3)
	testutil.RequireNoError(t, err, "newEncodeScratchDir with long profile name")
	t.Cleanup(func() { _ = closeEncodeScratchLock(lock) })
	if got, want := filepath.Base(dir), fmt.Sprintf("42-%x-3-", sha256.Sum256([]byte(profile))); !strings.HasPrefix(got, want) {
		t.Errorf("encode scratch directory = %q, want prefix %q", got, want)
	}
	if got := len(filepath.Base(dir)); got > 255 {
		t.Errorf("encode scratch basename is %d bytes, want at most 255", got)
	}
}
