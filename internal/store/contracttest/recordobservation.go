package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// ObservedRecording は契約テストが検査する録画行の状態を表す。
type ObservedRecording struct {
	ID                int64
	Status            string
	Source            string
	Superseded        bool
	Deleted           bool
	Snapshot          store.RecordingSnapshot
	QualityEventCount int
	QualityEventNames []string
	// QualityEventTimes は品質イベントの `at` を、保存された JSON から読み戻した値である。
	QualityEventTimes []time.Time
}

// ObservedMediaAsset は契約テストが検査する media asset の所有者と状態を表す。
type ObservedMediaAsset struct {
	RecordingID int64
	State       string
	Deleted     bool
}

// RecordSyncState は契約テストが検査する record 同期行の状態を表す。
type RecordSyncState struct {
	Found       bool
	RecordingID *int64
	Status      string
}

// RecordObservationFixture は record 観測契約の実装と DB 状態の操作をまとめる。
type RecordObservationFixture struct {
	Store               store.RecordObservations
	Catalog             store.Catalog
	Reservations        store.Reservations
	RulerPass           store.RulerPass
	Rules               store.Rules
	IngestTarget        store.IngestTarget
	Backlog             store.RecordBacklog
	NewObservation      func(recordID, status string) store.RecordObservation
	SeedProgramSnapshot func(t *testing.T, observation store.RecordObservation)
	SeedRecordSync      func(t *testing.T, observation store.RecordObservation)
	SeedReservation     func(t *testing.T, observation store.RecordObservation)
	NewOriginalAsset    func(t *testing.T, recordingID int64, state string)
	// RecordSyncStatus is retained because no public store read exposes record_sync.status.
	RecordSyncStatus func(t *testing.T, site, recordID string) string
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
	t.Run("failed event snapshot is persisted", func(t *testing.T) { runFailedEventSnapshot(t, checkedStore) })
	t.Run("failed event without reservation records nothing", func(t *testing.T) { runFailedEventWithoutReservation(t, checkedStore) })
	t.Run("failed schedule and failed event converge on one live recording", func(t *testing.T) { runFailedConvergence(t, checkedStore) })
	t.Run("stale record syncs are decided by the live id set", func(t *testing.T) { runStaleRecordSyncs(t, checkedStore) })
	t.Run("ingest hint follows the recording status", func(t *testing.T) { runIngestHint(t, checkedStore) })
	t.Run("ingest target preserves nullable fields", func(t *testing.T) { runIngestTarget(t, checkedStore) })
	t.Run("ingest backlog filters records and bytes", func(t *testing.T) { runRecordBacklog(t, checkedStore) })
	t.Run("failed record skipped by a live success keeps no recording", func(t *testing.T) { runFailedRecordSkipped(t, checkedStore) })
	t.Run("quality events carry a readable time", func(t *testing.T) { runQualityEventTime(t, checkedStore) })
	t.Run("record broken event appends to its linked recording", func(t *testing.T) { runRecordBrokenEvent(t, checkedStore) })
	t.Run("unmanaged record is synchronized without recording", func(t *testing.T) { runUnmanagedRecord(t, checkedStore) })
	t.Run("record snapshot is persisted", func(t *testing.T) { runRecordSnapshot(t, checkedStore) })
	t.Run("record status progresses", func(t *testing.T) { runRecordStatusProgression(t, checkedStore) })
	t.Run("record status does not downgrade", func(t *testing.T) { runRecordStatusNoDowngrade(t, checkedStore) })
	t.Run("record status values are normalized", func(t *testing.T) { runRecordStatusValues(t, checkedStore) })
	t.Run("event ID reuse with a new start time creates a new recording", func(t *testing.T) { runEventIDReuse(t, checkedStore) })
	t.Run("record source", func(t *testing.T) { runRecordSource(t, checkedStore) })
	t.Run("failed event source follows intent", func(t *testing.T) { runFailedEventSource(t, checkedStore) })
	t.Run("failed schedule source follows intent", func(t *testing.T) { runFailedScheduleSource(t, checkedStore) })
}

func runRecordStatusProgression(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("status-progression", "recording")
	if _, err := f.Store.ProcessRecord(ctx, observation); err != nil {
		t.Fatalf("ProcessRecord(recording): %v", err)
	}
	observation.Status = "finished"
	if _, err := f.Store.ProcessRecord(ctx, observation); err != nil {
		t.Fatalf("ProcessRecord(finished): %v", err)
	}
	if _, err := f.Store.ProcessRecord(ctx, observation); err != nil {
		t.Fatalf("ProcessRecord(finished again): %v", err)
	}
	got := observedRecordings(t, f, observation)
	if len(got) != 1 || got[0].Status != "finished" {
		t.Fatalf("recordings after status progression = %+v, want one finished recording", got)
	}
}

