package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// chapterDetectionPendingMessage は検出が終端に達していないときの 409 本文。
const chapterDetectionPendingMessage = "CM detection has not finished for this recording yet; retry after it reaches a terminal state"

// chapterStaleVersionMessage は下書きの基になった層が変わっているときの 409 本文。
// 検出中の 409 と区別できるよう文言を分ける。
const chapterStaleVersionMessage = "the chapter layer changed since it was fetched (re-detected, adopted, or reset); reload and edit again"

// chapterVersion は GET が返し PUT が突き合わせる版を導出する（列は持たない）。
//
// 所有済みなら所有の開始時刻とユーザー層の内容（呼び出し側が実際に読んだ spans の JSON）、所有前なら検出の終端（結果 / 失敗 / まだ）と検出時刻。
// 再検出・引き取り・自動に戻す のどれでも値が変わる。
func chapterVersion(s sqlcgen.GetRecordingChapterStateRow, userSpans json.RawMessage) string {
	switch {
	case s.Owned:
		// 所有済みは内容から導出する。adopted_at だけだと 2 回目以降の PUT で版が
		// 変わらず、2 タブの後勝ちで前の編集が黙って消える。
		sum := sha256.Sum256(userSpans)
		return fmt.Sprintf("user:%d:%s", s.AdoptedAtUs, hex.EncodeToString(sum[:8]))
	case s.Detected:
		return fmt.Sprintf("auto:detected:%d", s.DetectedAtUs)
	case s.Failed:
		return "auto:failed"
	default:
		return "auto:none"
	}
}

// chapterDetectionPending は所有前で検出が終端に達していないかを返す。
// **デプロイ側で CM 検出が無効なら偽** --- その構成では検出ジョブが積まれず、
// 終端に永久に達しないので、編集を封じると機能ごと使えなくなる。
func (h *Server) chapterDetectionPending(s sqlcgen.GetRecordingChapterStateRow) bool {
	return !s.Owned && h.capabilities.CmDetect && s.CmDetect && !s.Detected && !s.Failed
}

// GetRecordingChapters は録画の有効なチャプターを返す。
//
// 返すのは CM とユーザーが置いた区間だけで、隙間の本編は返さない。DB が持つ長さは
// EPG 上の番組長だけで、ファイルの実際の長さを api は知らない（原本は録画後に
// 削除されうるし、EIT 追従で延長もされる）。隙間を本編として扱うのはクライアントで、
// 再生中の `<video>.duration` で閉じる。
//
// 所有（= ユーザーが確認済み）ならユーザー層、していなければ自動層を読む。
// 自動層は chapters.AutoSpans を通すので、CM 率が 50% を超える壊れた検出は
// 「CM 無し」として返る。
func (h *Server) GetRecordingChapters(ctx context.Context, req GetRecordingChaptersRequestObject) (GetRecordingChaptersResponseObject, error) {
	q := sqlcgen.New(h.pool)
	state, err := q.GetRecordingChapterState(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetRecordingChapters404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("loading chapter state for recording %d: %w", req.Id, err)
	}

	if state.Owned {
		raw, err := q.GetRecordingChapterSpansJSON(ctx, req.Id)
		if err != nil {
			return nil, fmt.Errorf("loading chapter spans for recording %d: %w", req.Id, err)
		}
		spans, err := decodeChapterSpans(raw)
		if err != nil {
			return nil, fmt.Errorf("decoding chapter spans for recording %d: %w", req.Id, err)
		}
		// 保存時に量子化済みだが、GET の契約（境界はフレーム境界）は DB の状態に
		// 依存させない。直接 INSERT された行でも同じ形で返す。
		return GetRecordingChapters200JSONResponse{
			Source:           User,
			Version:          chapterVersion(state, raw),
			DetectionPending: false,
			Spans:            chapterSpansToAPI(chapters.Quantized(spans)),
		}, nil
	}

	raw, err := q.GetRecordingCMRangesJSON(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("loading CM ranges for recording %d: %w", req.Id, err)
	}
	ranges, err := decodeCMRanges(raw)
	if err != nil {
		return nil, fmt.Errorf("decoding CM ranges for recording %d: %w", req.Id, err)
	}
	return GetRecordingChapters200JSONResponse{
		Source:           Auto,
		Version:          chapterVersion(state, nil),
		DetectionPending: h.chapterDetectionPending(state),
		Spans:            chapterSpansToAPI(chapters.AutoSpans(ranges, state.ProgramDurationMs)),
	}, nil
}

