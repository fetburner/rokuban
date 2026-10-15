// Package pgstore は store の契約を PostgreSQL で実装する。
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

// Store は PostgreSQL によるアプリケーションの永続化契約の実装。
type Store struct {
	pool *pgxpool.Pool
}

// New は PostgreSQL を使う Store を作る。
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Read は現在のチャプター層と、楽観的並行制御用の不透明な版を返す。
// 読み取り専用の RepeatableRead tx で、状態と区間を同じスナップショットから読む。
func (s *Store) Read(ctx context.Context, recordingID int64) (store.ChapterSnapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
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

	snapshot := snapshotFromState(state)
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

// Replace はユーザー層の全体を 1 つの tx で引き取る、または置き換える。
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
	if snapshotFromState(state).DetectionPending(requireDetectionComplete) {
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

// Reset はユーザー層を消し、録画を自動層に戻す。
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

// lockRecording は録画の行をロックし、消去済み・ごみ箱の録画を弾く。
func lockRecording(ctx context.Context, q *sqlcgen.Queries, recordingID int64) error {
	// 書き込み tx の最初のクエリでなければならない。CM 検出 worker の recordings
	// ロックと同じ行を取り、両方の書き込みが互いを観測できるようにする。
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

// snapshotFromState は sqlcgen の行から ChapterSnapshot の状態部分を作る。
// Read と Replace が同じ判定を共有するための唯一の変換。
func snapshotFromState(state sqlcgen.GetRecordingChapterStateRow) store.ChapterSnapshot {
	return store.ChapterSnapshot{
		Owned:              state.Owned,
		ProgramDurationMs:  state.ProgramDurationMs,
		CMDetectionEnabled: state.CmDetect,
		Detected:           state.Detected,
		DetectionFailed:    state.Failed,
	}
}

// chapterVersion は Read が返し、Replace が再確認する不透明な版を返す。
// 所有時の版は保存済みの区間を含むので、ユーザーが編集するたびに版が変わる
// （TestPutRecordingChapterEdits_OwnedVersionChangesWithEachEdit が検査する）。
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

// mapDatabaseError は一意違反と排他制約違反を store.ErrConflict に写す。
func mapDatabaseError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23P01") {
		return errors.Join(store.ErrConflict, err)
	}
	return err
}

// rollback は tx を捨てる。commit 済みなら何もしない。
func rollback(tx pgx.Tx) {
	_ = tx.Rollback(context.Background())
}