func runRecordBrokenEvent(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("record-broken", "recording")
	result, err := f.Store.ProcessRecord(ctx, observation)
	if err != nil {
		t.Fatalf("ProcessRecord: %v", err)
	}
	if result.RecordingID == nil {
		t.Fatal("ProcessRecord returned no recording ID")
	}

	appended, err := f.Store.AppendRecordBrokenEvent(ctx, observation.Site, observation.RecordID, "io-error")
	if err != nil || !appended {
		t.Fatalf("AppendRecordBrokenEvent(linked record) = (%t, %v), want appended", appended, err)
	}
	got := observedRecordings(t, f, observation)
	if len(got) != 1 || got[0].ID != *result.RecordingID || got[0].QualityEventCount != 1 || !reflect.DeepEqual(got[0].QualityEventNames, []string{"recording.record-broken"}) {
		t.Fatalf("record after broken event = %+v, want one linked recording with a record-broken event", got)
	}

	appended, err = f.Store.AppendRecordBrokenEvent(ctx, observation.Site, "missing-record", "io-error")
	if err != nil || appended {
		t.Fatalf("AppendRecordBrokenEvent(unlinked record) = (%t, %v), want no event and no error", appended, err)
	}
	got = observedRecordings(t, f, observation)
	if len(got) != 1 || got[0].QualityEventCount != 1 {
		t.Fatalf("record after broken event for missing record = %+v, want no additional event", got)
	}
}

func runUnmanagedRecord(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("unmanaged-record", "finished")
	observation.Managed = false
	observation.Tags = nil

	result, err := f.Store.ProcessRecord(ctx, observation)
	if err != nil {
		t.Fatalf("ProcessRecord(unmanaged) = %v, want a synchronized observation without a recording", err)
	}
	if result.RecordingID != nil || result.EnqueueIngest {
		t.Fatalf("ProcessRecord(unmanaged) = %+v, want no recording ID and no ingest", result)
	}
	if got := recordSyncState(t, f, observation.Site, observation.RecordID); !got.Found || got.RecordingID != nil {
		t.Fatalf("record_sync for unmanaged record = %+v, want a row without recording ID", got)
	}
	if got := observedRecordings(t, f, observation); len(got) != 0 {
		t.Fatalf("recordings for unmanaged record = %+v, want none", got)
	}
}

func runRecordSnapshot(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	observation := f.NewObservation("record-snapshot", "finished")
	description := "番組説明"
	observation.Snapshot = store.RecordingSnapshot{
		NetworkID: 32736, ServiceID: 1024, EventID: 111,
		ServiceName: "NHK総合", ChannelType: "GR", Channel: "27", Title: "録画スナップショット",
		Description: &description, Extended: []byte(`{"出演者":"テスト太郎"}`),
		Genres: []byte(`[{"level1":1,"level2":2,"un1":3,"un2":4}]`), IsFree: true,
		ProgramStartAt:    time.Date(2026, time.July, 7, 12, 34, 56, 789000000, time.UTC),
		ProgramDurationMs: 3_600_000,
	}
	result, err := f.Store.ProcessRecord(context.Background(), observation)
	if err != nil {
		t.Fatalf("ProcessRecord: %v", err)
	}
	if result.RecordingID == nil {
		t.Fatal("ProcessRecord returned no recording ID")
	}
	got := observedRecordings(t, f, observation)
	if len(got) != 1 || got[0].ID != *result.RecordingID {
		t.Fatalf("recordings = %+v, want one recording with ID %d", got, *result.RecordingID)
	}
	requireRecordingSnapshot(t, got[0].Snapshot, observation.Snapshot)
}

func requireRecordingSnapshot(t *testing.T, got, want store.RecordingSnapshot) {
	t.Helper()
	if got.NetworkID != want.NetworkID || got.ServiceID != want.ServiceID || got.EventID != want.EventID ||
		got.ServiceName != want.ServiceName || got.ChannelType != want.ChannelType || got.Channel != want.Channel ||
		got.Title != want.Title || !reflect.DeepEqual(got.Description, want.Description) || got.IsFree != want.IsFree ||
		!got.ProgramStartAt.Equal(want.ProgramStartAt) || got.ProgramDurationMs != want.ProgramDurationMs {
		t.Errorf("recording snapshot = %+v, want %+v", got, want)
	}
	for _, field := range []struct {
		name      string
		got, want []byte
	}{{"extended", got.Extended, want.Extended}, {"genres", got.Genres, want.Genres}} {
		var gotJSON, wantJSON any
		if err := json.Unmarshal(field.got, &gotJSON); err != nil {
			t.Fatalf("unmarshalling stored snapshot %s %q: %v", field.name, field.got, err)
		}
		if err := json.Unmarshal(field.want, &wantJSON); err != nil {
			t.Fatalf("unmarshalling expected snapshot %s %q: %v", field.name, field.want, err)
		}
		if !reflect.DeepEqual(gotJSON, wantJSON) {
			t.Errorf("recording snapshot %s = %s, want %s", field.name, field.got, field.want)
		}
	}
}

