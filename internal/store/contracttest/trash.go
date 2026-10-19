package contracttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// RecordingState はごみ箱・完全削除の検査に必要な永続状態を表す。
type RecordingState struct {
	Deleted        bool
	Purged         bool
	PurgeRequested bool
}

// TrashFixture はごみ箱契約、完全削除契約と検査用 DB 操作をまとめる。
type TrashFixture struct {
	Trash        store.RecordingTrash
	Purge        store.RecordingPurge
	NewRecording NewRecording
	State        func(t *testing.T, recordingID int64) RecordingState
	SetDeletedAt func(t *testing.T, recordingID int64, deletedAt time.Time)
	// BeginPurgeBlocker は recordings 行をロックする tx を開き、要求行を挿入する操作と commit を返す。
	BeginPurgeBlocker func(t *testing.T, recordingID int64) (insertRequest func(), commit func())
	// BeginRestoreBlocker は復元後の recordings 状態を未 commit で作り、commit する操作を返す。
	BeginRestoreBlocker func(t *testing.T, recordingID int64) (commit func())
	WaitForLockWaiter   func(t *testing.T, operation string)
}

// NewTrashStore はごみ箱契約の実装とフィクスチャを作る。
type NewTrashStore func(t *testing.T) TrashFixture

// RunTrash は論理削除・復元・即時削除要求・完了記録の契約を実装に対して実行する。
func RunTrash(t *testing.T, newStore NewTrashStore) {
	t.Helper()

	t.Run("soft delete is idempotent", func(t *testing.T) { runSoftDeleteContract(t, newStore) })
	t.Run("purge request also trashes recording and is idempotent", func(t *testing.T) { runPurgeRequestContract(t, newStore) })
	t.Run("restore withdraws purge request", func(t *testing.T) { runRestoreContract(t, newStore) })
	t.Run("restore rejects conflicts and purged tombstones", func(t *testing.T) { runRestoreConflictContract(t, newStore) })
	t.Run("restore removes a concurrently committed purge request", func(t *testing.T) { runRestoreConcurrentPurgeContract(t, newStore) })
	t.Run("purge request serializes behind restore", func(t *testing.T) { runPurgeAfterRestoreContract(t, newStore) })
	t.Run("mark purged is idempotent", func(t *testing.T) { runMarkPurgedContract(t, newStore) })
}

func runSoftDeleteContract(t *testing.T, newStore NewTrashStore) {
	t.Helper()
	f := newStore(t)
	id := f.NewRecording(t, RecordingOptions{})
	ctx := context.Background()
	for range 2 {
		if err := f.Trash.SoftDelete(ctx, id); err != nil {
			t.Fatalf("SoftDelete() error = %v", err)
		}
	}
	if state := f.State(t, id); !state.Deleted {
		t.Fatalf("state after SoftDelete() = %+v, want deleted", state)
	}
	if err := f.Trash.SoftDelete(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SoftDelete(missing) error = %v, want ErrNotFound", err)
	}
}

func runPurgeRequestContract(t *testing.T, newStore NewTrashStore) {
	t.Helper()
	f := newStore(t)
	id := f.NewRecording(t, RecordingOptions{})
	ctx := context.Background()
	for range 2 {
		if err := f.Trash.RequestPurge(ctx, id); err != nil {
			t.Fatalf("RequestPurge() error = %v", err)
		}
	}
	if state := f.State(t, id); !state.Deleted || !state.PurgeRequested {
		t.Fatalf("state after RequestPurge() = %+v, want deleted and requested", state)
	}
	if err := f.Trash.RequestPurge(ctx, -1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RequestPurge(missing) error = %v, want ErrNotFound", err)
	}
}

func runRestoreContract(t *testing.T, newStore NewTrashStore) {
	t.Helper()
	f := newStore(t)
	id := f.NewRecording(t, RecordingOptions{Trashed: true})
	if err := f.Trash.RequestPurge(context.Background(), id); err != nil {
		t.Fatalf("seeding purge request: %v", err)
	}
	if err := f.Trash.Restore(context.Background(), id); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if state := f.State(t, id); state.Deleted || state.PurgeRequested {
		t.Fatalf("state after Restore() = %+v, want active and no purge request", state)
	}
	if err := f.Trash.Restore(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Restore(not in trash) error = %v, want ErrNotFound", err)
	}
}

func runRestoreConflictContract(t *testing.T, newStore NewTrashStore) {
	t.Helper()
	f := newStore(t)
	start := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
	trashedID := f.NewRecording(t, RecordingOptions{Trashed: true, EventID: 41, ProgramStartAt: start})
	_ = f.NewRecording(t, RecordingOptions{EventID: 41, ProgramStartAt: start})
	if err := f.Trash.Restore(context.Background(), trashedID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Restore(conflict) error = %v, want ErrConflict", err)
	}
	if state := f.State(t, trashedID); !state.Deleted {
		t.Fatalf("state after failed Restore() = %+v, want still deleted", state)
	}
	purgedID := f.NewRecording(t, RecordingOptions{Purged: true})
	if err := f.Trash.Restore(context.Background(), purgedID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Restore(purged) error = %v, want ErrNotFound", err)
	}
}

func runRestoreConcurrentPurgeContract(t *testing.T, newStore NewTrashStore) {
	t.Helper()
	f := newStore(t)
	id := f.NewRecording(t, RecordingOptions{Trashed: true})
	insertRequest, commit := f.BeginPurgeBlocker(t, id)
	defer commit()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Trash.Restore(ctx, id) }()
	f.WaitForLockWaiter(t, "restore")
	insertRequest()
	commit()
	if err := awaitTrashOperation(t, done, "Restore"); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if state := f.State(t, id); state.Deleted || state.PurgeRequested {
		t.Fatalf("state after concurrent restore = %+v, want active with no purge request", state)
	}
}

func runPurgeAfterRestoreContract(t *testing.T, newStore NewTrashStore) {
	t.Helper()
	f := newStore(t)
	id := f.NewRecording(t, RecordingOptions{Trashed: true})
	commitRestore := f.BeginRestoreBlocker(t, id)
	defer commitRestore()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Trash.RequestPurge(ctx, id) }()
	f.WaitForLockWaiter(t, "purge")
	if state := f.State(t, id); state.PurgeRequested {
		t.Fatal("purge request appeared while restore held the recording row")
	}
	commitRestore()
	if err := awaitTrashOperation(t, done, "RequestPurge"); err != nil {
		t.Fatalf("RequestPurge() error = %v", err)
	}
	if state := f.State(t, id); !state.Deleted || !state.PurgeRequested {
		t.Fatalf("state after serialized purge request = %+v, want deleted and requested", state)
	}
}

func runMarkPurgedContract(t *testing.T, newStore NewTrashStore) {
	t.Helper()
	f := newStore(t)
	id := f.NewRecording(t, RecordingOptions{Trashed: true})
	cutoff := time.Now().Add(-time.Hour)
	f.SetDeletedAt(t, id, cutoff.Add(-time.Hour))
	first, err := f.Purge.MarkPurged(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("first MarkPurged() error = %v", err)
	}
	if len(first) != 1 || first[0].ID != id {
		t.Fatalf("first MarkPurged() = %+v, want one row for %d", first, id)
	}
	second, err := f.Purge.MarkPurged(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("second MarkPurged() error = %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second MarkPurged() = %+v, want no newly purged rows", second)
	}
	if state := f.State(t, id); !state.Purged {
		t.Fatalf("state after MarkPurged() = %+v, want purged", state)
	}
}

func awaitTrashOperation(t *testing.T, done <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return within 10 seconds", name)
		return nil
	}
}
