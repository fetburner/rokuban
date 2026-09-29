package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestExportRescue_RoundTripsUserChapters は、ユーザーが手で置いたチャプター
// （所有の行 + 区間）が export → rescue の往復で戻ることを確かめる。自動検出で
// 作り直せない事実なので、落ちると取り返しがつかない。
//
// 併せて 2 回当てても増殖しないことも見る（区間の表は主キーを持たず、重なりを
// EXCLUDE が禁じているので、素の INSERT では 2 回目が同じ区間と衝突して 1 世代
// まるごと復元できなくなる）。
func TestExportRescue_RoundTripsUserChapters(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	mediaDir := t.TempDir()

	recID, err := q.CreateRecording(ctx, sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              "default",
		NetworkID:         32736,
		ServiceID:         1024,
		EventID:           400,
		ServiceName:       "NHK総合",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "チャプター付き",
		ProgramStartAt:    time.Now().UTC().Truncate(time.Second),
		ProgramDurationMs: (30 * time.Minute).Milliseconds(),
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("CreateRecording: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO recording_chapter_ownership (recording_id, adopted_at)
VALUES ($1, '2026-01-02 03:04:05+00')`, recID); err != nil {
		t.Fatalf("inserting ownership: %v", err)
	}
	cm, op := "CM", "OP"
	for _, span := range []struct {
		start, end int64
		label      *string
		cut        bool
	}{
		{1001, 2002, &cm, true},
		{3003, 4004, &op, false},
		{5005, 6006, nil, true},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO recording_chapter_spans (recording_id, span, label, cut)
VALUES ($1, int8range($2, $3), $4, $5)`, recID, span.start, span.end, span.label, span.cut); err != nil {
			t.Fatalf("inserting span: %v", err)
		}
	}

	doc, err := Export(ctx, pool)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(doc.RecordingChapterOwnerships) != 1 || len(doc.RecordingChapterSpans) != 3 {
		t.Fatalf("exported chapters = %d ownerships / %d spans, want 1 / 3",
			len(doc.RecordingChapterOwnerships), len(doc.RecordingChapterSpans))
	}
	if _, err := Write(mediaDir, doc, DefaultKeep); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// DB を失った状況を作る（この 2 表だけ落とす）。
	if _, err := pool.Exec(ctx, `DELETE FROM recording_chapter_ownership`); err != nil {
		t.Fatalf("simulating DB loss: %v", err)
	}

	result, err := RescueLatest(ctx, pool, mediaDir, []string{"default"})
	if err != nil {
		t.Fatalf("RescueLatest: %v", err)
	}
	if result.RecordingChapterOwnerships != 1 || result.RecordingChapterSpans != 3 {
		t.Fatalf("rescued chapters = %d ownerships / %d spans, want 1 / 3",
			result.RecordingChapterOwnerships, result.RecordingChapterSpans)
	}

	var adoptedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT adopted_at FROM recording_chapter_ownership WHERE recording_id = $1`, recID).Scan(&adoptedAt); err != nil {
		t.Fatalf("query ownership: %v", err)
	}
	if !adoptedAt.UTC().Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("adopted_at = %s, want 2026-01-02T03:04:05Z", adoptedAt.UTC())
	}

	// label の NULL（ラベル無しの cut 区間）が往復で保たれること。
	rows, err := pool.Query(ctx, `
SELECT lower(span), upper(span), label, cut
FROM recording_chapter_spans
WHERE recording_id = $1
ORDER BY lower(span)`, recID)
	if err != nil {
		t.Fatalf("query spans: %v", err)
	}
	defer rows.Close()
	want := []struct {
		start, end int64
		label      *string
		cut        bool
	}{
		{1001, 2002, &cm, true},
		{3003, 4004, &op, false},
		{5005, 6006, nil, true},
	}
	var got []struct {
		start, end int64
		label      *string
		cut        bool
	}
	for rows.Next() {
		var s struct {
			start, end int64
			label      *string
			cut        bool
		}
		if err := rows.Scan(&s.start, &s.end, &s.label, &s.cut); err != nil {
			t.Fatalf("scanning span: %v", err)
		}
		got = append(got, s)
	}
	if len(got) != len(want) {
		t.Fatalf("restored spans = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].start != want[i].start || got[i].end != want[i].end || got[i].cut != want[i].cut {
			t.Errorf("span %d = [%d,%d) cut=%v, want [%d,%d) cut=%v",
				i, got[i].start, got[i].end, got[i].cut, want[i].start, want[i].end, want[i].cut)
		}
		switch {
		case want[i].label == nil && got[i].label != nil:
			t.Errorf("span %d label = %q, want NULL", i, *got[i].label)
		case want[i].label != nil && (got[i].label == nil || *got[i].label != *want[i].label):
			t.Errorf("span %d label = %v, want %q", i, got[i].label, *want[i].label)
		}
	}

	// 2 回目の rescue で増殖せず、落ちもしない。
	if _, err := RescueLatest(ctx, pool, mediaDir, []string{"default"}); err != nil {
		t.Fatalf("RescueLatest second: %v", err)
	}
	var spanCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM recording_chapter_spans WHERE recording_id = $1`, recID).Scan(&spanCount); err != nil {
		t.Fatalf("counting spans: %v", err)
	}
	if spanCount != 3 {
		t.Errorf("spans after second rescue = %d, want 3 (増殖しない)", spanCount)
	}
}