func runRecordStatusNoDowngrade(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	observation := f.NewObservation("status-no-downgrade", "finished")
	if _, err := f.Store.ProcessRecord(ctx, observation); err != nil {
		t.Fatalf("ProcessRecord(finished): %v", err)
	}
	observation.Status = "recording"
	if _, err := f.Store.ProcessRecord(ctx, observation); err != nil {
		t.Fatalf("ProcessRecord(recording): %v", err)
	}
	got := observedRecordings(t, f, observation)
	if len(got) != 1 || got[0].Status != "finished" {
		t.Fatalf("recordings after older status = %+v, want one finished recording", got)
	}
}

func runRecordStatusValues(t *testing.T, newStore NewRecordObservationStore) {
	for i, tc := range []struct {
		status string
		want   string
	}{
		{status: "recording", want: "recording"},
		{status: "finished", want: "finished"},
		{status: "canceled", want: "canceled"},
		{status: "failed", want: "failed"},
		{status: "rescheduling", want: "failed"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			f := newStore(t)
			observation := f.NewObservation("status-value-"+strconv.Itoa(i), tc.status)
			result, err := f.Store.ProcessRecord(context.Background(), observation)
			if err != nil {
				t.Fatalf("ProcessRecord(%q): %v", tc.status, err)
			}
			if result.RecordingID == nil {
				t.Fatal("ProcessRecord returned no recording ID")
			}
			if got := recordSyncState(t, f, observation.Site, observation.RecordID); !got.Found || got.Status != tc.status || got.RecordingID == nil {
				t.Fatalf("record_sync = %+v, want raw status %q and a recording ID", got, tc.status)
			}
			got := observedRecordings(t, f, observation)
			if len(got) != 1 || got[0].Status != tc.want {
				t.Fatalf("recordings = %+v, want one recording with normalized status %q", got, tc.want)
			}
		})
	}
}

func runEventIDReuse(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	first := f.NewObservation("event-first", "failed")
	firstResult, err := f.Store.ProcessRecord(ctx, first)
	if err != nil {
		t.Fatalf("ProcessRecord(first): %v", err)
	}
	second := first
	second.RecordID = "event-second"
	second.ProgramID++
	second.Status = "finished"
	second.Snapshot.ProgramStartAt = second.Snapshot.ProgramStartAt.Add(24 * time.Hour)
	secondResult, err := f.Store.ProcessRecord(ctx, second)
	if err != nil {
		t.Fatalf("ProcessRecord(second start time): %v", err)
	}
	if firstResult.RecordingID == nil || secondResult.RecordingID == nil || *firstResult.RecordingID == *secondResult.RecordingID {
		t.Fatalf("recording IDs for reused event = (%s, %s), want distinct recordings", int64Text(firstResult.RecordingID), int64Text(secondResult.RecordingID))
	}
	if got := observedRecordings(t, f, first); len(got) != 1 || got[0].ID != *firstResult.RecordingID || got[0].Status != "failed" || got[0].Superseded {
		t.Fatalf("old event recording = %+v, want live failed ID %d", got, *firstResult.RecordingID)
	}
	if got := observedRecordings(t, f, second); len(got) != 1 || got[0].ID != *secondResult.RecordingID {
		t.Fatalf("reused event recording = %+v, want ID %d", got, *secondResult.RecordingID)
	}

	failedFirst := f.NewObservation("failed-event-first", "failed")
	failedFirst.ProgramID += 100
	failedFirst.Snapshot.EventID++
	f.SeedReservation(t, failedFirst)
	failedFirstID, recorded, err := f.Store.RecordFailedRecording(ctx, store.FailedRecordingObservation{
		Site: failedFirst.Site, ProgramID: failedFirst.ProgramID, Snapshot: failedFirst.Snapshot,
		Reason: []byte(`{"type":"recording-failed"}`),
	})
	if err != nil || !recorded {
		t.Fatalf("RecordFailedRecording(first): (%d, %t, %v), want recorded", failedFirstID, recorded, err)
	}
	failedSecond := failedFirst
	failedSecond.RecordID = "failed-event-second"
	failedSecond.Snapshot.ProgramStartAt = failedSecond.Snapshot.ProgramStartAt.Add(24 * time.Hour)
	f.SeedReservation(t, failedSecond)
	failedSecondID, recorded, err := f.Store.RecordFailedRecording(ctx, store.FailedRecordingObservation{
		Site: failedSecond.Site, ProgramID: failedSecond.ProgramID, Snapshot: failedSecond.Snapshot,
		Reason: []byte(`{"type":"recording-failed"}`),
	})
	if err != nil || !recorded || failedFirstID == failedSecondID {
		t.Fatalf("RecordFailedRecording(reused event) = (%d, %t, %v), want a distinct recording from %d", failedSecondID, recorded, err, failedFirstID)
	}
	if got := observedRecordings(t, f, failedFirst); len(got) != 1 || got[0].ID != failedFirstID {
		t.Fatalf("first failed event recording = %+v, want ID %d", got, failedFirstID)
	}
	if got := observedRecordings(t, f, failedSecond); len(got) != 1 || got[0].ID != failedSecondID {
		t.Fatalf("reused failed event recording = %+v, want ID %d", got, failedSecondID)
	}
}

