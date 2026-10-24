package contracttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// TrashFixture はごみ箱契約、完全削除契約と検査用 DB 操作をまとめる。
type TrashFixture struct {
	Trash        store.RecordingTrash
	Purge        store.RecordingPurge
	Serving      store.MediaServing
	Catalog      store.Catalog
	NewRecording NewRecording
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
	if state := recordingRemovalState(t, f, id); state.DeletedAt == nil {
		t.Fatalf("GetRecordingRemovalState after SoftDelete() = %s, want DeletedAt set", removalStateText(state))
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
	if state := recordingRemovalState(t, f, id); state.DeletedAt == nil {
		t.Fatalf("GetRecordingRemovalState after RequestPurge() = %s, want DeletedAt set", removalStateText(state))
	}
	if !recordingPurgeRequested(t, f, id) {
		t.Fatalf("Catalog.Export after RequestPurge() is missing recording %d's purge request", id)
	}
	if purged, err := f.Purge.MarkPurged(ctx, time.Now().Add(time.Hour)); err != nil || len(purged) != 1 || purged[0].ID != id {
		t.Fatalf("MarkPurged after RequestPurge() = %d rows, err = %v, want exactly recording %d", len(purged), err, id)
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
	if state := recordingRemovalState(t, f, id); state.DeletedAt != nil {
		t.Fatalf("GetRecordingRemovalState after Restore() = %s, want DeletedAt nil", removalStateText(state))
	}
	if recordingPurgeRequested(t, f, id) {
		t.Fatalf("Catalog.Export after Restore() still contains recording %d's purge request", id)
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
	if state := recordingRemovalState(t, f, trashedID); state.DeletedAt == nil {
		t.Fatalf("GetRecordingRemovalState after failed Restore() = %s, want DeletedAt set", removalStateText(state))
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
	if state := recordingRemovalState(t, f, id); state.DeletedAt != nil {
		t.Fatalf("GetRecordingRemovalState after concurrent restore = %s, want DeletedAt nil", removalStateText(state))
	}
	if recordingPurgeRequested(t, f, id) {
		t.Fatalf("Catalog.Export after concurrent restore still contains recording %d's purge request", id)
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
	if recordingPurgeRequested(t, f, id) {
		t.Fatal("purge request appeared while restore held the recording row")
	}
	commitRestore()
	if err := awaitTrashOperation(t, done, "RequestPurge"); err != nil {
		t.Fatalf("RequestPurge() error = %v", err)
	}
	if state := recordingRemovalState(t, f, id); state.DeletedAt == nil {
		t.Fatalf("GetRecordingRemovalState after serialized purge request = %s, want DeletedAt set", removalStateText(state))
	}
	if !recordingPurgeRequested(t, f, id) {
		t.Fatalf("Catalog.Export after serialized RequestPurge() is missing recording %d's purge request", id)
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
	if state := recordingRemovalState(t, f, id); state.PurgedAt == nil {
		t.Fatalf("GetRecordingRemovalState after MarkPurged() = %s, want PurgedAt set", removalStateText(state))
	}
}

func recordingRemovalState(t *testing.T, f TrashFixture, recordingID int64) store.RecordingRemovalState {
	t.Helper()
	state, err := f.Serving.GetRecordingRemovalState(context.Background(), recordingID)
	if err != nil {
		t.Fatalf("GetRecordingRemovalState(%d): %v", recordingID, err)
	}
	return state
}

func recordingPurgeRequested(t *testing.T, f TrashFixture, recordingID int64) bool {
	t.Helper()
	document, err := f.Catalog.Export(context.Background())
	if err != nil {
		t.Fatalf("Catalog.Export for purge request %d: %v", recordingID, err)
	}
	for _, request := range document.RecordingPurgeRequests {
		if request.RecordingID == recordingID {
			return true
		}
	}
	return false
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

// removalStateText は失敗メッセージ用に、状態の 3 つの時刻をフィールド名つきで表す。
func removalStateText(state store.RecordingRemovalState) string {
	text := func(at *time.Time) string {
		if at == nil {
			return "nil"
		}
		return at.UTC().Format(time.RFC3339Nano)
	}
	return "{DeletedAt:" + text(state.DeletedAt) + " PurgedAt:" + text(state.PurgedAt) + " SupersededAt:" + text(state.SupersededAt) + "}"
}
