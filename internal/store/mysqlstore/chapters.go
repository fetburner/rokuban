package mysqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// ChapterEdits はチャプターのユーザー層と自動層の読み書きを MySQL で実装する。
type ChapterEdits struct {
	db *sql.DB
}

var _ store.ChapterEdits = (*ChapterEdits)(nil)

// NewChapterEdits は MySQL を使う ChapterEdits を作る。
func NewChapterEdits(db *sql.DB) *ChapterEdits {
	return &ChapterEdits{db: db}
}

// chapterState は録画の行と所有・検出の状態を、同じ読み取りから得た値。
type chapterState struct {
	owned             bool
	cmDetect          bool
	detected          bool
	failed            bool
	programDurationMs int64
	detectedAtUs      int64
	adoptedAtUs       int64
}

func (st chapterState) snapshot() store.ChapterSnapshot {
	return store.ChapterSnapshot{
		Owned:              st.owned,
		ProgramDurationMs:  st.programDurationMs,
		CMDetectionEnabled: st.cmDetect,
		Detected:           st.detected,
		DetectionFailed:    st.failed,
	}
}

// Read は現在のチャプター層と、楽観的並行制御用の不透明な版を返す。
// 読み取り専用の REPEATABLE READ で、状態と区間を同じスナップショットから読む。
func (s *ChapterEdits) Read(ctx context.Context, recordingID int64) (store.ChapterSnapshot, error) {
	var snapshot store.ChapterSnapshot
	err := runSnapshotTx(ctx, s.db, func(tx *sql.Tx) error {
		st, err := readChapterState(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		snapshot = st.snapshot()
		version, spans, err := currentChapterVersion(ctx, tx, recordingID, st)
		if err != nil {
			return err
		}
		snapshot.Version = version
		if st.owned {
			snapshot.UserSpans = spans
			return nil
		}
		detection, err := readDetection(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		snapshot.CMRanges = detection.cmRanges
		return nil
	})
	if err != nil {
		return store.ChapterSnapshot{}, fmt.Errorf("reading chapters for recording %d: %w", recordingID, err)
	}
	return snapshot, nil
}

// ReadReview は最新の自動 CM 区間と、あれば同じ検出行のレビュー材料を読む。
// 所有の有無によらず自動層は最新の検出を返す。
func (s *ChapterEdits) ReadReview(ctx context.Context, recordingID int64) (store.ChapterReviewSnapshot, error) {
	var result store.ChapterReviewSnapshot
	err := runSnapshotTx(ctx, s.db, func(tx *sql.Tx) error {
		st, err := readChapterState(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		detection, err := readDetection(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		result = store.ChapterReviewSnapshot{Chapter: st.snapshot()}
		result.Chapter.CMRanges = detection.cmRanges
		if detection.materials {
			result.MaterialsAvailable = true
			result.LogoRanges = detection.logoRanges
			result.Elements = detection.elements
		}
		return nil
	})
	if err != nil {
		return store.ChapterReviewSnapshot{}, fmt.Errorf("reading chapter review for recording %d: %w", recordingID, err)
	}
	return result, nil
}

// Replace はユーザー層の全体を 1 つの tx で引き取る、または置き換える。
// 区間の重なりは、録画の行ロックの下で書いた後に SELECT で確かめる。主キーの (recording_id, start_ms)
// は開始位置が同じ重なりだけを拒む。重なり全体を拒む排他制約（Postgres）は MySQL に無い。
func (s *ChapterEdits) Replace(ctx context.Context, recordingID int64, expectedVersion string, spans []chapters.Span, requireDetectionComplete bool) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockChapterRecording(ctx, tx, recordingID); err != nil {
			return err
		}
		st, err := readChapterState(ctx, tx, recordingID)
		if err != nil {
			return err
		}
		version, _, err := currentChapterVersion(ctx, tx, recordingID, st)
		if err != nil {
			return err
		}
		if expectedVersion != version {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrStaleVersion)
		}
		if st.snapshot().DetectionPending(requireDetectionComplete) {
			return fmt.Errorf("recording %d: %w", recordingID, store.ErrDetectionPending)
		}

		if !st.owned {
			if _, err := tx.ExecContext(ctx, `INSERT INTO recording_chapter_ownership (recording_id) VALUES (?)`, recordingID); err != nil {
				return fmt.Errorf("adopting chapters for recording %d: %w", recordingID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recording_chapter_spans WHERE recording_id = ?`, recordingID); err != nil {
			return fmt.Errorf("clearing chapter spans for recording %d: %w", recordingID, err)
		}
		for _, span := range spans {
			var label any
			if span.Label != "" {
				label = span.Label
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO recording_chapter_spans (recording_id, start_ms, end_ms, label, cut)
VALUES (?, ?, ?, ?, ?)`, recordingID, span.StartMs, span.EndMs, label, span.Cut); err != nil {
				return fmt.Errorf("inserting chapter span [%d,%d) for recording %d: %w", span.StartMs, span.EndMs, recordingID, err)
			}
		}
		return checkSpansDisjoint(ctx, tx, recordingID)
	})
}

// Reset はユーザー層を消し、録画を自動層に戻す。spans は ON DELETE CASCADE で一緒に消える。
func (s *ChapterEdits) Reset(ctx context.Context, recordingID int64) error {
	return runTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockChapterRecording(ctx, tx, recordingID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recording_chapter_ownership WHERE recording_id = ?`, recordingID); err != nil {
			return fmt.Errorf("resetting chapters for recording %d: %w", recordingID, err)
		}
		return nil
	})
}

// lockChapterRecording は録画の行を排他で取り、消去済みなら store.ErrNotFound、
// ごみ箱なら store.ErrTrashed を返す。チャプターの状態を読む前に呼ぶ。
func lockChapterRecording(ctx context.Context, tx *sql.Tx, recordingID int64) error {
	var trashed, purged bool
	err := tx.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL, purged_at IS NOT NULL FROM recordings WHERE id = ? FOR UPDATE`, recordingID).Scan(&trashed, &purged)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && purged) {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("locking recording %d for chapters: %w", recordingID, err)
	}
	if trashed {
		return fmt.Errorf("recording %d: %w", recordingID, store.ErrTrashed)
	}
	return nil
}