func runRecordSource(t *testing.T, newStore NewRecordObservationStore) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f RecordObservationFixture, observation store.RecordObservation)
		want  string
	}{
		{
			name: "manual reservation with matching rule",
			setup: func(t *testing.T, f RecordObservationFixture, observation store.RecordObservation) {
				seedRuleReservation(t, f, observation)
				if err := f.Reservations.PutIntent(context.Background(), observation.Site, observation.ProgramID, reservation.IntentRecord); err != nil {
					t.Fatalf("PutIntent: %v", err)
				}
			},
			want: reservation.SourceManual,
		},
		{
			name: "manual intent without reservation",
			setup: func(t *testing.T, f RecordObservationFixture, observation store.RecordObservation) {
				f.SeedProgramSnapshot(t, observation)
				if err := f.Reservations.PutIntent(context.Background(), observation.Site, observation.ProgramID, reservation.IntentRecord); err != nil {
					t.Fatalf("PutIntent: %v", err)
				}
			},
			want: reservation.SourceManual,
		},
		{
			name: "rule reservation",
			setup: func(t *testing.T, f RecordObservationFixture, observation store.RecordObservation) {
				seedRuleReservation(t, f, observation)
			},
			want: reservation.SourceRule,
		},
		{
			name: "rule reservation with override",
			setup: func(t *testing.T, f RecordObservationFixture, observation store.RecordObservation) {
				seedRuleReservation(t, f, observation)
				priority := 5
				if err := f.Reservations.PatchOverrides(context.Background(), observation.Site, observation.ProgramID, reservation.OverridesPatch{Priority: &priority}); err != nil {
					t.Fatalf("PatchOverrides: %v", err)
				}
			},
			want: reservation.SourceRule,
		},
		{
			name: "missing reservation",
			want: reservation.SourceUnattributed,
		},
		{
			name: "GC removes source reservation",
			setup: func(t *testing.T, f RecordObservationFixture, observation store.RecordObservation) {
				observation.Snapshot.ProgramStartAt = time.Now().UTC().Add(-48 * time.Hour)
				seedRuleReservation(t, f, observation)
				deleted, err := f.RulerPass.PruneEndedSnapshots(context.Background(), time.Now().UTC().Add(-24*time.Hour))
				if err != nil || deleted != 1 {
					t.Fatalf("PruneEndedSnapshots() = (%d, %v), want one deleted snapshot", deleted, err)
				}
			},
			want: reservation.SourceUnattributed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStore(t)
			observation := f.NewObservation("source-"+strings.ReplaceAll(tc.name, " ", "-"), "finished")
			if tc.setup != nil {
				tc.setup(t, f, observation)
			}
			result, err := f.Store.ProcessRecord(context.Background(), observation)
			if err != nil {
				t.Fatalf("ProcessRecord: %v", err)
			}
			got := observedRecordings(t, f, observation)
			if result.RecordingID == nil || len(got) != 1 || got[0].Source != tc.want {
				t.Fatalf("recording source = %+v, result = %+v; want %q", got, result, tc.want)
			}
		})
	}
}

func runFailedEventSource(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	observation := f.NewObservation("failed-event-manual-source", "failed")
	seedRuleReservation(t, f, observation)
	if err := f.Reservations.PutIntent(context.Background(), observation.Site, observation.ProgramID, reservation.IntentRecord); err != nil {
		t.Fatalf("PutIntent: %v", err)
	}
	id, recorded, err := f.Store.RecordFailedRecording(context.Background(), store.FailedRecordingObservation{
		Site: observation.Site, ProgramID: observation.ProgramID, Snapshot: observation.Snapshot,
		Reason: []byte(`{"type":"tuner-unavailable"}`),
	})
	if err != nil || !recorded || id == 0 {
		t.Fatalf("RecordFailedRecording() = (%d, %t, %v), want recorded with an ID", id, recorded, err)
	}
	got := observedRecordings(t, f, observation)
	if len(got) != 1 || got[0].ID != id || got[0].Source != reservation.SourceManual {
		t.Fatalf("failed event recording = %+v, want source %q", got, reservation.SourceManual)
	}
}

