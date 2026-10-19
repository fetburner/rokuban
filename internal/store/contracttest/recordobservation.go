package contracttest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/fetburner/rokuban/internal/store"
)

// ObservedRecording は契約テストが検査する録画行の状態を表す。
type ObservedRecording struct {
	ID                int64
	Status            string
	Superseded        bool
	QualityEventCount int
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
}

func runRecordSerialization(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("concurrent-record", "finished")
	f.SeedRecordSync(t, observation)

	const contenders = 20
	start := make(chan struct{})
	errs := make(chan error, contenders)
	var ready sync.WaitGroup
	ready.Add(contenders)
	for range contenders {
		go func() {
			ready.Done()
			<-start
			_, err := f.Store.ProcessRecord(ctx, observation)
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	for range contenders {
		if err := <-errs; err != nil {
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
		id, err := f.Store.RecordFailedRecording(ctx, failed)
		if err != nil {
			t.Fatalf("RecordFailedRecording() = %v", err)
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

func requireStore(t *testing.T, f RecordObservationFixture) {
	t.Helper()
	if f.Store == nil || f.NewObservation == nil || f.SeedRecordSync == nil || f.SeedReservation == nil || f.InspectRecordSync == nil || f.InspectRecordings == nil {
		t.Fatalf("incomplete record observation fixture: %+v", f)
	}
}
