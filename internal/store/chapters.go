package store

import (
	"context"
	"errors"

	"github.com/fetburner/rokuban/internal/chapters"
)

var (
	// ErrNotFound means the recording does not exist or has been purged.
	ErrNotFound = errors.New("recording not found")
	// ErrTrashed means the recording is in the trash and cannot be edited.
	ErrTrashed = errors.New("recording is in the trash")
	// ErrStaleVersion means the supplied version no longer describes the chapter layer.
	ErrStaleVersion = errors.New("chapter layer has changed")
	// ErrDetectionPending means a required CM detection has not reached a terminal state.
	ErrDetectionPending = errors.New("CM detection is pending")
	// ErrConflict means persisted chapter data conflicts with a database constraint.
	ErrConflict = errors.New("chapter edit conflicts with persisted data")
)

// ChapterSnapshot contains the chapter layer selected for a recording and the
// state needed by the API to report whether CM detection is still pending.
// Version is opaque to callers and is only meaningful when passed to Replace.
type ChapterSnapshot struct {
	Owned              bool
	ProgramDurationMs  int64
	CMDetectionEnabled bool
	Detected           bool
	DetectionFailed    bool
	Version            string
	UserSpans          []chapters.Span
	CMRanges           []chapters.Range
}

// ChapterEdits persists the user-owned chapter layer for recordings.
//
// Each method completes its own transaction before returning. Replace and Reset
// lock the recording row before reading or writing chapter state, so they
// serialize with CM detection writes that use the same lock.
type ChapterEdits interface {
	// Read returns the active chapter layer and its opaque version.
	Read(ctx context.Context, recordingID int64) (ChapterSnapshot, error)
	// Replace atomically replaces the user layer if expectedVersion is current.
	// requireDetectionComplete is true when the deployment runs CM detection.
	Replace(ctx context.Context, recordingID int64, expectedVersion string, spans []chapters.Span, requireDetectionComplete bool) error
	// Reset removes the user layer and returns the recording to its automatic layer.
	Reset(ctx context.Context, recordingID int64) error
}