func runFailedEventSnapshot(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	observation := f.NewObservation("failed-event-snapshot", "failed")
	description := "失敗イベントの番組説明"
	observation.Snapshot = store.RecordingSnapshot{
		NetworkID: 32736, ServiceID: 1024, EventID: 121,
		ServiceName: "NHK総合", ChannelType: "GR", Channel: "27", Title: "失敗イベントの番組",
		Description: &description, Extended: []byte(`{"出演者":"テスト花子"}`),
		Genres: []byte(`[{"level1":4,"level2":5,"un1":6,"un2":7}]`), IsFree: true,
		ProgramStartAt:    time.Date(2026, time.August, 9, 18, 20, 0, 0, time.UTC),
		ProgramDurationMs: 5_400_000,
	}
	f.SeedReservation(t, observation)
	id, recorded, err := f.Store.RecordFailedRecording(context.Background(), store.FailedRecordingObservation{
		Site: observation.Site, ProgramID: observation.ProgramID, Snapshot: observation.Snapshot,
		Reason: []byte(`{"type":"tuner-unavailable"}`),
	})
	if err != nil || !recorded || id == 0 {
		t.Fatalf("RecordFailedRecording() = (%d, %t, %v), want recorded with an ID", id, recorded, err)
	}
	got := observedRecordings(t, f, observation)
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("failed event recordings = %+v, want one recording with ID %d", got, id)
	}
	requireRecordingSnapshot(t, got[0].Snapshot, observation.Snapshot)
}

func runFailedScheduleSource(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	observation := f.NewObservation("failed-schedule-manual-source", "failed")
	seedRuleReservation(t, f, observation)
	if err := f.Reservations.PutIntent(context.Background(), observation.Site, observation.ProgramID, reservation.IntentRecord); err != nil {
		t.Fatalf("PutIntent: %v", err)
	}
	result, err := f.Store.ProcessFailedSchedule(context.Background(), store.FailedScheduleObservation{
		Site: observation.Site, ProgramID: observation.ProgramID, Snapshot: observation.Snapshot,
		Reason: []byte(`{"type":"start-recording-failed"}`),
	})
	if err != nil || result.RecordingID == nil {
		t.Fatalf("ProcessFailedSchedule() = (%+v, %v), want a recording", result, err)
	}
	got := observedRecordings(t, f, observation)
	if len(got) != 1 || got[0].ID != *result.RecordingID || got[0].Source != reservation.SourceManual {
		t.Fatalf("failed schedule recording = %+v, want source %q", got, reservation.SourceManual)
	}
}

func seedRuleReservation(t *testing.T, f RecordObservationFixture, observation store.RecordObservation) {
	t.Helper()
	f.SeedProgramSnapshot(t, observation)
	ctx := context.Background()
	rule, err := f.Rules.Create(ctx, store.RuleInput{Name: "record observation source", Sites: []string{observation.Site}})
	if err != nil {
		t.Fatalf("Rules.Create: %v", err)
	}
	revision, err := f.RulerPass.GetRulerInputRevision(ctx, observation.Site)
	if err != nil {
		t.Fatalf("GetRulerInputRevision: %v", err)
	}
	result, err := f.RulerPass.ApplyPass(ctx, observation.Site, store.RulerPassPlan{
		InputRevision: revision,
		Reservations:  []store.RulerReservationInput{{ProgramID: observation.ProgramID, RuleID: &rule.ID, Base: []byte(`{}`)}},
	})
	if err != nil || result.Created != 1 {
		t.Fatalf("ApplyPass(rule reservation) = (%+v, %v), want one created reservation", result, err)
	}
}

// int64Text は失敗メッセージ用に *int64 を数値で、nil を "nil" で表す。
func int64Text(p *int64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatInt(*p, 10)
}

