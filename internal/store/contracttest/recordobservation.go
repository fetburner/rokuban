package contracttest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// ObservedRecording は契約テストが検査する録画行の状態を表す。
type ObservedRecording struct {
	ID                int64
	Status            string
	Superseded        bool
	QualityEventCount int
	// QualityEventTimes は品質イベントの `at` を、保存された JSON から読み戻した値である。
	QualityEventTimes []time.Time
}

// RecordSyncState は契約テストが検査する record 同期行の状態を表す。
type RecordSyncState struct {
	Found       bool
	RecordingID *int64
	Status      string
}

// RecordObservationFixture は record 観測契約の実装と DB 状態の操作をまとめる。
type RecordObservationFixture struct {
	Store             store.RecordObservations
	NewObservation    func(recordID, status string) store.RecordObservation
	SeedRecordSync    func(t *testing.T, observation store.RecordObservation)
	SeedReservation   func(t *testing.T, observation store.RecordObservation)
	InspectRecordSync func(t *testing.T, site, recordID string) RecordSyncState
	InspectRecordings func(t *testing.T, observation store.RecordObservation) []ObservedRecording
	// SetRecordSyncObservedAt は record 同期行の観測時刻を書き換える。
	SetRecordSyncObservedAt func(t *testing.T, site, recordID string, observedAt time.Time)
	// SetAfterRecordAcquired は、実装が同じ record の観測の排他を確保した直後、
	// 何かを書く前に hook を呼ぶようにする。hook は t の終了時に外される。
	// 排他を確保する実装は、後発をここへ入れる前に待たせる。
	SetAfterRecordAcquired func(t *testing.T, hook func())
}

// NewRecordObservationStore は実在する契約実装と DB フィクスチャを作る。
type NewRecordObservationStore func(t *testing.T) RecordObservationFixture

// RunRecordObservations は record 観測の契約を実装に対して実行する。
func RunRecordObservations(t *testing.T, newStore NewRecordObservationStore) {
	t.Helper()
	checkedStore := func(t *testing.T) RecordObservationFixture {
		fixture := newStore(t)
		requireStore(t, fixture)
		return fixture
	}
	t.Run("same record observations serialize", func(t *testing.T) { runRecordSerialization(t, checkedStore) })
	t.Run("record sync rolls back with recording conflict", func(t *testing.T) { runRecordConflictRollback(t, checkedStore) })
	t.Run("success supersedes failed recording", func(t *testing.T) { runSuccessSupersedesFailure(t, checkedStore) })
	t.Run("failed quality observation is idempotent", func(t *testing.T) { runFailedQualityIdempotency(t, checkedStore) })
	t.Run("failed schedule is deduplicated and typed skip is returned", func(t *testing.T) { runFailedSchedule(t, checkedStore) })
	t.Run("failed event returns its recording id", func(t *testing.T) { runFailedEventRecordingID(t, checkedStore) })
	t.Run("failed event without reservation records nothing", func(t *testing.T) { runFailedEventWithoutReservation(t, checkedStore) })
	t.Run("failed schedule and failed event converge on one live recording", func(t *testing.T) { runFailedConvergence(t, checkedStore) })
	t.Run("stale record syncs are decided by the live id set", func(t *testing.T) { runStaleRecordSyncs(t, checkedStore) })
	t.Run("ingest hint follows the recording status", func(t *testing.T) { runIngestHint(t, checkedStore) })
	t.Run("failed record skipped by a live success keeps no recording", func(t *testing.T) { runFailedRecordSkipped(t, checkedStore) })
	t.Run("quality events carry a readable time", func(t *testing.T) { runQualityEventTime(t, checkedStore) })
}

