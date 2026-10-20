package contracttest

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// ScheduleSyncFixture は schedule 同期契約とその状態を作るフックをまとめる。
type ScheduleSyncFixture struct {
	Writer store.ScheduleSync
	Reader store.ScheduleSyncReader
	// SeedSyncEvaluation は同期評価 query で読む予約と番組スナップショットを作る。
	SeedSyncEvaluation func(t *testing.T, site string, programID int64, title string)
	// SeedStartedRecording は開始時刻を持つ録画を作る。
	SeedStartedRecording func(t *testing.T, site string, key store.BroadcastEventKey, startedAt time.Time)
	// SetSnapshotAt は比較用の既知時刻を snapshot marker に設定する。
	SetSnapshotAt func(t *testing.T, site string, at time.Time)
	// FailSnapshotUpdate は marker upsert を失敗させる。
	FailSnapshotUpdate func(t *testing.T)
}

// NewScheduleSyncStore は契約テスト用の store を作る。
type NewScheduleSyncStore func(t *testing.T) ScheduleSyncFixture

// RunScheduleSync は schedule 同期契約を実装に対して実行する。
func RunScheduleSync(t *testing.T, newStore NewScheduleSyncStore) {
	t.Helper()
	t.Run("replace removes stale observations and advances marker", func(t *testing.T) { testScheduleSyncReplacement(t, newStore) })
	t.Run("failed replacement preserves prior observations and marker", func(t *testing.T) { testScheduleSyncUpsertFailure(t, newStore) })
	t.Run("marker failure rolls back observations", func(t *testing.T) { testScheduleSyncMarkerFailure(t, newStore) })
	t.Run("replacement is scoped to one site", func(t *testing.T) { testScheduleSyncSiteScope(t, newStore) })
	t.Run("sync reads are scoped to one site", func(t *testing.T) { testScheduleSyncReadSiteScope(t, newStore) })
}

func testScheduleSyncReplacement(t *testing.T, newStore NewScheduleSyncStore) {
	f := newStore(t)
	ctx := context.Background()
	const site = "contract-schedule-sync"
	if err := f.Writer.ReplaceObserved(ctx, site, []store.ObservedSchedule{{
		ProgramID: 101, State: "scheduled", Options: json.RawMessage(`{}`), Tags: []string{},
	}}); err != nil {
		t.Fatalf("creating initial snapshot: %v", err)
	}
	f.SetSnapshotAt(t, site, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	before, found, err := f.Reader.SnapshotAt(ctx, site)
	if err != nil || !found {
		t.Fatalf("SnapshotAt(initial) = (%v, %t, %v), want marker", before, found, err)
	}
	if err := f.Writer.ReplaceObserved(ctx, site, []store.ObservedSchedule{{
		ProgramID: 202, State: "recording", Options: json.RawMessage(`{"priority":1}`), Tags: []string{"program:202"},
		FailedReason: json.RawMessage(`{"reason":"preempted"}`),
	}}); err != nil {
		t.Fatalf("replacing snapshot: %v", err)
	}
	after, found, err := f.Reader.SnapshotAt(ctx, site)
	if err != nil || !found || !after.After(before) {
		t.Fatalf("SnapshotAt(after replace) = (%v, %t, %v), want a newer marker than %v", after, found, err, before)
	}
	rows, err := f.Reader.ListObserved(ctx, site)
	if err != nil {
		t.Fatalf("listing observations: %v", err)
	}
	if len(rows) != 1 || rows[0].ProgramID != 202 || rows[0].State != "recording" {
		t.Fatalf("observations after replacement = %+v, want only program 202", rows)
	}
	var options map[string]int
	if err := json.Unmarshal(rows[0].Options, &options); err != nil || len(options) != 1 || options["priority"] != 1 {
		t.Fatalf("options after replacement = (%v, %v), want priority 1", options, err)
	}
	if !slices.Equal(rows[0].Tags, []string{"program:202"}) {
		t.Fatalf("tags after replacement = %v, want [program:202]", rows[0].Tags)
	}
	var failedReason struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rows[0].FailedReason, &failedReason); err != nil || failedReason.Reason != "preempted" {
		t.Fatalf("failed reason after replacement = (%+v, %v), want reason preempted", failedReason, err)
	}
}