// readChapterState は録画の行と所有・検出の状態を 1 つの文で読む。
// 消去済みと存在しない録画は store.ErrNotFound。
func readChapterState(ctx context.Context, tx *sql.Tx, recordingID int64) (chapterState, error) {
	var (
		st         chapterState
		detectedAt sql.NullTime
		adoptedAt  sql.NullTime
	)
	err := tx.QueryRowContext(ctx, `
SELECT COALESCE(p.cm_detect, FALSE),
       r.program_duration_ms,
       d.recording_id IS NOT NULL,
       d.detected_at,
       o.recording_id IS NOT NULL,
       o.adopted_at,
       EXISTS (SELECT 1 FROM current_cm_attempts ca WHERE ca.recording_id = r.id AND ca.state = 'failed')
FROM recordings r
LEFT JOIN recording_processing_policy p ON p.recording_id = r.id
LEFT JOIN recording_cm_detections d ON d.recording_id = r.id
LEFT JOIN recording_chapter_ownership o ON o.recording_id = r.id
WHERE r.id = ? AND r.purged_at IS NULL`, recordingID).Scan(
		&st.cmDetect, &st.programDurationMs, &st.detected, &detectedAt, &st.owned, &adoptedAt, &st.failed)
	if errors.Is(err, sql.ErrNoRows) {
		return chapterState{}, fmt.Errorf("recording %d: %w", recordingID, store.ErrNotFound)
	}
	if err != nil {
		return chapterState{}, fmt.Errorf("loading chapter state for recording %d: %w", recordingID, err)
	}
	// 版の材料はマイクロ秒の整数にして比べる。行が無ければ 0（Postgres の COALESCE と同じ）。
	if detectedAt.Valid {
		st.detectedAtUs = detectedAt.Time.UnixMicro()
	}
	if adoptedAt.Valid {
		st.adoptedAtUs = adoptedAt.Time.UnixMicro()
	}
	return st, nil
}

// currentChapterVersion は所有時にユーザー層の区間を読み、版を作る。
// 版は Go 側で決めた形の JSON を基にするので、同じ状態からは同じ値になる。
// 所有していないときは区間を返さない。
func currentChapterVersion(ctx context.Context, tx *sql.Tx, recordingID int64, st chapterState) (string, []chapters.Span, error) {
	if !st.owned {
		return chapterVersion(st, nil), nil, nil
	}
	spans, err := readUserSpans(ctx, tx, recordingID)
	if err != nil {
		return "", nil, err
	}
	return chapterVersion(st, spans), spans, nil
}

// chapterVersion は Read が返し、Replace が再確認する不透明な版を返す。
// 所有時の版は区間の JSON のハッシュを含むので、編集ごとに変わる。
func chapterVersion(st chapterState, spans []chapters.Span) string {
	switch {
	case st.owned:
		// hash.Hash の Write はエラーを返さない（契約）。区間は行ごとに書き、label は %q で囲んで、
		// 区切りの文字を含む label でも別の区間列と同じ入力にならないようにする。
		h := sha256.New()
		for _, s := range spans {
			_, _ = fmt.Fprintf(h, "%d %d %t %q\n", s.StartMs, s.EndMs, s.Cut, s.Label)
		}
		return fmt.Sprintf("user:%d:%s", st.adoptedAtUs, hex.EncodeToString(h.Sum(nil)[:8]))
	case st.detected:
		return fmt.Sprintf("auto:detected:%d", st.detectedAtUs)
	case st.failed:
		return "auto:failed"
	default:
		return "auto:none"
	}
}

