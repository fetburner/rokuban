package pgstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestQualityEventAtIsAlwaysDBClock は、呼び出し側が `at` を含めて渡しても、
// 品質イベントを書く 3 つのクエリが DB の時計で上書きすることを固定する。
func TestQualityEventAtIsAlwaysDBClock(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)
	old := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	events := func(reason string) json.RawMessage {
		b, err := json.Marshal([]db.QualityEvent{{At: old, Event: "recording.failed", Reason: json.RawMessage(reason)}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	started := time.Date(2026, time.May, 1, 10, 0, 0, 0, time.UTC)
	snap := store.RecordingSnapshot{
		NetworkID: 1, ServiceID: 2, EventID: 3, ServiceName: "局", ChannelType: "GR", Channel: "27",
		Title: "番組", ProgramStartAt: started, ProgramDurationMs: 1000,
	}
	id, err := q.CreateFailedRecording(ctx, withFailedEventSnapshot(snap, sqlcgen.CreateFailedRecordingParams{
		Source: "rule", Site: "s", NetworkID: 1, ServiceID: 2, EventID: 3, QualityEvents: events(`{"n":1}`),
	}))
	if err != nil {
		t.Fatalf("CreateFailedRecording: %v", err)
	}
	if _, err := q.AppendQualityEventsIfMissing(ctx, sqlcgen.AppendQualityEventsIfMissingParams{ID: id, Events: events(`{"n":2}`)}); err != nil {
		t.Fatalf("AppendQualityEventsIfMissing: %v", err)
	}
	if err := q.AppendQualityEvents(ctx, sqlcgen.AppendQualityEventsParams{ID: id, Events: events(`{"n":3}`)}); err != nil {
		t.Fatalf("AppendQualityEvents: %v", err)
	}
	var raw json.RawMessage
	if err := pool.QueryRow(ctx, `SELECT quality_events FROM recordings WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got []db.QualityEvent
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("quality events = %d, want 3", len(got))
	}
	for i, e := range got {
		if d := time.Since(e.At); d < -time.Minute || d > time.Minute {
			t.Fatalf("event %d at = %v, want the DB clock (caller-supplied at must be overwritten)", i, e.At)
		}
	}
}