func testScheduleSyncUpsertFailure(t *testing.T, newStore NewScheduleSyncStore) {
	f := newStore(t)
	ctx := context.Background()
	const site = "contract-schedule-sync-rollback"
	old := store.ObservedSchedule{ProgramID: 301, State: "scheduled", Options: json.RawMessage(`{}`), Tags: []string{"program:301"}}
	if err := f.Writer.ReplaceObserved(ctx, site, []store.ObservedSchedule{old}); err != nil {
		t.Fatalf("creating initial snapshot: %v", err)
	}
	f.SetSnapshotAt(t, site, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	before, found, err := f.Reader.SnapshotAt(ctx, site)
	if err != nil || !found {
		t.Fatalf("SnapshotAt(before failure) = (%v, %t, %v), want marker", before, found, err)
	}
	invalid := []store.ObservedSchedule{
		{ProgramID: 302, State: "scheduled", Options: json.RawMessage(`{}`), Tags: []string{"program:302"}},
		{ProgramID: 303, State: "scheduled", Options: json.RawMessage(`{`), Tags: []string{"program:303"}},
	}
	if err := f.Writer.ReplaceObserved(ctx, site, invalid); err == nil {
		t.Fatal("ReplaceObserved(invalid options) = nil, want database error")
	}
	assertScheduleSyncSnapshotState(t, f, site, old.ProgramID, old.State, before)
}

func testScheduleSyncMarkerFailure(t *testing.T, newStore NewScheduleSyncStore) {
	f := newStore(t)
	ctx := context.Background()
	const site = "contract-schedule-sync-marker-failure"
	old := store.ObservedSchedule{ProgramID: 401, State: "scheduled", Options: json.RawMessage(`{}`), Tags: []string{"program:401"}}
	if err := f.Writer.ReplaceObserved(ctx, site, []store.ObservedSchedule{old}); err != nil {
		t.Fatalf("creating initial snapshot: %v", err)
	}
	f.SetSnapshotAt(t, site, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	before, found, err := f.Reader.SnapshotAt(ctx, site)
	if err != nil || !found {
		t.Fatalf("SnapshotAt(before marker failure) = (%v, %t, %v), want marker", before, found, err)
	}
	f.FailSnapshotUpdate(t)
	replacement := store.ObservedSchedule{ProgramID: 402, State: "recording", Options: json.RawMessage(`{}`), Tags: []string{"program:402"}}
	if err := f.Writer.ReplaceObserved(ctx, site, []store.ObservedSchedule{replacement}); err == nil {
		t.Fatal("ReplaceObserved(marker failure) = nil, want database error")
	}
	assertScheduleSyncSnapshotState(t, f, site, old.ProgramID, old.State, before)
}

func testScheduleSyncSiteScope(t *testing.T, newStore NewScheduleSyncStore) {
	f := newStore(t)
	ctx := context.Background()
	const siteA = "contract-schedule-sync-site-a"
	const siteB = "contract-schedule-sync-site-b"
	for _, seed := range []struct {
		site      string
		programID int64
	}{
		{siteA, 411},
		{siteB, 412},
	} {
		if err := f.Writer.ReplaceObserved(ctx, seed.site, []store.ObservedSchedule{{
			ProgramID: seed.programID, State: "scheduled", Options: json.RawMessage(`{}`), Tags: []string{},
		}}); err != nil {
			t.Fatalf("seeding site %q: %v", seed.site, err)
		}
	}
	f.SetSnapshotAt(t, siteA, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	f.SetSnapshotAt(t, siteB, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	markerB, found, err := f.Reader.SnapshotAt(ctx, siteB)
	if err != nil || !found {
		t.Fatalf("SnapshotAt(site B) = (%v, %t, %v), want marker", markerB, found, err)
	}
	if err := f.Writer.ReplaceObserved(ctx, siteA, []store.ObservedSchedule{{
		ProgramID: 413, State: "recording", Options: json.RawMessage(`{}`), Tags: []string{},
	}}); err != nil {
		t.Fatalf("replacing site A: %v", err)
	}
	rowsA, err := f.Reader.ListObserved(ctx, siteA)
	if err != nil {
		t.Fatalf("listing site A observations: %v", err)
	}
	if len(rowsA) != 1 || rowsA[0].ProgramID != 413 {
		t.Fatalf("site A observations = %+v, want program 413 only", rowsA)
	}
	assertScheduleSyncSnapshotState(t, f, siteB, 412, "scheduled", markerB)
}

func testScheduleSyncReadSiteScope(t *testing.T, newStore NewScheduleSyncStore) {
	f := newStore(t)
	ctx := context.Background()
	const siteA = "contract-schedule-sync-read-site-a"
	const siteB = "contract-schedule-sync-read-site-b"
	const programID int64 = 421
	f.SeedSyncEvaluation(t, siteA, programID, "site A")
	f.SeedSyncEvaluation(t, siteB, programID, "site B")
	rows, err := f.Writer.ListForSyncEvaluation(ctx, siteA)
	if err != nil {
		t.Fatalf("ListForSyncEvaluation(site A): %v", err)
	}
	if len(rows) != 1 || rows[0].Reservation.ProgramID != programID || rows[0].ProgramSnapshot.Title != "site A" {
		t.Fatalf("ListForSyncEvaluation(site A) = %+v, want only site A program %d", rows, programID)
	}

	startedAfter := time.Now().Add(-24 * time.Hour)
	keyA := store.BroadcastEventKey{NetworkID: 100, ServiceID: 200, EventID: 300}
	keyB := store.BroadcastEventKey{NetworkID: 101, ServiceID: 201, EventID: 301}
	f.SeedStartedRecording(t, siteA, keyA, startedAfter.Add(time.Minute))
	f.SeedStartedRecording(t, siteB, keyB, startedAfter.Add(time.Minute))
	started, err := f.Writer.ListStartedBroadcastEventKeys(ctx, siteA, []store.BroadcastEventKey{keyA, keyB}, startedAfter)
	if err != nil {
		t.Fatalf("ListStartedBroadcastEventKeys(site A): %v", err)
	}
	if len(started) != 1 || started[0] != keyA {
		t.Fatalf("ListStartedBroadcastEventKeys(site A) = %+v, want only site A key %+v", started, keyA)
	}
}

func assertScheduleSyncSnapshotState(t *testing.T, f ScheduleSyncFixture, site string, programID int64, state string, marker time.Time) {
	t.Helper()
	after, found, err := f.Reader.SnapshotAt(context.Background(), site)
	if err != nil || !found || !after.Equal(marker) {
		t.Fatalf("SnapshotAt(%s after operation) = (%v, %t, %v), want unchanged marker %v", site, after, found, err, marker)
	}
	rows, err := f.Reader.ListObserved(context.Background(), site)
	if err != nil {
		t.Fatalf("listing observations for site %s after operation: %v", site, err)
	}
	if len(rows) != 1 || rows[0].ProgramID != programID || rows[0].State != state {
		t.Fatalf("observations for site %s after operation = %+v, want program %d in state %q only", site, rows, programID, state)
	}
}
