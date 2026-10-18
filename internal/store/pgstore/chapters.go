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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
)

// Analysis は録画の解析のドメイン（チャプター編集など）を PostgreSQL で実装する。
// pool を持ち、このドメインの interface をまとめて満たす。
type Analysis struct {
	pool *pgxpool.Pool
}

// NewAnalysis は PostgreSQL を使う Analysis を作る。
func NewAnalysis(pool *pgxpool.Pool) *Analysis {
	return &Analysis{pool: pool}
}

// Read は現在のチャプター層と、楽観的並行制御用の不透明な版を返す。
// 読み取り専用の RepeatableRead tx で、状態と区間を同じスナップショットから読む。
func (s *Analysis) Read(ctx context.Context, recordingID int64) (store.ChapterSnapshot, error) {
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
func (s *Analysis) Replace(ctx context.Context, recordingID int64, expectedVersion string, spans []chapters.Span, requireDetectionComplete bool) error {
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
func (s *Analysis) Reset(ctx context.Context, recordingID int64) error {
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