// runRecordSerialization は、先発が排他の確保を通過した後、後発が先へ進む機会を与える。
// 排他があれば後発は確保で待たされ、先発の commit 後の状態を見て成功する。
// 排他が無ければ後発は先発と同時に録画を作ろうとして一意違反になる。
// 行が既にある状態で競合させるのは、行が無いと INSERT の一意制約だけで直列化され、
// 排他の有無を検出できないからである。
func runRecordSerialization(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("concurrent-record", "finished")
	f.SeedRecordSync(t, observation)

	const contenders = 2
	// grace は、先発が確保を通過してから、後発が追いつくのを待つ時間である。
	const grace = 500 * time.Millisecond
	var arrived atomic.Int32
	overlapped := make(chan struct{})
	f.SetAfterRecordAcquired(t, func() {
		if arrived.Add(1) == contenders {
			close(overlapped)
		}
		select {
		case <-overlapped:
		case <-time.After(grace):
		}
	})

	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.Store.ProcessRecord(ctx, observation)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ProcessRecord(concurrent) = %v, want every observation to succeed", err)
		}
	}
	state := f.InspectRecordSync(t, observation.Site, observation.RecordID)
	if !state.Found || state.RecordingID == nil {
		t.Fatalf("record_sync after concurrent observations = %+v, want one linked row", state)
	}
	recordings := f.InspectRecordings(t, observation)
	if len(recordings) != 1 {
		t.Fatalf("recordings for the event = %d, want 1 after %d concurrent observations", len(recordings), contenders)
	}
}

func runRecordConflictRollback(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	first := f.NewObservation("active-event-first", "finished")
	if _, err := f.Store.ProcessRecord(ctx, first); err != nil {
		t.Fatalf("ProcessRecord(first) = %v", err)
	}
	duplicate := first
	duplicate.RecordID = "active-event-duplicate"
	if _, err := f.Store.ProcessRecord(ctx, duplicate); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ProcessRecord(duplicate event) = %v, want ErrConflict", err)
	}
	state := f.InspectRecordSync(t, duplicate.Site, duplicate.RecordID)
	if state.Found {
		t.Fatalf("record_sync for rejected observation = %+v, want transaction rollback", state)
	}
	if got := len(f.InspectRecordings(t, first)); got != 1 {
		t.Fatalf("recordings after rejected observation = %d, want 1", got)
	}
}

func runSuccessSupersedesFailure(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	failed := f.NewObservation("failed-try", "failed")
	failed.FailedReason = []byte(`{"type":"signal-loss"}`)
	if _, err := f.Store.ProcessRecord(ctx, failed); err != nil {
		t.Fatalf("ProcessRecord(failed) = %v", err)
	}
	success := failed
	success.RecordID = "successful-retry"
	success.Status = "finished"
	success.FailedReason = nil
	if _, err := f.Store.ProcessRecord(ctx, success); err != nil {
		t.Fatalf("ProcessRecord(success) = %v", err)
	}
	got := f.InspectRecordings(t, failed)
	if len(got) != 2 {
		t.Fatalf("recordings for retried event = %d, want failed history plus success", len(got))
	}
	var supersededFailed, liveFinished int
	for _, row := range got {
		if row.Status == "failed" && row.Superseded {
			supersededFailed++
		}
		if row.Status == "finished" && !row.Superseded {
			liveFinished++
		}
	}
	if supersededFailed != 1 || liveFinished != 1 {
		t.Fatalf("recordings after retry = %+v, want one superseded failed row and one live finished row", got)
	}
}

func runFailedQualityIdempotency(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("failed-quality", "failed")
	observation.FailedReason = []byte(`{"type":"signal-loss"}`)
	for i := range 3 {
		result, err := f.Store.ProcessRecord(ctx, observation)
		if err != nil {
			t.Fatalf("ProcessRecord(failed, pass %d) = %v", i, err)
		}
		if result.FailedQualityEventAdded != (i == 0) {
			t.Fatalf("pass %d FailedQualityEventAdded = %t, want %t", i, result.FailedQualityEventAdded, i == 0)
		}
	}
	got := f.InspectRecordings(t, observation)
	if len(got) != 1 || got[0].QualityEventCount != 1 {
		t.Fatalf("failed observation state = %+v, want one recording and one quality event", got)
	}
}

