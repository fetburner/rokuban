package dbtest

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ChapterSpanParams は ChapterSpan の上書き対象。Label が nil なら label は NULL になり、Cut は true でなければならない（CHECK 制約）。
type ChapterSpanParams struct {
	StartMs int64
	EndMs   int64
	Label   *string
	Cut     bool
}

// AdoptChapters は録画 recordingID のチャプターを利用者の所有にする（recording_chapter_ownership の行を作る）。既に所有なら何もしない。
func AdoptChapters(t testing.TB, db sqlcgen.DBTX, recordingID int64) {
	t.Helper()
	if err := sqlcgen.New(db).UpsertRecordingChapterOwnership(t.Context(), recordingID); err != nil {
		t.Fatalf("adopting chapters for recording %d: %v", recordingID, err)
	}
}

// ChapterSpan は録画 recordingID に既定値のチャプター区間を 1 件作る。既定値は 600000..900000 ms の CM（cut = true）。opts で既定値を上書きする。区間は所有の行に外部キーで従うので、先に AdoptChapters を呼んでおく必要がある。
func ChapterSpan(t testing.TB, db sqlcgen.DBTX, recordingID int64, opts ...func(*ChapterSpanParams)) {
	t.Helper()
	label := "CM"
	params := ChapterSpanParams{
		StartMs: 600000,
		EndMs:   900000,
		Label:   &label,
		Cut:     true,
	}
	for _, opt := range opts {
		opt(&params)
	}
	if err := sqlcgen.New(db).InsertRecordingChapterSpan(t.Context(), sqlcgen.InsertRecordingChapterSpanParams{
		RecordingID: recordingID,
		StartMs:     params.StartMs,
		EndMs:       params.EndMs,
		Label:       params.Label,
		Cut:         params.Cut,
	}); err != nil {
		t.Fatalf("inserting chapter span %d..%d for recording %d: %v", params.StartMs, params.EndMs, recordingID, err)
	}
}