// readUserSpans はユーザー層の区間を start_ms 順に読む。
// 区間は重ならないので、start_ms が同じ行は存在しない（0 長は CHECK で禁止）。end_ms は並びを決めるための副次の鍵。
func readUserSpans(ctx context.Context, tx *sql.Tx, recordingID int64) ([]chapters.Span, error) {
	return queryRows(ctx, tx, "chapter spans", `
SELECT start_ms, end_ms, label, cut
FROM recording_chapter_spans
WHERE recording_id = ?
ORDER BY start_ms, end_ms`, func(rows *sql.Rows) (chapters.Span, error) {
		var span chapters.Span
		var label sql.NullString
		err := rows.Scan(&span.StartMs, &span.EndMs, &label, &span.Cut)
		span.Label = label.String
		return span, err
	}, recordingID)
}

// detectionRow は録画の CM 検出行を Go の形に直したもの。行が無ければ空。
type detectionRow struct {
	cmRanges   []chapters.Range
	logoRanges []chapters.Range
	elements   []chapters.CMElement
	// materials は材料の両方があるときだけ真（logo と structure は両方あるか両方無いか）。
	materials bool
}

// readDetection は CM 検出行を読み、材料が揃っていれば decode する。
// cm_ranges の並びは保証されないので、読んだ後に startMs で並べ直す。
// structure_elements は保存された値をそのまま返す。
func readDetection(ctx context.Context, tx *sql.Tx, recordingID int64) (detectionRow, error) {
	var cmRaw, logoRaw, elemRaw []byte
	err := tx.QueryRowContext(ctx, `SELECT cm_ranges, logo_ranges, structure_elements FROM recording_cm_detections WHERE recording_id = ?`, recordingID).
		Scan(&cmRaw, &logoRaw, &elemRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return detectionRow{cmRanges: []chapters.Range{}}, nil
	}
	if err != nil {
		return detectionRow{}, fmt.Errorf("loading CM detection for recording %d: %w", recordingID, err)
	}
	row := detectionRow{materials: logoRaw != nil && elemRaw != nil}
	if err := json.Unmarshal(cmRaw, &row.cmRanges); err != nil {
		return detectionRow{}, fmt.Errorf("decoding CM ranges for recording %d: %w", recordingID, err)
	}
	sortRanges(row.cmRanges)
	if row.materials {
		if err := json.Unmarshal(logoRaw, &row.logoRanges); err != nil {
			return detectionRow{}, fmt.Errorf("decoding logo ranges for recording %d: %w", recordingID, err)
		}
		sortRanges(row.logoRanges)
		if err := json.Unmarshal(elemRaw, &row.elements); err != nil {
			return detectionRow{}, fmt.Errorf("decoding structure elements for recording %d: %w", recordingID, err)
		}
	}
	return row, nil
}

func sortRanges(ranges []chapters.Range) {
	sort.SliceStable(ranges, func(i, j int) bool {
		if ranges[i].StartMs != ranges[j].StartMs {
			return ranges[i].StartMs < ranges[j].StartMs
		}
		return ranges[i].EndMs < ranges[j].EndMs
	})
}

// checkSpansDisjoint は録画のユーザー層に重なる区間が無いことを確かめる。
// start_ms の順に並べた隣どうしを比べれば足りる。重なりがあるなら、start の小さい方の
// 区間は次の区間の開始より後に終わっているので、並びの隣の組にも重なりが出る。
// 重なれば store.ErrConflict を返し、呼び出し側の tx がロールバックする。
func checkSpansDisjoint(ctx context.Context, tx *sql.Tx, recordingID int64) error {
	var overlaps int
	err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM (
    SELECT start_ms, LAG(end_ms) OVER (ORDER BY start_ms, end_ms) AS prev_end_ms
    FROM recording_chapter_spans
    WHERE recording_id = ?
) ordered
WHERE prev_end_ms > start_ms`, recordingID).Scan(&overlaps)
	if err != nil {
		return fmt.Errorf("checking chapter span overlaps for recording %d: %w", recordingID, err)
	}
	if overlaps > 0 {
		return fmt.Errorf("recording %d: overlapping chapter spans: %w", recordingID, store.ErrConflict)
	}
	return nil
}