func runFailedSchedule(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("failed-schedule", "failed")
	f.SeedReservation(t, observation)
	failed := store.FailedScheduleObservation{
		Site: observation.Site, ProgramID: observation.ProgramID,
		Snapshot: observation.Snapshot, Reason: []byte(`{"type":"signal-loss"}`),
	}
	for i := range 2 {
		result, err := f.Store.ProcessFailedSchedule(ctx, failed)
		if err != nil {
			t.Fatalf("ProcessFailedSchedule(pass %d) = %v", i, err)
		}
		if (result.RecordingID != nil) != (i == 0) || result.FailedQualityEventAdded != (i == 0) {
			t.Fatalf("pass %d result = %+v, want id/event only on first observation", i, result)
		}
	}
	if got := f.InspectRecordings(t, observation); len(got) != 1 || got[0].QualityEventCount != 1 {
		t.Fatalf("failed schedule state = %+v, want one recording and one quality event", got)
	}

	success := f.NewObservation("failed-schedule-success", "finished")
	success.ProgramID++
	success.Snapshot.EventID++
	f.SeedReservation(t, success)
	if _, err := f.Store.ProcessRecord(ctx, success); err != nil {
		t.Fatalf("ProcessRecord(success) = %v", err)
	}
	lateFailure := store.FailedScheduleObservation{
		Site: success.Site, ProgramID: success.ProgramID,
		Snapshot: success.Snapshot, Reason: []byte(`{"type":"signal-loss"}`),
	}
	result, err := f.Store.ProcessFailedSchedule(ctx, lateFailure)
	if err != nil {
		t.Fatalf("ProcessFailedSchedule(after success) = %v", err)
	}
	if !result.FailedRecordingSkipped || result.RecordingID != nil {
		t.Fatalf("late failed schedule result = %+v, want typed skipped result without id", result)
	}
	got := f.InspectRecordings(t, success)
	if len(got) != 1 || got[0].Status != "finished" || got[0].QualityEventCount != 0 {
		t.Fatalf("success after late failed schedule = %+v, want untouched success", got)
	}
}

func runFailedEventRecordingID(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("failed-event", "failed")
	f.SeedReservation(t, observation)
	failed := store.FailedRecordingObservation{
		Site: observation.Site, ProgramID: observation.ProgramID,
		Snapshot: observation.Snapshot, Reason: []byte(`{"type":"recording-failed"}`),
	}
	wantID := int64(0)
	for range 2 {
		id, recorded, err := f.Store.RecordFailedRecording(ctx, failed)
		if err != nil || !recorded {
			t.Fatalf("RecordFailedRecording() = (%d, %t, %v), want recorded", id, recorded, err)
		}
		if id == 0 || (wantID != 0 && id != wantID) {
			t.Fatalf("RecordFailedRecording() id = %d, want stable non-zero id %d", id, wantID)
		}
		wantID = id
	}
	got := f.InspectRecordings(t, observation)
	if len(got) != 1 || got[0].ID != wantID || got[0].QualityEventCount != 2 {
		t.Fatalf("recording.failed event state = %+v, want one recording with two observed events", got)
	}
}

func runFailedEventWithoutReservation(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	observation := f.NewObservation("failed-event-no-reservation", "failed")
	id, recorded, err := f.Store.RecordFailedRecording(context.Background(), store.FailedRecordingObservation{
		Site: observation.Site, ProgramID: observation.ProgramID,
		Snapshot: observation.Snapshot, Reason: []byte(`{"type":"recording-failed"}`),
	})
	if err != nil || recorded || id != 0 {
		t.Fatalf("RecordFailedRecording(no reservation) = (%d, %t, %v), want (0, false, nil)", id, recorded, err)
	}
	if got := f.InspectRecordings(t, observation); len(got) != 0 {
		t.Fatalf("recordings without reservation = %+v, want none", got)
	}
}