// PutRecordingChapterEdits はユーザー層をタイムライン全体で置き換える。
//
// 最初の PUT が所有の行を作り、以後この録画では自動層を読まない。引き取りで
// 自動層を別途複製する手順は要らない --- 送られてくるタイムラインは「その時点で
// 有効なタイムライン」そのもので（クライアントは GET の結果を基に境界を直す）、
// 全置換なので複製した直後に同じ内容で上書きされる。効くのは所有の行だけで、
// それがある限り再検出がユーザーの修正を上書きしない。
func (h *Server) PutRecordingChapterEdits(ctx context.Context, req PutRecordingChapterEditsRequestObject) (PutRecordingChapterEditsResponseObject, error) {
	if req.Body == nil {
		return PutRecordingChapterEdits400JSONResponse{Error: "request body is required"}, nil
	}
	spans, err := chapters.Validate(chapterSpansFromAPI(req.Body.Spans))
	if err != nil {
		return PutRecordingChapterEdits400JSONResponse{Error: err.Error()}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction to edit chapters of recording %d: %w", req.Id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	// **行ロックを先頭で取ってから条件を評価する。** READ COMMITTED では条件が文の
	// 開始時点のスナップショットで評価されるので、ロック無しで「検出が終端に達して
	// いるか」を見ると、その後に commit された検出結果が見えないまま空の自動層を
	// 引き取ってしまう。検出結果を書く tx（internal/worker/cm_detect.go）も同じ行を
	// ロックするので、両者は直列化される。docs/storage/retention.md §7「復元と即時
	// 削除要求の競合」と同じ形。
	lock, err := q.LockRecording(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PutRecordingChapterEdits404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("locking recording %d: %w", req.Id, err)
	}
	if lock.IsPurged {
		return PutRecordingChapterEdits404JSONResponse{Error: "recording not found"}, nil
	}
	if lock.IsTrashed {
		return PutRecordingChapterEdits404JSONResponse{Error: "recording is in the trash"}, nil
	}

	state, err := q.GetRecordingChapterState(ctx, req.Id)
	if err != nil {
		return nil, fmt.Errorf("loading chapter state for recording %d: %w", req.Id, err)
	}
	// 下書きの基になった層の版。クライアントは GET の結果から下書きを作るので、
	// GET と PUT の間に層が変わっていれば（検出 commit・再検出・引き取り・自動に
	// 戻す）、行ロックでは防げない。所有済みでも比べる。
	var userSpansRaw json.RawMessage
	if state.Owned {
		userSpansRaw, err = q.GetRecordingChapterSpansJSON(ctx, req.Id)
		if err != nil {
			return nil, fmt.Errorf("loading chapter spans for recording %d: %w", req.Id, err)
		}
	}
	if req.Body.Version != chapterVersion(state, userSpansRaw) {
		return PutRecordingChapterEdits409JSONResponse{Error: chapterStaleVersionMessage}, nil
	}
	// 検出中（cm_detect が真で、結果行も終端の失敗行も無い）は引き取らせない。
	// 所有済みなら自動層を読まないので検出状態は無関係（再検出中でも編集できる）。
	if h.chapterDetectionPending(state) {
		return PutRecordingChapterEdits409JSONResponse{Error: chapterDetectionPendingMessage}, nil
	}

	if err := q.UpsertRecordingChapterOwnership(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("adopting chapters for recording %d: %w", req.Id, err)
	}
	if err := q.DeleteRecordingChapterSpans(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("clearing chapter spans for recording %d: %w", req.Id, err)
	}
	for _, s := range spans {
		params := sqlcgen.InsertRecordingChapterSpanParams{
			RecordingID: req.Id,
			StartMs:     s.StartMs,
			EndMs:       s.EndMs,
			Cut:         s.Cut,
		}
		if s.Label != "" {
			label := s.Label
			params.Label = &label
		}
		if err := q.InsertRecordingChapterSpan(ctx, params); err != nil {
			return nil, fmt.Errorf("inserting chapter span [%d,%d) for recording %d: %w", s.StartMs, s.EndMs, req.Id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing chapter edits for recording %d: %w", req.Id, err)
	}
	return PutRecordingChapterEdits204Response{}, nil
}

// DeleteRecordingChapterEdits はユーザー層を捨てて自動層へ戻す。
//
// 所有の行を消すだけで、区間は ON DELETE CASCADE で落ちる。冪等（所有していなくても
// 204）。取り込み直しの操作は作らない --- やり直しは「自動に戻す」だけである。
func (h *Server) DeleteRecordingChapterEdits(ctx context.Context, req DeleteRecordingChapterEditsRequestObject) (DeleteRecordingChapterEditsResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction to reset chapters of recording %d: %w", req.Id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	lock, err := q.LockRecording(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeleteRecordingChapterEdits404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("locking recording %d: %w", req.Id, err)
	}
	if lock.IsPurged {
		return DeleteRecordingChapterEdits404JSONResponse{Error: "recording not found"}, nil
	}
	if lock.IsTrashed {
		return DeleteRecordingChapterEdits404JSONResponse{Error: "recording is in the trash"}, nil
	}
	if err := q.DeleteRecordingChapterOwnership(ctx, req.Id); err != nil {
		return nil, fmt.Errorf("resetting chapters for recording %d: %w", req.Id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing chapter reset for recording %d: %w", req.Id, err)
	}
	return DeleteRecordingChapterEdits204Response{}, nil
}

// decodeChapterSpans はユーザー層の JSON（GetRecordingChapterSpansJSON）を読む。
// label の null は「ラベル無し」として空文字に落ちる。
func decodeChapterSpans(raw json.RawMessage) ([]chapters.Span, error) {
	var spans []chapters.Span
	if err := json.Unmarshal(raw, &spans); err != nil {
		return nil, err
	}
	return spans, nil
}

// decodeCMRanges は自動層の CM 区間の JSON（GetRecordingCMRangesJSON）を読む。
func decodeCMRanges(raw json.RawMessage) ([]chapters.Range, error) {
	var ranges []chapters.Range
	if err := json.Unmarshal(raw, &ranges); err != nil {
		return nil, err
	}
	return ranges, nil
}

// chapterSpansFromAPI は生成型のスパン列を純関数の型へ写す。
func chapterSpansFromAPI(spans []ChapterSpan) []chapters.Span {
	out := make([]chapters.Span, 0, len(spans))
	for _, s := range spans {
		span := chapters.Span{StartMs: s.StartMs, EndMs: s.EndMs, Cut: s.Cut}
		if s.Label != nil {
			span.Label = *s.Label
		}
		out = append(out, span)
	}
	return out
}

// chapterSpansToAPI は純関数のスパン列を生成型へ写す。ラベル無しは省略する。
func chapterSpansToAPI(spans []chapters.Span) []ChapterSpan {
	out := make([]ChapterSpan, 0, len(spans))
	for _, s := range spans {
		span := ChapterSpan{StartMs: s.StartMs, EndMs: s.EndMs, Cut: s.Cut}
		if s.Label != "" {
			label := s.Label
			span.Label = &label
		}
		out = append(out, span)
	}
	return out
}