func runIngestTarget(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	var eventID int32
	addRecord := func(site, recordID string, managed bool, contentLength *int64) *int64 {
		t.Helper()
		eventID++
		observation := f.NewObservation(recordID, "finished")
		observation.Site = site
		observation.ProgramID = int64(eventID) + 900_000
		observation.Snapshot.EventID = eventID
		observation.Managed = managed
		observation.ContentLength = contentLength
		result, err := f.Store.ProcessRecord(ctx, observation)
		if err != nil {
			t.Fatalf("ProcessRecord(%q): %v", recordID, err)
		}
		return result.RecordingID
	}

	wantLength := int64(1234)
	wantID := addRecord("contract-site", "target-sized", true, &wantLength)
	if wantID == nil {
		t.Fatal("ProcessRecord(target-sized) returned no recording ID")
	}
	got, err := f.IngestTarget.GetRecordSyncIngestTarget(ctx, "contract-site", "target-sized")
	if err != nil {
		t.Fatalf("GetRecordSyncIngestTarget(target-sized): %v", err)
	}
	if got.RecordingID == nil || *got.RecordingID != *wantID || got.ContentLength == nil || *got.ContentLength != wantLength {
		t.Fatalf("target-sized = recording %s, content length %s; want recording %d and content length %d", int64Text(got.RecordingID), int64Text(got.ContentLength), *wantID, wantLength)
	}

	gotID := addRecord("contract-site", "target-unknown-length", true, nil)
	if gotID == nil {
		t.Fatal("ProcessRecord(target-unknown-length) returned no recording ID")
	}
	got, err = f.IngestTarget.GetRecordSyncIngestTarget(ctx, "contract-site", "target-unknown-length")
	if err != nil {
		t.Fatalf("GetRecordSyncIngestTarget(target-unknown-length): %v", err)
	}
	if got.RecordingID == nil || *got.RecordingID != *gotID || got.ContentLength != nil {
		t.Fatalf("target-unknown-length = recording %s, content length %s; want recording %d and nil content length", int64Text(got.RecordingID), int64Text(got.ContentLength), *gotID)
	}

	firstSiteLength := int64(111)
	secondSiteLength := int64(222)
	firstSiteID := addRecord("contract-site", "same-record-id", true, &firstSiteLength)
	secondSiteID := addRecord("another-site", "same-record-id", true, &secondSiteLength)
	for _, tc := range []struct {
		site       string
		wantID     *int64
		wantLength int64
	}{
		{site: "contract-site", wantID: firstSiteID, wantLength: firstSiteLength},
		{site: "another-site", wantID: secondSiteID, wantLength: secondSiteLength},
	} {
		got, err := f.IngestTarget.GetRecordSyncIngestTarget(ctx, tc.site, "same-record-id")
		if err != nil {
			t.Fatalf("GetRecordSyncIngestTarget(%q, same-record-id): %v", tc.site, err)
		}
		if got.RecordingID == nil || tc.wantID == nil || *got.RecordingID != *tc.wantID || got.ContentLength == nil || *got.ContentLength != tc.wantLength {
			t.Fatalf("target for site %q = recording %s, content length %s; want recording %s and content length %d", tc.site, int64Text(got.RecordingID), int64Text(got.ContentLength), int64Text(tc.wantID), tc.wantLength)
		}
	}

	lengthWithoutRecording := int64(5678)
	if gotID := addRecord("contract-site", "target-unlinked", false, &lengthWithoutRecording); gotID != nil {
		t.Fatalf("ProcessRecord(target-unlinked) recording ID = %d; want nil", *gotID)
	}
	got, err = f.IngestTarget.GetRecordSyncIngestTarget(ctx, "contract-site", "target-unlinked")
	if err != nil {
		t.Fatalf("GetRecordSyncIngestTarget(target-unlinked): %v", err)
	}
	if got.RecordingID != nil || got.ContentLength == nil || *got.ContentLength != lengthWithoutRecording {
		t.Fatalf("target-unlinked = recording %s, content length %s; want nil recording ID and content length %d", int64Text(got.RecordingID), int64Text(got.ContentLength), lengthWithoutRecording)
	}

	if _, err := f.IngestTarget.GetRecordSyncIngestTarget(ctx, "contract-site", "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecordSyncIngestTarget(missing) error = %v; want ErrNotFound", err)
	}
}

func runRecordBacklog(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	var eventID int32
	addRecord := func(site, recordID, status string, managed bool, contentLength *int64, originalState string) {
		t.Helper()
		eventID++
		observation := f.NewObservation(recordID, status)
		observation.Site = site
		observation.ProgramID = int64(eventID) + 910_000
		observation.Snapshot.EventID = eventID
		observation.Managed = managed
		observation.ContentLength = contentLength
		result, err := f.Store.ProcessRecord(ctx, observation)
		if err != nil {
			t.Fatalf("ProcessRecord(%q): %v", recordID, err)
		}
		if originalState != "" {
			if result.RecordingID == nil {
				t.Fatalf("ProcessRecord(%q) returned no recording ID for original asset", recordID)
			}
			f.NewOriginalAsset(t, *result.RecordingID, originalState)
		}
	}

	lengthWithoutAsset := int64(100)
	lengthWithDeletedAsset := int64(200)
	lengthWithActiveAsset := int64(300)
	lengthWithDeletingAsset := int64(400)
	lengthOtherSite := int64(500)
	lengthUnfinished := int64(600)
	lengthUnlinked := int64(700)
	addRecord("contract-site", "backlog-no-asset", "finished", true, &lengthWithoutAsset, "")
	addRecord("contract-site", "backlog-deleted-original", "finished", true, &lengthWithDeletedAsset, "deleted")
	addRecord("contract-site", "backlog-active-original", "finished", true, &lengthWithActiveAsset, "active")
	addRecord("contract-site", "backlog-deleting-original", "finished", true, &lengthWithDeletingAsset, "deleting")
	addRecord("another-site", "backlog-other-site", "finished", true, &lengthOtherSite, "")
	addRecord("contract-site", "backlog-recording", "recording", true, &lengthUnfinished, "")
	addRecord("contract-site", "backlog-unlinked", "finished", false, &lengthUnlinked, "")
	addRecord("contract-site", "backlog-null-length", "finished", true, nil, "")

	got, err := f.Backlog.GetUningestedRecordBacklog(ctx, "contract-site")
	if err != nil {
		t.Fatalf("GetUningestedRecordBacklog: %v", err)
	}
	if got.Records != 3 || got.Bytes != lengthWithoutAsset+lengthWithDeletedAsset {
		t.Fatalf("backlog = %+v; want 3 records and %d bytes", got, lengthWithoutAsset+lengthWithDeletedAsset)
	}
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
	state := recordSyncState(t, f, observation.Site, observation.RecordID)
	if !state.Found || state.RecordingID == nil {
		t.Fatalf("record_sync after concurrent observations = %+v, want one linked row", state)
	}
	recordings := observedRecordings(t, f, observation)
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
	state := recordSyncState(t, f, duplicate.Site, duplicate.RecordID)
	if state.Found {
		t.Fatalf("record_sync for rejected observation = %+v, want transaction rollback", state)
	}
	if got := len(observedRecordings(t, f, first)); got != 1 {
		t.Fatalf("recordings after rejected observation = %d, want 1", got)
	}
}