// runFailedConvergence は、SSE の recording.failed と sweep の失敗観測が並行して
// 同じ放送イベントを観測しても、生きた録画が 1 つに収束することを固定する。
func runFailedConvergence(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("failed-convergence", "failed")
	f.SeedReservation(t, observation)
	event := store.FailedRecordingObservation{
		Site: observation.Site, ProgramID: observation.ProgramID,
		Snapshot: observation.Snapshot, Reason: []byte(`{"type":"start-recording-failed"}`),
	}
	schedule := store.FailedScheduleObservation{
		Site: observation.Site, ProgramID: observation.ProgramID,
		Snapshot: observation.Snapshot, Reason: []byte(`{"type":"start-recording-failed"}`),
	}
	const contenders = 8
	start := make(chan struct{})
	errs := make(chan error, 2*contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := f.Store.RecordFailedRecording(ctx, event)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := f.Store.ProcessFailedSchedule(ctx, schedule)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent failed observation = %v, want success", err)
		}
	}
	got := f.InspectRecordings(t, observation)
	if len(got) != 1 || got[0].Status != "failed" || got[0].Superseded {
		t.Fatalf("recordings after concurrent failed observations = %+v, want one live failed recording", got)
	}
	if got[0].QualityEventCount < 1 {
		t.Fatalf("quality events = %d, want at least 1", got[0].QualityEventCount)
	}
}

func runStaleRecordSyncs(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	live := f.NewObservation("live-record", "finished")
	gone := f.NewObservation("gone-record", "finished")
	f.SeedRecordSync(t, live)
	f.SeedRecordSync(t, gone)
	// 渡した ID の行は、観測時刻がどれだけ古くても残す。
	f.SetRecordSyncObservedAt(t, live.Site, live.RecordID, time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC))

	deleted, err := f.Store.DeleteStaleRecordSyncs(ctx, live.Site, []string{live.RecordID})
	if err != nil {
		t.Fatalf("DeleteStaleRecordSyncs() = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteStaleRecordSyncs() deleted = %d, want 1", deleted)
	}
	if !f.InspectRecordSync(t, live.Site, live.RecordID).Found {
		t.Fatal("record_sync of the live record was deleted, want it kept regardless of observed time")
	}
	if f.InspectRecordSync(t, gone.Site, gone.RecordID).Found {
		t.Fatal("record_sync of the record missing from the live set was kept, want it deleted")
	}

	// 空の集合は、その site に record が 1 件も無いことを表す。
	if _, err := f.Store.DeleteStaleRecordSyncs(ctx, live.Site, nil); err != nil {
		t.Fatalf("DeleteStaleRecordSyncs(empty) = %v", err)
	}
	if f.InspectRecordSync(t, live.Site, live.RecordID).Found {
		t.Fatal("record_sync survived an empty live set, want every row of the site deleted")
	}
}

func runIngestHint(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	for i, tc := range []struct {
		status       string
		managed      bool
		wantIngest   bool
		wantPriority int
	}{
		{"recording", true, true, 0},
		{"finished", true, true, 2},
		{"canceled", true, false, 0},
		{"failed", true, false, 0},
		{"finished", false, false, 0},
	} {
		observation := f.NewObservation("ingest-"+tc.status+"-"+string(rune('a'+i)), tc.status)
		observation.Managed = tc.managed
		observation.Snapshot.EventID += int32(i) // 放送イベントを分け、録画の衝突を避ける
		result, err := f.Store.ProcessRecord(ctx, observation)
		if err != nil {
			t.Fatalf("ProcessRecord(%s, managed=%t) = %v", tc.status, tc.managed, err)
		}
		if result.EnqueueIngest != tc.wantIngest || result.IngestPriority != tc.wantPriority {
			t.Fatalf("ProcessRecord(%s, managed=%t) ingest = (%t, %d), want (%t, %d)",
				tc.status, tc.managed, result.EnqueueIngest, result.IngestPriority, tc.wantIngest, tc.wantPriority)
		}
	}
}

