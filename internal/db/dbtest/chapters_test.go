package dbtest_test

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestChapterSpan_DefaultsAndOptions(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	dbtest.AdoptChapters(t, pool, recordingID)
	dbtest.ChapterSpan(t, pool, recordingID)

	type span struct {
		startMs, endMs int64
		label          *string
		cut            bool
	}
	readSpans := func(id int64) []span {
		t.Helper()
		rows, err := pool.Query(t.Context(), `
SELECT lower(span), upper(span), label, cut FROM recording_chapter_spans
WHERE recording_id = $1 ORDER BY lower(span)`, id)
		if err != nil {
			t.Fatalf("reading chapter spans of %d: %v", id, err)
		}
		defer rows.Close()
		var got []span
		for rows.Next() {
			var s span
			testutil.RequireNoError(t, rows.Scan(&s.startMs, &s.endMs, &s.label, &s.cut), "scanning chapter span")
			got = append(got, s)
		}
		testutil.RequireNoError(t, rows.Err(), "iterating chapter spans")
		return got
	}

	wantLabel := "CM"
	if got := readSpans(recordingID); len(got) != 1 || got[0].startMs != 600000 || got[0].endMs != 900000 || got[0].label == nil || *got[0].label != wantLabel || !got[0].cut {
		t.Fatalf("default chapter span = %+v, want 600000..900000 CM cut", got)
	}

	overriddenID := dbtest.Recording(t, pool)
	dbtest.AdoptChapters(t, pool, overriddenID)
	dbtest.ChapterSpan(t, pool, overriddenID, func(p *dbtest.ChapterSpanParams) {
		p.StartMs, p.EndMs = 1200000, 1300000
		p.Label = nil
	})
	if got := readSpans(overriddenID); len(got) != 1 || got[0].startMs != 1200000 || got[0].endMs != 1300000 || got[0].label != nil || !got[0].cut {
		t.Fatalf("overridden chapter span = %+v, want 1200000..1300000 NULL label cut", got)
	}
}

func TestAdoptChapters_CreatesOwnershipOnce(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := dbtest.Recording(t, pool)
	dbtest.AdoptChapters(t, pool, recordingID)
	dbtest.AdoptChapters(t, pool, recordingID)

	var count int
	testutil.RequireNoError(t, pool.QueryRow(t.Context(), `
SELECT count(*) FROM recording_chapter_ownership WHERE recording_id = $1`, recordingID).Scan(&count), "counting ownership rows")
	if count != 1 {
		t.Fatalf("ownership rows = %d, want 1", count)
	}
}
