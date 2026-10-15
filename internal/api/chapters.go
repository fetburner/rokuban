package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/store"
)

// chapterDetectionPendingMessage は検出が終端に達していないときの 409 本文。
const chapterDetectionPendingMessage = "CM detection has not finished for this recording yet; retry after it reaches a terminal state"

// chapterStaleVersionMessage は下書きの基になった層が変わっているときの 409 本文。
// 検出中の 409 と区別できるよう文言を分ける。
const chapterStaleVersionMessage = "the chapter layer changed since it was fetched (re-detected, adopted, or reset); reload and edit again"

// chapterDetectionPending は所有前で検出が終端に達していないかを返す。
// **デプロイ側で CM 検出が無効なら偽** --- その構成では検出ジョブが積まれず、
// 終端に永久に達しないので、編集を封じると機能ごと使えなくなる。
func (h *Server) chapterDetectionPending(s store.ChapterSnapshot) bool {
	return !s.Owned && h.capabilities.CmDetect && s.CMDetectionEnabled && !s.Detected && !s.DetectionFailed
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
	if h.chapterStore == nil {
		return nil, fmt.Errorf("chapter store is not configured")
	}
	state, err := h.chapterStore.Read(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return GetRecordingChapters404JSONResponse{Error: "recording not found"}, nil
		}
		return nil, fmt.Errorf("loading chapter state for recording %d: %w", req.Id, err)
	}

	if state.Owned {
		// 保存時に量子化済みだが、GET の契約（境界はフレーム境界）は DB の状態に
		// 依存させない。直接 INSERT された行でも同じ形で返す。
		return GetRecordingChapters200JSONResponse{
			Source:           User,
			Version:          state.Version,
			DetectionPending: false,
			Spans:            chapterSpansToAPI(chapters.Quantized(state.UserSpans)),
		}, nil
	}

	return GetRecordingChapters200JSONResponse{
		Source:           Auto,
		Version:          state.Version,
		DetectionPending: h.chapterDetectionPending(state),
		Spans:            chapterSpansToAPI(chapters.AutoSpans(state.CMRanges, state.ProgramDurationMs)),
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
		return PutRecordingChapterEdits400JSONResponse{Error: err.Error()}, nil //nolint:nilerr // Validation failures map to an HTTP 400 response.
	}
	if h.chapterStore == nil {
		return nil, fmt.Errorf("chapter store is not configured")
	}
	if err := h.chapterStore.Replace(ctx, req.Id, req.Body.Version, spans, h.capabilities.CmDetect); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			return PutRecordingChapterEdits404JSONResponse{Error: "recording not found"}, nil
		case errors.Is(err, store.ErrTrashed):
			return PutRecordingChapterEdits404JSONResponse{Error: "recording is in the trash"}, nil
		case errors.Is(err, store.ErrStaleVersion):
			return PutRecordingChapterEdits409JSONResponse{Error: chapterStaleVersionMessage}, nil
		case errors.Is(err, store.ErrDetectionPending):
			return PutRecordingChapterEdits409JSONResponse{Error: chapterDetectionPendingMessage}, nil
		case errors.Is(err, store.ErrConflict):
			return PutRecordingChapterEdits409JSONResponse{Error: "chapter edits conflict with stored ranges; reload and edit again"}, nil
		default:
			return nil, fmt.Errorf("replacing chapter edits for recording %d: %w", req.Id, err)
		}
	}
	return PutRecordingChapterEdits204Response{}, nil
}

// DeleteRecordingChapterEdits はユーザー層を捨てて自動層へ戻す。
//
// 所有の行を消すだけで、区間は ON DELETE CASCADE で落ちる。冪等（所有していなくても
// 204）。取り込み直しの操作は作らない --- やり直しは「自動に戻す」だけである。
func (h *Server) DeleteRecordingChapterEdits(ctx context.Context, req DeleteRecordingChapterEditsRequestObject) (DeleteRecordingChapterEditsResponseObject, error) {
	if h.chapterStore == nil {
		return nil, fmt.Errorf("chapter store is not configured")
	}
	if err := h.chapterStore.Reset(ctx, req.Id); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			return DeleteRecordingChapterEdits404JSONResponse{Error: "recording not found"}, nil
		case errors.Is(err, store.ErrTrashed):
			return DeleteRecordingChapterEdits404JSONResponse{Error: "recording is in the trash"}, nil
		default:
			return nil, fmt.Errorf("resetting chapters for recording %d: %w", req.Id, err)
		}
	}
	return DeleteRecordingChapterEdits204Response{}, nil
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