func runFailedRecordSkipped(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	success := f.NewObservation("live-success", "finished")
	if _, err := f.Store.ProcessRecord(ctx, success); err != nil {
		t.Fatalf("ProcessRecord(success) = %v", err)
	}
	failed := success
	failed.RecordID = "late-failed"
	failed.Status = "failed"
	failed.FailedReason = []byte(`{"type":"signal-loss"}`)
	result, err := f.Store.ProcessRecord(ctx, failed)
	if err != nil {
		t.Fatalf("ProcessRecord(late failed) = %v", err)
	}
	if !result.FailedRecordingSkipped || result.RecordingID != nil || result.EnqueueIngest || result.FailedQualityEventAdded {
		t.Fatalf("late failed record result = %+v, want skipped without recording, ingest, or quality event", result)
	}
	if state := f.InspectRecordSync(t, failed.Site, failed.RecordID); !state.Found || state.RecordingID != nil || state.Status != "failed" {
		t.Fatalf("record_sync of skipped failed record = %+v, want an observation without recording", state)
	}
	got := f.InspectRecordings(t, success)
	if len(got) != 1 || got[0].Status != "finished" || got[0].QualityEventCount != 0 {
		t.Fatalf("recordings after skipped failed record = %+v, want the untouched success", got)
	}
}

func runQualityEventTime(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	failed := f.NewObservation("quality-time", "failed")
	failed.FailedReason = []byte(`{"type":"signal-loss"}`)
	f.SeedReservation(t, failed)
	if _, err := f.Store.ProcessRecord(ctx, failed); err != nil {
		t.Fatalf("ProcessRecord(failed) = %v", err)
	}
	if _, _, err := f.Store.RecordFailedRecording(ctx, store.FailedRecordingObservation{
		Site: failed.Site, ProgramID: failed.ProgramID, Snapshot: failed.Snapshot, Reason: []byte(`{"type":"other"}`),
	}); err != nil {
		t.Fatalf("RecordFailedRecording() = %v", err)
	}
	if _, err := f.Store.AppendRecordBrokenEvent(ctx, failed.Site, failed.RecordID, "io-error"); err != nil {
		t.Fatalf("AppendRecordBrokenEvent() = %v", err)
	}
	got := f.InspectRecordings(t, failed)
	if len(got) != 1 || len(got[0].QualityEventTimes) != 3 {
		t.Fatalf("recordings = %+v, want one recording with 3 quality events", got)
	}
	requireRecentTimes(t, got[0].QualityEventTimes)

	// 録画を新しく作る経路でも同じである。
	fresh := f.NewObservation("quality-time-fresh", "failed")
	fresh.ProgramID++
	fresh.Snapshot.EventID++
	f.SeedReservation(t, fresh)
	if _, _, err := f.Store.RecordFailedRecording(ctx, store.FailedRecordingObservation{
		Site: fresh.Site, ProgramID: fresh.ProgramID, Snapshot: fresh.Snapshot, Reason: []byte(`{"type":"other"}`),
	}); err != nil {
		t.Fatalf("RecordFailedRecording(fresh) = %v", err)
	}
	got = f.InspectRecordings(t, fresh)
	if len(got) != 1 || len(got[0].QualityEventTimes) != 1 {
		t.Fatalf("fresh recordings = %+v, want one recording with 1 quality event", got)
	}
	requireRecentTimes(t, got[0].QualityEventTimes)
}

func requireRecentTimes(t *testing.T, times []time.Time) {
	t.Helper()
	for i, at := range times {
		if d := time.Since(at); at.IsZero() || d < -time.Minute || d > time.Minute {
			t.Fatalf("quality event %d at = %v, want a time within a minute of now", i, at)
		}
	}
}

func requireStore(t *testing.T, f RecordObservationFixture) {
	t.Helper()
	if f.Store == nil || f.NewObservation == nil || f.SeedRecordSync == nil || f.SeedReservation == nil || f.InspectRecordSync == nil || f.InspectRecordings == nil || f.SetRecordSyncObservedAt == nil || f.SetAfterRecordAcquired == nil {
		t.Fatalf("incomplete record observation fixture: %+v", f)
	}
}
