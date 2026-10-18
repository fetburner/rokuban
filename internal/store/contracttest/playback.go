package contracttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// PlaybackFixture は視聴状態契約の実装と、保存状態を読む検査口をまとめる。
type PlaybackFixture struct {
	Store              store.PlaybackState
	NewRecording       NewRecording
	Position           func(t *testing.T, recordingID int64) (int64, bool)
	Watched            func(t *testing.T, recordingID int64) bool
	FailPositionDelete func(t *testing.T) (cleanup func())
}

// NewPlaybackStore は視聴状態契約の実装とフィクスチャを作る。
type NewPlaybackStore func(t *testing.T) PlaybackFixture

// RunPlayback は視聴状態の契約を実装に対して実行する。
func RunPlayback(t *testing.T, newStore NewPlaybackStore) {
	t.Helper()

	t.Run("position save and clear are idempotent", func(t *testing.T) {
		f := newStore(t)
		id := f.NewRecording(t, RecordingOptions{})
		ctx := context.Background()
		if err := f.Store.SavePosition(ctx, id, 12_000); err != nil {
			t.Fatalf("SavePosition() error = %v", err)
		}
		if got, ok := f.Position(t, id); !ok || got != 12_000 {
			t.Fatalf("position = %d, present = %t, want 12000 and present", got, ok)
		}
		if err := f.Store.ClearPosition(ctx, id); err != nil {
			t.Fatalf("ClearPosition() error = %v", err)
		}
		if _, ok := f.Position(t, id); ok {
			t.Fatal("position remains after ClearPosition()")
		}
		if err := f.Store.ClearPosition(ctx, id); err != nil {
			t.Fatalf("second ClearPosition() error = %v, want idempotent success", err)
		}
	})

	t.Run("mark watched clears position atomically", func(t *testing.T) {
		f := newStore(t)
		id := f.NewRecording(t, RecordingOptions{})
		ctx := context.Background()
		if err := f.Store.SavePosition(ctx, id, 12_000); err != nil {
			t.Fatalf("seeding position: %v", err)
		}
		if err := f.Store.MarkWatched(ctx, id); err != nil {
			t.Fatalf("MarkWatched() error = %v", err)
		}
		if !f.Watched(t, id) {
			t.Fatal("watched marker was not saved")
		}
		if _, ok := f.Position(t, id); ok {
			t.Fatal("position remains after MarkWatched()")
		}
	})

	t.Run("mark watched rolls back when clearing position fails", func(t *testing.T) {
		f := newStore(t)
		id := f.NewRecording(t, RecordingOptions{})
		ctx := context.Background()
		if err := f.Store.SavePosition(ctx, id, 12_000); err != nil {
			t.Fatalf("seeding position: %v", err)
		}
		if f.FailPositionDelete == nil {
			t.Fatal("test fixture does not support position delete failure injection")
		}
		cleanup := f.FailPositionDelete(t)
		defer cleanup()

		if err := f.Store.MarkWatched(ctx, id); err == nil {
			t.Fatal("MarkWatched() succeeded with failing position delete, want error")
		}
		if f.Watched(t, id) {
			t.Fatal("watched marker remained after position delete failure")
		}
		if position, ok := f.Position(t, id); !ok || position != 12_000 {
			t.Fatalf("position after rollback = %d, present = %t, want 12000 and present", position, ok)
		}
	})

	t.Run("clear watched removes every marker for the event", func(t *testing.T) {
		f := newStore(t)
		start := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
		one := f.NewRecording(t, RecordingOptions{ProgramStartAt: start, EventID: 31})
		two := f.NewRecording(t, RecordingOptions{ProgramStartAt: start, EventID: 32})
		ctx := context.Background()
		for _, id := range []int64{one, two} {
			if err := f.Store.MarkWatched(ctx, id); err != nil {
				t.Fatalf("MarkWatched(%d): %v", id, err)
			}
		}
		if err := f.Store.ClearWatchedForEvent(ctx, one); err != nil {
			t.Fatalf("ClearWatchedForEvent() error = %v", err)
		}
		for _, id := range []int64{one, two} {
			if f.Watched(t, id) {
				t.Errorf("watched marker remains on event sibling %d", id)
			}
		}
	})

	t.Run("missing and purged recordings are not found", func(t *testing.T) {
		f := newStore(t)
		purgedID := f.NewRecording(t, RecordingOptions{Purged: true})
		operations := []struct {
			name string
			run  func(int64) error
		}{
			{name: "SavePosition", run: func(id int64) error { return f.Store.SavePosition(context.Background(), id, 12_000) }},
			{name: "ClearPosition", run: func(id int64) error { return f.Store.ClearPosition(context.Background(), id) }},
			{name: "MarkWatched", run: func(id int64) error { return f.Store.MarkWatched(context.Background(), id) }},
			{name: "ClearWatchedForEvent", run: func(id int64) error { return f.Store.ClearWatchedForEvent(context.Background(), id) }},
		}
		for _, operation := range operations {
			for _, target := range []struct {
				name string
				id   int64
			}{{name: "missing", id: -1}, {name: "purged", id: purgedID}} {
				t.Run(operation.name+"/"+target.name, func(t *testing.T) {
					if err := operation.run(target.id); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("%s(%d) error = %v, want ErrNotFound", operation.name, target.id, err)
					}
				})
			}
		}
	})
}
