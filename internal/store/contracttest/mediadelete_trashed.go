package contracttest

import (
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// runUntilEncodedTrashed は、ごみ箱の録画の原本を until_encoded の遷移で deleting にせず、store.ErrTrashed を返すことを確かめる。
// 消去済みの録画は（false, nil）になる。ごみ箱の録画は、呼び出し元がエラーとして扱えるように区別する。
func runUntilEncodedTrashed(t *testing.T, newStore NewMediaDeleteStore) {
	f := newStore(t)
	ctx := t.Context()
	recordingID, originalID := f.NewUntilEncodedOriginal(t)
	copyID := f.CopyID(t, originalID)
	f.TrashRecording(t, recordingID, time.Now(), false)
	marked, err := f.Store.MarkUntilEncodedDeleting(ctx, recordingID, originalID, copyID)
	if !errors.Is(err, store.ErrTrashed) {
		t.Fatalf("MarkUntilEncodedDeleting(trashed recording) error = %v, want store.ErrTrashed", err)
	}
	if marked {
		t.Fatal("MarkUntilEncodedDeleting(trashed recording) marked the original, want no change")
	}
	if got := f.CopyState(t, copyID); got != "active" {
		t.Fatalf("copy state = %q, want active", got)
	}
}