func runSuccessSupersedesFailure(t *testing.T, newStore NewRecordObservationStore) {
	f := newStore(t)
	ctx := context.Background()
	failed := f.NewObservation("failed-try", "failed")
	failed.FailedReason = []byte(`{"type":"signal-loss"}`)
	failedResult, err := f.Store.ProcessRecord(ctx, failed)
	if err != nil {
		t.Fatalf("ProcessRecord(failed) = %v", err)
	}
	if failedResult.RecordingID == nil {
		t.Fatal("ProcessRecord(failed) returned no recording ID")
	}
	f.NewOriginalAsset(t, *failedResult.RecordingID, "active")
	success := failed
	success.RecordID = "successful-retry"
	success.Status = "finished"
	success.FailedReason = nil
	var successID int64
	for i := range 3 {
		result, err := f.Store.ProcessRecord(ctx, success)
		if err != nil {
			t.Fatalf("ProcessRecord(success, pass %d) = %v", i, err)
		}
		if result.RecordingID == nil || (successID != 0 && successID != *result.RecordingID) {
			t.Fatalf("ProcessRecord(success, pass %d) recording ID = %s, want stable non-zero ID %d", i, int64Text(result.RecordingID), successID)
		}
		successID = *result.RecordingID
	}
	got := observedRecordings(t, f, failed)
	if len(got) != 2 {
		t.Fatalf("recordings for retried event = %d, want failed history plus success", len(got))
	}
	var supersededFailed, liveFinished int
	for _, row := range got {
		if row.Status == "failed" && row.Superseded {
			if row.Deleted {
				t.Errorf("superseded failed recording %d is deleted, want the failed attempt retained", row.ID)
			}
			supersededFailed++
		}
		if row.Status == "finished" && !row.Superseded {
			liveFinished++
		}
	}
	if supersededFailed != 1 || liveFinished != 1 {
		t.Fatalf("recordings after retry = %+v, want one superseded failed row and one live finished row", got)
	}
	failedAssets := observedMediaAssets(t, f, *failedResult.RecordingID)
	if len(failedAssets) != 1 || failedAssets[0].RecordingID != *failedResult.RecordingID || failedAssets[0].State != "active" || failedAssets[0].Deleted {
		t.Fatalf("media assets for superseded failed recording = %+v, want one active, undeleted asset still owned by the failed row", failedAssets)
	}
	if assets := observedMediaAssets(t, f, successID); len(assets) != 0 {
		t.Fatalf("media assets for successful retry = %+v, want none", assets)
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
	got := observedRecordings(t, f, observation)
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
	if got := observedRecordings(t, f, observation); len(got) != 1 || got[0].QualityEventCount != 1 {
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
	got := observedRecordings(t, f, success)
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
	got := observedRecordings(t, f, observation)
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
	if got := observedRecordings(t, f, observation); len(got) != 0 {
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
	got := observedRecordings(t, f, observation)
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
	if !recordSyncState(t, f, live.Site, live.RecordID).Found {
		t.Fatal("record_sync of the live record was deleted, want it kept regardless of observed time")
	}
	if recordSyncState(t, f, gone.Site, gone.RecordID).Found {
		t.Fatal("record_sync of the record missing from the live set was kept, want it deleted")
	}

	// 空の集合は、その site に record が 1 件も無いことを表す。
	if _, err := f.Store.DeleteStaleRecordSyncs(ctx, live.Site, nil); err != nil {
		t.Fatalf("DeleteStaleRecordSyncs(empty) = %v", err)
	}
	if recordSyncState(t, f, live.Site, live.RecordID).Found {
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
		// 同じ観測を重ねても、毎回同じ判定を返す（recording 中は継続して投入を求める）。
		for pass := range 2 {
			result, err := f.Store.ProcessRecord(ctx, observation)
			if err != nil {
				t.Fatalf("ProcessRecord(%s, managed=%t, pass %d) = %v", tc.status, tc.managed, pass, err)
			}
			if result.EnqueueIngest != tc.wantIngest || result.IngestPriority != tc.wantPriority {
				t.Fatalf("ProcessRecord(%s, managed=%t, pass %d) ingest = (%t, %d), want (%t, %d)",
					tc.status, tc.managed, pass, result.EnqueueIngest, result.IngestPriority, tc.wantIngest, tc.wantPriority)
			}
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
	if state := recordSyncState(t, f, failed.Site, failed.RecordID); !state.Found || state.RecordingID != nil || state.Status != "failed" {
		t.Fatalf("record_sync of skipped failed record = %+v, want an observation without recording", state)
	}
	got := observedRecordings(t, f, success)
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
	got := observedRecordings(t, f, failed)
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
	got = observedRecordings(t, f, fresh)
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

func observedRecordings(t *testing.T, f RecordObservationFixture, observation store.RecordObservation) []ObservedRecording {
	t.Helper()
	document, err := f.Catalog.Export(context.Background())
	if err != nil {
		t.Fatalf("Catalog.Export while reading recordings: %v", err)
	}
	var got []ObservedRecording
	for _, recording := range document.Recordings {
		if recording.Site != observation.Site ||
			recording.NetworkID != observation.Snapshot.NetworkID ||
			recording.ServiceID != observation.Snapshot.ServiceID ||
			recording.EventID != observation.Snapshot.EventID ||
			!recording.ProgramStartAt.Equal(observation.Snapshot.ProgramStartAt) {
			continue
		}
		row := ObservedRecording{
			ID: recording.ID, Status: recording.Status, Source: recording.Source,
			Superseded: recording.SupersededAt != nil, Deleted: recording.DeletedAt != nil,
			Snapshot: store.RecordingSnapshot{
				NetworkID: recording.NetworkID, ServiceID: recording.ServiceID, EventID: recording.EventID,
				ServiceName: recording.ServiceName, ChannelType: recording.ChannelType, Channel: recording.Channel,
				Title: recording.Title, Description: recording.Description, Extended: recording.Extended,
				Genres: recording.Genres, IsFree: recording.IsFree,
				ProgramStartAt: recording.ProgramStartAt, ProgramDurationMs: recording.ProgramDurationMs,
			},
		}
		var events []struct {
			At    time.Time `json:"at"`
			Event string    `json:"event"`
		}
		if err := json.Unmarshal(recording.QualityEvents, &events); err != nil {
			t.Fatalf("decoding recording %d quality events: %v", recording.ID, err)
		}
		row.QualityEventCount = len(events)
		for _, event := range events {
			row.QualityEventNames = append(row.QualityEventNames, event.Event)
			row.QualityEventTimes = append(row.QualityEventTimes, event.At)
		}
		got = append(got, row)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].ID < got[j].ID })
	return got
}

func observedMediaAssets(t *testing.T, f RecordObservationFixture, recordingID int64) []ObservedMediaAsset {
	t.Helper()
	document, err := f.Catalog.Export(context.Background())
	if err != nil {
		t.Fatalf("Catalog.Export while reading media assets: %v", err)
	}
	var got []ObservedMediaAsset
	for _, asset := range document.MediaAssets {
		if asset.RecordingID == recordingID {
			got = append(got, ObservedMediaAsset{
				RecordingID: asset.RecordingID, State: asset.State, Deleted: asset.DeletedAt != nil,
			})
		}
	}
	return got
}

func recordSyncState(t *testing.T, f RecordObservationFixture, site, recordID string) RecordSyncState {
	t.Helper()
	target, err := f.IngestTarget.GetRecordSyncIngestTarget(context.Background(), site, recordID)
	if errors.Is(err, store.ErrNotFound) {
		return RecordSyncState{}
	}
	if err != nil {
		t.Fatalf("GetRecordSyncIngestTarget(%q, %q): %v", site, recordID, err)
	}
	return RecordSyncState{
		Found: true, RecordingID: target.RecordingID,
		Status: f.RecordSyncStatus(t, site, recordID),
	}
}

func requireStore(t *testing.T, f RecordObservationFixture) {
	t.Helper()
	if f.Store == nil || f.Catalog == nil || f.Reservations == nil || f.RulerPass == nil || f.Rules == nil || f.IngestTarget == nil || f.Backlog == nil || f.NewObservation == nil || f.SeedProgramSnapshot == nil || f.SeedRecordSync == nil || f.SeedReservation == nil || f.NewOriginalAsset == nil || f.RecordSyncStatus == nil || f.SetRecordSyncObservedAt == nil || f.SetAfterRecordAcquired == nil {
		t.Fatalf("incomplete record observation fixture: %+v", f)
	}
}
