// Package pgstore implements store contracts with PostgreSQL.
package pgstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// Store implements the application persistence contracts with PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// New creates a PostgreSQL-backed store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Read returns the current chapter layer and an opaque optimistic-concurrency version.
func (s *Store) Read(ctx context.Context, recordingID int64) (store.ChapterSnapshot, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.ChapterSnapshot{}, fmt.Errorf("beginning chapter read for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	state, err := q.GetRecordingChapterState(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ChapterSnapshot{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return store.ChapterSnapshot{}, fmt.Errorf("loading chapter state for recording %d: %w", recordingID, err)
	}

	snapshot := store.ChapterSnapshot{
		Owned:              state.Owned,
		ProgramDurationMs:  state.ProgramDurationMs,
		CMDetectionEnabled: state.CmDetect,
		Detected:           state.Detected,
		DetectionFailed:    state.Failed,
	}
	if state.Owned {
		raw, err := q.GetRecordingChapterSpansJSON(ctx, recordingID)
		if err != nil {
			return store.ChapterSnapshot{}, fmt.Errorf("loading chapter spans for recording %d: %w", recordingID, err)
		}
		if err := json.Unmarshal(raw, &snapshot.UserSpans); err != nil {
			return store.ChapterSnapshot{}, fmt.Errorf("decoding chapter spans for recording %d: %w", recordingID, err)
		}
		snapshot.Version = chapterVersion(state, raw)
	} else {
		raw, err := q.GetRecordingCMRangesJSON(ctx, recordingID)
		if err != nil {
			return store.ChapterSnapshot{}, fmt.Errorf("loading CM ranges for recording %d: %w", recordingID, err)
		}
		if err := json.Unmarshal(raw, &snapshot.CMRanges); err != nil {
			return store.ChapterSnapshot{}, fmt.Errorf("decoding CM ranges for recording %d: %w", recordingID, err)
		}
		snapshot.Version = chapterVersion(state, nil)
	}

	if err := tx.Commit(ctx); err != nil {
		return store.ChapterSnapshot{}, fmt.Errorf("committing chapter read for recording %d: %w", recordingID, err)
	}
	return snapshot, nil
}

// Replace adopts or replaces the complete user chapter layer in one transaction.
func (s *Store) Replace(ctx context.Context, recordingID int64, expectedVersion string, spans []chapters.Span, requireDetectionComplete bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning chapter edit for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if err := lockRecording(ctx, q, recordingID); err != nil {
		return err
	}
	state, err := q.GetRecordingChapterState(ctx, recordingID)
	if err != nil {
		return fmt.Errorf("loading chapter state for recording %d: %w", recordingID, err)
	}
	var userSpans json.RawMessage
	if state.Owned {
		userSpans, err = q.GetRecordingChapterSpansJSON(ctx, recordingID)
		if err != nil {
			return fmt.Errorf("loading chapter spans for recording %d: %w", recordingID, err)
		}
	}
	if expectedVersion != chapterVersion(state, userSpans) {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrStaleVersion)
	}
	if requireDetectionComplete && chapterDetectionPending(state) {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrDetectionPending)
	}

	if err := q.UpsertRecordingChapterOwnership(ctx, recordingID); err != nil {
		return fmt.Errorf("adopting chapters for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	if err := q.DeleteRecordingChapterSpans(ctx, recordingID); err != nil {
		return fmt.Errorf("clearing chapter spans for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	for _, span := range spans {
		params := sqlcgen.InsertRecordingChapterSpanParams{
			RecordingID: recordingID,
			StartMs:     span.StartMs,
			EndMs:       span.EndMs,
			Cut:         span.Cut,
		}
		if span.Label != "" {
			label := span.Label
			params.Label = &label
		}
		if err := q.InsertRecordingChapterSpan(ctx, params); err != nil {
			return fmt.Errorf("inserting chapter span [%d,%d) for recording %d: %w", span.StartMs, span.EndMs, recordingID, mapDatabaseError(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing chapter edits for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return nil
}

// Reset removes the user layer and returns the recording to its automatic layer.
func (s *Store) Reset(ctx context.Context, recordingID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning chapter reset for recording %d: %w", recordingID, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if err := lockRecording(ctx, q, recordingID); err != nil {
		return err
	}
	if err := q.DeleteRecordingChapterOwnership(ctx, recordingID); err != nil {
		return fmt.Errorf("resetting chapters for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing chapter reset for recording %d: %w", recordingID, mapDatabaseError(err))
	}
	return nil
}

func lockRecording(ctx context.Context, q *sqlcgen.Queries, recordingID int64) error {
	// This must be the first query in each write transaction, matching the CM
	// detection worker's lock on recordings so both writers observe each other.
	lock, err := q.LockRecording(ctx, recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
		}
		return fmt.Errorf("locking recording %d: %w", recordingID, err)
	}
	if lock.IsPurged {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if lock.IsTrashed {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrTrashed)
	}
	return nil
}

func chapterDetectionPending(state sqlcgen.GetRecordingChapterStateRow) bool {
	return !state.Owned && state.CmDetect && !state.Detected && !state.Failed
}

// chapterVersion returns the opaque token used by Read and rechecked by Replace.
// Owned tokens include the stored spans so each user edit changes the token;
// TestPutRecordingChapterEdits_OwnedVersionChangesWithEachEdit covers that behavior.
func chapterVersion(state sqlcgen.GetRecordingChapterStateRow, userSpans json.RawMessage) string {
	switch {
	case state.Owned:
		sum := sha256.Sum256(userSpans)
		return fmt.Sprintf("user:%d:%s", state.AdoptedAtUs, hex.EncodeToString(sum[:8]))
	case state.Detected:
		return fmt.Sprintf("auto:detected:%d", state.DetectedAtUs)
	case state.Failed:
		return "auto:failed"
	default:
		return "auto:none"
	}
}

func mapDatabaseError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23P01") {
		return errors.Join(store.ErrConflict, err)
	}
	return err
}

func rollback(tx pgx.Tx) {
	_ = tx.Rollback(context.Background())
}
