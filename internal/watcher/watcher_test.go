package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/jobq"
	jobqtestutil "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/mirakc/mirakctest"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func newWatcherMirakcFake(t *testing.T, services []mirakc.Service, schedules []mirakc.Schedule, records []mirakc.Record) *mirakctest.Fake {
	t.Helper()
	fake := mirakctest.New(t)
	fake.SetServices(services)
	fake.SetSchedules(schedules)
	fake.SetRecords(records)
	return fake
}

func setupTest(t *testing.T) (*Watcher, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.SetupDB(t)

	enqueuer := jobqtestutil.NewRecordingEnqueuer()
	mc := mirakc.NewClient("http://unused:40772", nil)
	w := New(DefaultSite, mc, pgstoretest.Recordings(pool), pgstoretest.Reservations(pool), enqueuer, nil)
	return w, pool
}

// failingRecords wraps a real store and can inject one method error while recording calls.
type failingRecords struct {
	store.RecordObservations
	failRecordID                string
	failFailedScheduleProgramID int64
	mu                          sync.Mutex
	calls                       []string
	liveRecordIDs               []string
	observations                []store.RecordObservation
	results                     []store.ProcessResult
	failedScheduleObservations  []store.FailedScheduleObservation
	failedEventObservations     []store.FailedRecordingObservation
	brokenEvents                []brokenEventCall
}

type brokenEventCall struct {
	site, recordID, reason string
}

func (r *failingRecords) ProcessRecord(ctx context.Context, observation store.RecordObservation) (store.ProcessResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "record:"+observation.RecordID)
	r.observations = append(r.observations, observation)
	r.mu.Unlock()
	if observation.RecordID == r.failRecordID {
		return store.ProcessResult{}, errors.New("injected record failure")
	}
	result, err := r.RecordObservations.ProcessRecord(ctx, observation)
	r.mu.Lock()
	r.results = append(r.results, result)
	r.mu.Unlock()
	return result, err
}

func (r *failingRecords) ProcessFailedSchedule(ctx context.Context, observation store.FailedScheduleObservation) (store.ProcessResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, fmt.Sprintf("failed-schedule:%d", observation.ProgramID))
	r.failedScheduleObservations = append(r.failedScheduleObservations, observation)
	r.mu.Unlock()
	if observation.ProgramID == r.failFailedScheduleProgramID {
		return store.ProcessResult{}, errors.New("injected failed schedule error")
	}
	return r.RecordObservations.ProcessFailedSchedule(ctx, observation)
}

func (r *failingRecords) DeleteStaleRecordSyncs(ctx context.Context, site string, liveRecordIDs []string) (int64, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "delete-stale")
	r.liveRecordIDs = append([]string(nil), liveRecordIDs...)
	r.mu.Unlock()
	return r.RecordObservations.DeleteStaleRecordSyncs(ctx, site, liveRecordIDs)
}

func (r *failingRecords) RecordFailedRecording(ctx context.Context, observation store.FailedRecordingObservation) (int64, bool, error) {
	r.mu.Lock()
	r.failedEventObservations = append(r.failedEventObservations, observation)
	r.mu.Unlock()
	return r.RecordObservations.RecordFailedRecording(ctx, observation)
}

func (r *failingRecords) AppendRecordBrokenEvent(ctx context.Context, site, recordID, reason string) (bool, error) {
	r.mu.Lock()
	r.brokenEvents = append(r.brokenEvents, brokenEventCall{site: site, recordID: recordID, reason: reason})
	r.mu.Unlock()
	return r.RecordObservations.AppendRecordBrokenEvent(ctx, site, recordID, reason)
}

// recordsSnapshot is a copy of the calls a failingRecords has seen.
type recordsSnapshot struct {
	calls           []string
	liveRecordIDs   []string
	observations    []store.RecordObservation
	results         []store.ProcessResult
	failedSchedules []store.FailedScheduleObservation
	failedEvents    []store.FailedRecordingObservation
	brokenEvents    []brokenEventCall
}

func (r *failingRecords) snapshot() recordsSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return recordsSnapshot{
		calls:           append([]string(nil), r.calls...),
		liveRecordIDs:   append([]string(nil), r.liveRecordIDs...),
		observations:    append([]store.RecordObservation(nil), r.observations...),
		results:         append([]store.ProcessResult(nil), r.results...),
		failedSchedules: append([]store.FailedScheduleObservation(nil), r.failedScheduleObservations...),
		failedEvents:    append([]store.FailedRecordingObservation(nil), r.failedEventObservations...),
		brokenEvents:    append([]brokenEventCall(nil), r.brokenEvents...),
	}
}

func seedRecordSweepMarker(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	ctx := context.Background()
	markerAt := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO record_sweep_snapshots (site, last_success_at)
		VALUES ($1, $2)`, DefaultSite, markerAt,
	); err != nil {
		t.Fatalf("seeding record sweep marker: %v", err)
	}
	return markerAt
}

func requireRecordSweepMarker(t *testing.T, pool *pgxpool.Pool, want time.Time) {
	t.Helper()
	var got time.Time
	if err := pool.QueryRow(context.Background(), `
		SELECT last_success_at FROM record_sweep_snapshots WHERE site = $1`, DefaultSite,
	).Scan(&got); err != nil {
		t.Fatalf("reading record sweep marker: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("record sweep marker = %s, want unchanged %s", got, want)
	}
}

func requireRecordingSnapshotEqual(t *testing.T, got, want store.RecordingSnapshot) {
	t.Helper()
	want.ProgramStartAt = want.ProgramStartAt.Truncate(time.Millisecond)
	if !got.ProgramStartAt.Equal(want.ProgramStartAt) {
		t.Errorf("recording snapshot start time = %s, want %s", got.ProgramStartAt, want.ProgramStartAt)
	}
	got.ProgramStartAt = time.Time{}
	want.ProgramStartAt = time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recording snapshot = %#v, want %#v", got, want)
	}
}

func listIngestEnqueues(t *testing.T, watcher *Watcher) []jobqtestutil.EnqueueCall {
	t.Helper()
	enqueuer, ok := watcher.enqueuer.(*jobqtestutil.RecordingEnqueuer)
	if !ok {
		t.Fatalf("watcher enqueuer = %T, want *RecordingEnqueuer", watcher.enqueuer)
	}
	return enqueuer.CallsOfKind(jobs.IngestJobArgs{}.Kind())
}

func seedSweepLastPassMetric(t *testing.T) float64 {
	t.Helper()
	previous := promtestutil.ToFloat64(metrics.SweepLastPass)
	const sentinel = 12345
	metrics.SweepLastPass.Set(sentinel)
	t.Cleanup(func() { metrics.SweepLastPass.Set(previous) })
	return sentinel
}

func requireSweepLastPassMetric(t *testing.T, want float64) {
	t.Helper()
	if got := promtestutil.ToFloat64(metrics.SweepLastPass); got != want {
		t.Errorf("SweepLastPass = %v, want unchanged %v", got, want)
	}
}
func insertTestProgramSnapshot(t *testing.T, pool *pgxpool.Pool, programID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO program_snapshots (
			site, program_id, title, start_at, duration_ms,
			network_id, service_id, channel_type, channel, event_id, service_name
		)
		VALUES ('default', $1, 'Test Program', now(), 3600000, 32736, 1024, 'GR', '27', 1, 'テスト局')
		ON CONFLICT (site, program_id) DO NOTHING`, programID,
	); err != nil {
		t.Fatalf("creating program_snapshot fixture: %v", err)
	}
}

// createTestReservation は watcher の処理経路で予約が必要なテストの状態を作る。
func createTestReservation(t *testing.T, pool *pgxpool.Pool, programID int64) {
	t.Helper()
	insertTestProgramSnapshot(t, pool, programID)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO reservations (site, program_id)
		VALUES ('default', $1)`, programID,
	); err != nil {
		t.Fatalf("creating reservation: %v", err)
	}
}
func testRecord(recordID string, programID int64, status string) mirakc.Record {
	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	recStart := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name := "Test Program"

	r := mirakc.Record{
		ID: recordID,
		Program: mirakc.Program{
			ID:        programID,
			EventID:   100,
			ServiceID: 1024,
			NetworkID: 32736,
			StartAt:   &startAt,
			Duration:  &duration,
			IsFree:    true,
			Name:      &name,
		},
		Service: mirakc.Service{
			Name:    "NHK総合",
			Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"},
		},
		Tags: []string{mirakc.ProgramTag(programID)},
		Recording: mirakc.RecordInfo{
			Status:    status,
			StartTime: recStart,
		},
		Content: mirakc.ContentInfo{
			Path: "videos/test.m2ts",
			Type: "video/mp2t",
		},
	}

	if status == "finished" {
		endTime := mirakc.Milliseconds(time.Now())
		r.Recording.EndTime = &endTime
	}
	return r
}
func TestProcessRecord_UntaggedRecord(t *testing.T) {
	w, _ := setupTest(t)
	records := &failingRecords{RecordObservations: w.records}
	w.records = records

	record := testRecord("record-notag-001", 100004, "finished")
	record.Tags = nil
	if err := w.processRecord(context.Background(), record); err != nil {
		t.Fatalf("processRecord: %v", err)
	}

	observations := records.snapshot().observations
	if len(observations) != 1 {
		t.Fatalf("ProcessRecord calls = %d, want 1", len(observations))
	}
	if observations[0].Managed {
		t.Errorf("record observation Managed = true for record without Rokuban tags")
	}
	if len(observations[0].Tags) != 0 {
		t.Errorf("record observation Tags = %v, want no tags", observations[0].Tags)
	}
	if enqueues := listIngestEnqueues(t, w); len(enqueues) != 0 {
		t.Errorf("ingest enqueue calls for untagged record = %d, want 0", len(enqueues))
	}
}

// TestProcessRecord_EnqueuesFollowResult は、store の結果が ingest の投入を求めるときだけ
// 投入し、結果の優先度が非 0 ならそれを、0 なら job の既定値を使うことを検証する。
// どの status が何を求めるかは store の契約（ingest hint follows the recording status）が持つので、
// 期待値は store が返した結果から作る。
func TestProcessRecord_EnqueuesFollowResult(t *testing.T) {
	var sawPriority, sawDefault, sawNone bool
	var ran int
	for _, status := range []string{store.RecordingStatusRecording, store.RecordingStatusFinished, store.RecordingStatusCanceled} {
		t.Run(status, func(t *testing.T) {
			ran++
			w, pool := setupTest(t)
			records := &failingRecords{RecordObservations: w.records}
			w.records = records
			const programID int64 = 100010
			createTestReservation(t, pool, programID)
			record := testRecord("ingest-hint-"+status, programID, status)
			if err := w.processRecord(context.Background(), record); err != nil {
				t.Fatalf("processRecord: %v", err)
			}
			results := records.snapshot().results
			if len(results) != 1 {
				t.Fatalf("store results = %d, want 1", len(results))
			}
			result := results[0]
			enqueues := listIngestEnqueues(t, w)
			if !result.EnqueueIngest {
				sawNone = true
				if len(enqueues) != 0 {
					t.Fatalf("ingest enqueue calls = %d, want 0 when the store does not ask for ingest", len(enqueues))
				}
				return
			}
			if len(enqueues) != 1 {
				t.Fatalf("ingest enqueue calls = %d, want 1 when the store asks for ingest", len(enqueues))
			}
			args, ok := enqueues[0].Args.(jobs.IngestJobArgs)
			if !ok {
				t.Fatalf("ingest args = %T, want jobs.IngestJobArgs", enqueues[0].Args)
			}
			if args.Site != DefaultSite || args.RecordID != record.ID {
				t.Errorf("ingest args = %+v, want site %q and record %q", args, DefaultSite, record.ID)
			}
			want := jobq.EffectivePriority(args.Spec())
			if result.IngestPriority != 0 {
				sawPriority = true
				want = jobq.EffectivePriority(args.Spec(), jobq.Priority(result.IngestPriority))
			} else {
				sawDefault = true
			}
			if got := jobq.EffectivePriority(args.Spec(), enqueues[0].Options...); got != want {
				t.Errorf("ingest priority = %d, want %d (store priority %d)", got, want, result.IngestPriority)
			}
		})
	}
	// 3 通り（優先度あり・既定・投入なし）を実際に通ったことを確かめる。空振りを防ぐ。
	// -run で一部のサブテストだけ実行したときは評価しない。
	if ran == 3 && (!sawPriority || !sawDefault || !sawNone) {
		t.Fatalf("cases covered: priority=%t default=%t none=%t, want all three", sawPriority, sawDefault, sawNone)
	}
}

func TestHandleRecordBroken(t *testing.T) {
	w, _ := setupTest(t)
	records := &failingRecords{RecordObservations: w.records}
	w.records = records

	if err := w.handleRecordBroken(context.Background(), mirakc.RecordBrokenData{RecordID: "record-broken-001", Reason: "io-error"}); err != nil {
		t.Fatalf("handleRecordBroken: %v", err)
	}

	got := records.snapshot().brokenEvents
	if !reflect.DeepEqual(got, []brokenEventCall{{site: DefaultSite, recordID: "record-broken-001", reason: "io-error"}}) {
		t.Errorf("AppendRecordBrokenEvent calls = %+v, want site, record ID, and reason from mirakc event", got)
	}
}
func TestHandleRecordingFailed_ConvertsScheduleObservation(t *testing.T) {
	w, pool := setupTest(t)
	records := &failingRecords{RecordObservations: w.records}
	w.records = records

	programID := int64(327361024100)
	createTestReservation(t, pool, programID)
	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour).Truncate(time.Millisecond))
	duration := int64(3600000)
	name := "Failed Program"
	description := "失敗番組の説明"
	schedule := mirakc.Schedule{
		State: "scheduled",
		Program: mirakc.Program{
			ID: programID, EventID: 100, ServiceID: 1024, NetworkID: 32736,
			StartAt: &startAt, Duration: &duration, IsFree: true, Name: &name,
			Description: &description, Extended: map[string]string{"出演者": "テスト太郎"},
			Genres: []mirakc.Genre{{LV1: 1, LV2: 2, UN1: 3, UN2: 4}},
		},
	}
	services := []mirakc.Service{{ServiceID: 1024, NetworkID: 32736, Name: "NHK総合", Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"}}}
	fake := newWatcherMirakcFake(t, services, []mirakc.Schedule{schedule}, nil)
	w.mirakc = mirakc.NewClient(fake.URL(), nil)

	reason := mirakc.FailedReason{Type: "tuner-unavailable"}
	if err := w.handleRecordingFailed(context.Background(), mirakc.RecordingFailedData{ProgramID: programID, Reason: reason}); err != nil {
		t.Fatalf("handleRecordingFailed: %v", err)
	}

	got := records.snapshot().failedEvents
	if len(got) != 1 {
		t.Fatalf("RecordFailedRecording calls = %d, want 1", len(got))
	}
	wantReason, err := json.Marshal(reason)
	if err != nil {
		t.Fatalf("marshalling expected reason: %v", err)
	}
	wantSnapshot := snapshotFromSchedule(schedule, services[0]).recordingSnapshot(
		schedule.Program.NetworkID, schedule.Program.ServiceID, schedule.Program.EventID)
	if got[0].Site != DefaultSite || got[0].ProgramID != programID || !bytes.Equal(got[0].Reason, wantReason) {
		t.Errorf("RecordFailedRecording observation = %+v, want watcher conversion of schedule and event reason", got[0])
	}
	requireRecordingSnapshotEqual(t, got[0].Snapshot, wantSnapshot)
}
func TestHandleRecordingFailed_NoReservationSkipsScheduleLookup(t *testing.T) {
	w, _ := setupTest(t)
	recordsStore := &failingRecords{RecordObservations: w.records}
	w.records = recordsStore
	fake := newWatcherMirakcFake(t, nil, nil, nil)
	w.mirakc = mirakc.NewClient(fake.URL(), nil)
	const programID = int64(200099)

	if err := w.handleRecordingFailed(context.Background(), mirakc.RecordingFailedData{
		ProgramID: programID, Reason: mirakc.FailedReason{Type: "tuner-unavailable"},
	}); err != nil {
		t.Fatalf("handleRecordingFailed without reservation: %v", err)
	}
	if calls := fake.CallCount(http.MethodGet, fmt.Sprintf("/api/recording/schedules/%d", programID)); calls != 0 {
		t.Errorf("GetSchedule calls without reservation = %d, want 0", calls)
	}
	failedEvents := recordsStore.snapshot().failedEvents
	if len(failedEvents) != 0 {
		t.Errorf("RecordFailedRecording calls without reservation = %d, want 0", len(failedEvents))
	}
}

func TestSweep_FailedScheduleWithoutReservationSkipsRecording(t *testing.T) {
	w, _ := setupTest(t)
	recordsStore := &failingRecords{RecordObservations: w.records}
	w.records = recordsStore
	reason := mirakc.FailedReason{Type: "start-recording-failed"}
	schedule := mirakc.Schedule{
		State: store.RecordingStatusFailed, FailedReason: &reason,
		Program: mirakc.Program{ID: 200098, NetworkID: 32736, ServiceID: 1024, EventID: 98},
	}
	services := []mirakc.Service{{ServiceID: schedule.Program.ServiceID, NetworkID: schedule.Program.NetworkID, Name: "NHK総合"}}
	fake := newWatcherMirakcFake(t, services, []mirakc.Schedule{schedule}, nil)
	w.mirakc = mirakc.NewClient(fake.URL(), nil)

	if err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep without reservation: %v", err)
	}
	snap := recordsStore.snapshot()
	calls := snap.calls
	failedSchedules := snap.failedSchedules
	if !reflect.DeepEqual(calls, []string{"delete-stale"}) || len(failedSchedules) != 0 {
		t.Errorf("store calls = %v, failed schedule observations = %d; want no failure write", calls, len(failedSchedules))
	}
	if calls := fake.CallCount(http.MethodGet, "/api/services"); calls != 1 {
		t.Errorf("ListServices calls = %d, want only the sweep cache refresh", calls)
	}
}

func TestSweep_CatchesMissedRecords(t *testing.T) {
	w, pool := setupTest(t)
	recordsStore := &failingRecords{RecordObservations: w.records}
	w.records = recordsStore
	ctx := context.Background()

	records := []mirakc.Record{
		testRecord("reconcile-rec-001", 200001, "finished"),
		testRecord("reconcile-rec-002", 200002, "finished"),
	}
	for i := range records {
		records[i].Program.EventID = i + 1
	}
	fake := newWatcherMirakcFake(t, nil, nil, records)
	w.mirakc = mirakc.NewClient(fake.URL(), nil)
	markerBefore := seedRecordSweepMarker(t, pool)
	lastPass := seedSweepLastPassMetric(t)

	if err := w.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	snap := recordsStore.snapshot()
	calls := snap.calls
	liveIDs := snap.liveRecordIDs
	wantCalls := []string{"delete-stale", "record:" + records[0].ID, "record:" + records[1].ID}
	wantIDs := []string{records[0].ID, records[1].ID}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("Sweep store call order = %v, want %v", calls, wantCalls)
	}
	if !reflect.DeepEqual(liveIDs, wantIDs) {
		t.Errorf("DeleteStaleRecordSyncs live IDs = %v, want %v", liveIDs, wantIDs)
	}
	var markerAt time.Time
	if err := pool.QueryRow(ctx, `SELECT last_success_at FROM record_sweep_snapshots WHERE site = $1`, DefaultSite).Scan(&markerAt); err != nil {
		t.Fatalf("reading successful sweep marker: %v", err)
	}
	if !markerAt.After(markerBefore) {
		t.Errorf("record sweep marker = %s, want a time after %s", markerAt, markerBefore)
	}
	if got := promtestutil.ToFloat64(metrics.SweepLastPass); got <= lastPass {
		t.Errorf("SweepLastPass = %v, want a value after %v", got, lastPass)
	}
}
func TestSweep_PartialRecordFailureDoesNotAdvanceMarker(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()
	const failingRecordID = "sweep-fail-record"
	recordsStore := &failingRecords{RecordObservations: w.records, failRecordID: failingRecordID}
	w.records = recordsStore

	records := []mirakc.Record{
		testRecord("sweep-good-before-failure", 200010, store.RecordingStatusFinished),
		testRecord(failingRecordID, 200011, store.RecordingStatusFinished),
		testRecord("sweep-good-after-failure", 200012, store.RecordingStatusFinished),
	}
	for i := range records {
		records[i].Program.EventID = i + 10
	}
	fake := newWatcherMirakcFake(t, nil, nil, records)
	w.mirakc = mirakc.NewClient(fake.URL(), nil)
	markerAt := seedRecordSweepMarker(t, pool)
	lastPass := seedSweepLastPassMetric(t)

	err := w.Sweep(ctx)
	if err == nil || !strings.Contains(err.Error(), failingRecordID) {
		t.Fatalf("Sweep error = %v, want failure identifying %q", err, failingRecordID)
	}
	snap := recordsStore.snapshot()
	calls := snap.calls
	liveIDs := snap.liveRecordIDs
	wantCalls := []string{"delete-stale", "record:" + records[0].ID, "record:" + records[1].ID, "record:" + records[2].ID}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("Sweep store call order = %v, want %v", calls, wantCalls)
	}
	wantIDs := []string{records[0].ID, records[1].ID, records[2].ID}
	if !reflect.DeepEqual(liveIDs, wantIDs) {
		t.Errorf("DeleteStaleRecordSyncs live IDs = %v, want %v", liveIDs, wantIDs)
	}
	requireRecordSweepMarker(t, pool, markerAt)
	requireSweepLastPassMetric(t, lastPass)
}

func TestSweep_ListSchedulesFailureDoesNotAdvanceMarker(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()
	record := testRecord("sweep-after-schedule-list-failure", 200013, store.RecordingStatusFinished)
	recordsStore := &failingRecords{RecordObservations: w.records}
	w.records = recordsStore
	fake := newWatcherMirakcFake(t, nil, nil, []mirakc.Record{record})
	fake.FailNext(http.MethodGet, "/api/recording/schedules", 1, http.StatusInternalServerError)
	w.mirakc = mirakc.NewClient(fake.URL(), nil)
	markerAt := seedRecordSweepMarker(t, pool)
	lastPass := seedSweepLastPassMetric(t)

	err := w.Sweep(ctx)
	if err == nil || !strings.Contains(err.Error(), "listing schedules") {
		t.Fatalf("Sweep error = %v, want listing-schedules failure", err)
	}
	calls := recordsStore.snapshot().calls
	if want := []string{"delete-stale", "record:" + record.ID}; !reflect.DeepEqual(calls, want) {
		t.Errorf("store calls after ListSchedules failure = %v, want %v", calls, want)
	}
	if calls := fake.CallCount(http.MethodGet, "/api/recording/records"); calls != 1 {
		t.Errorf("ListRecords calls = %d, want 1 after schedule-list failure", calls)
	}
	requireRecordSweepMarker(t, pool, markerAt)
	requireSweepLastPassMetric(t, lastPass)
}

func TestSweep_FailedScheduleProcessingDoesNotAdvanceMarker(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()
	const failedProgramID = int64(200014)
	createTestReservation(t, pool, failedProgramID)
	const laterProgramID = int64(200015)
	failedProgram := mirakc.Program{ID: failedProgramID, EventID: 42, ServiceID: 1024, NetworkID: 32736}
	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name := "Failed schedule"
	failedProgram.StartAt, failedProgram.Duration, failedProgram.Name = &startAt, &duration, &name
	reason := mirakc.FailedReason{Type: "start-recording-failed"}
	schedule := mirakc.Schedule{State: store.RecordingStatusFailed, FailedReason: &reason, Program: failedProgram}
	record := testRecord("sweep-record-after-failed-schedule", laterProgramID, store.RecordingStatusFinished)
	record.Program.EventID = 43
	recordsStore := &failingRecords{RecordObservations: w.records, failFailedScheduleProgramID: failedProgramID}
	w.records = recordsStore
	services := []mirakc.Service{{ServiceID: 1024, NetworkID: 32736, Name: "NHK総合", Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"}}}
	fake := newWatcherMirakcFake(t, services, []mirakc.Schedule{schedule}, []mirakc.Record{record})
	w.mirakc = mirakc.NewClient(fake.URL(), nil)
	markerAt := seedRecordSweepMarker(t, pool)
	lastPass := seedSweepLastPassMetric(t)

	err := w.Sweep(ctx)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("program_id %d", failedProgramID)) {
		t.Fatalf("Sweep error = %v, want failed schedule program %d", err, failedProgramID)
	}
	calls := recordsStore.snapshot().calls
	wantCalls := []string{fmt.Sprintf("failed-schedule:%d", failedProgramID), "delete-stale", "record:" + record.ID}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("Sweep store call order = %v, want %v", calls, wantCalls)
	}
	requireRecordSweepMarker(t, pool, markerAt)
	requireSweepLastPassMetric(t, lastPass)
}

func TestSweep_ConvertsFailedSchedule(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()
	const programID = int64(200003)
	createTestReservation(t, pool, programID)
	startAt := mirakc.Milliseconds(time.Now().Add(-1 * time.Hour))
	duration := int64(3600000)
	name := "Recordless Failed Program"
	message := "failed to start recording"
	reason := mirakc.FailedReason{Type: "start-recording-failed", Message: &message}
	schedule := mirakc.Schedule{
		State:   store.RecordingStatusFailed,
		Program: mirakc.Program{ID: programID, EventID: 3, ServiceID: 1024, NetworkID: 32736, StartAt: &startAt, Duration: &duration, IsFree: true, Name: &name},
		Tags:    []string{mirakc.ProgramTag(programID)}, FailedReason: &reason,
	}
	services := []mirakc.Service{{ServiceID: 1024, NetworkID: 32736, Name: "NHK総合", Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"}}}
	recordsStore := &failingRecords{RecordObservations: w.records}
	w.records = recordsStore
	fake := newWatcherMirakcFake(t, services, []mirakc.Schedule{schedule}, nil)
	w.mirakc = mirakc.NewClient(fake.URL(), nil)

	if err := w.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	snap := recordsStore.snapshot()
	calls := snap.calls
	got := snap.failedSchedules
	if len(got) != 1 {
		t.Fatalf("ProcessFailedSchedule calls = %d, want 1", len(got))
	}
	wantReason, err := json.Marshal(reason)
	if err != nil {
		t.Fatalf("marshalling expected reason: %v", err)
	}
	wantSnapshot := snapshotFromSchedule(schedule, services[0]).recordingSnapshot(
		schedule.Program.NetworkID, schedule.Program.ServiceID, schedule.Program.EventID)
	if got[0].Site != DefaultSite || got[0].ProgramID != programID || !bytes.Equal(got[0].Reason, wantReason) {
		t.Errorf("ProcessFailedSchedule observation = %+v, want watcher conversion of failed schedule", got[0])
	}
	requireRecordingSnapshotEqual(t, got[0].Snapshot, wantSnapshot)
	if want := []string{fmt.Sprintf("failed-schedule:%d", programID), "delete-stale"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("Sweep store calls = %v, want %v", calls, want)
	}
}

func TestSweep_ConvertsFailedRecord(t *testing.T) {
	w, _ := setupTest(t)
	ctx := context.Background()
	record := testRecord("reconcile-failed-record-001", 200004, store.RecordingStatusFailed)
	message := "pipeline exited with an error"
	reason := &mirakc.FailedReason{Type: "pipeline-error", Message: &message}
	record.Recording.FailedReason = reason
	recordsStore := &failingRecords{RecordObservations: w.records}
	w.records = recordsStore
	fake := newWatcherMirakcFake(t, nil, nil, []mirakc.Record{record})
	w.mirakc = mirakc.NewClient(fake.URL(), nil)

	if err := w.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	observations := recordsStore.snapshot().observations
	if len(observations) != 1 {
		t.Fatalf("ProcessRecord calls = %d, want 1", len(observations))
	}
	wantReason, err := json.Marshal(reason)
	if err != nil {
		t.Fatalf("marshalling expected failed reason: %v", err)
	}
	got := observations[0]
	if got.Site != DefaultSite || got.RecordID != record.ID || got.ProgramID != record.Program.ID || got.Status != store.RecordingStatusFailed || !bytes.Equal(got.FailedReason, wantReason) {
		t.Errorf("record observation = %+v, want failed record fields and reason from mirakc", got)
	}
}

func TestSweep_UsesLiveRecordIDsForStaleCleanup(t *testing.T) {
	w, _ := setupTest(t)
	recordsStore := &failingRecords{RecordObservations: w.records}
	w.records = recordsStore
	current := testRecord("current-external", 903, store.RecordingStatusFinished)
	current.Tags = nil
	fake := newWatcherMirakcFake(t, nil, nil, []mirakc.Record{current})
	w.mirakc = mirakc.NewClient(fake.URL(), nil)

	if err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	snap := recordsStore.snapshot()
	calls := snap.calls
	liveIDs := snap.liveRecordIDs
	if !reflect.DeepEqual(liveIDs, []string{current.ID}) {
		t.Errorf("DeleteStaleRecordSyncs live IDs = %v, want only current mirakc record %q", liveIDs, current.ID)
	}
	if want := []string{"delete-stale", "record:" + current.ID}; !reflect.DeepEqual(calls, want) {
		t.Errorf("Sweep store call order = %v, want %v", calls, want)
	}
}

// TestSweep_SSERecordCommittedMidLoopSurvives は、stale 削除を snapshot の record 処理より
// 前に行い、その間に届いた SSE 観測を巻き込まない順序を固定する。
func TestSweep_SSERecordCommittedMidLoopSurvives(t *testing.T) {
	w, _ := setupTest(t)
	ctx := context.Background()
	recordsStore := &failingRecords{RecordObservations: w.records}
	w.records = recordsStore

	injected := testRecord("sse-mid-loop", 700001, "finished")
	anchor := testRecord("anchor", 700002, "finished")
	anchor.Tags = nil
	fake := newWatcherMirakcFake(t, nil, nil, []mirakc.Record{anchor})
	w.mirakc = mirakc.NewClient(fake.URL(), nil)

	beforeSweepProcessRecords = func() {
		if err := w.processRecord(ctx, injected); err != nil {
			t.Errorf("injecting SSE observation: %v", err)
		}
	}
	t.Cleanup(func() { beforeSweepProcessRecords = nil })

	if err := w.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	snap := recordsStore.snapshot()
	calls := snap.calls
	liveIDs := snap.liveRecordIDs
	if !reflect.DeepEqual(liveIDs, []string{anchor.ID}) {
		t.Errorf("DeleteStaleRecordSyncs live IDs = %v, want snapshot %q", liveIDs, anchor.ID)
	}
	wantCalls := []string{"delete-stale", "record:" + injected.ID, "record:" + anchor.ID}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("Sweep store call order = %v, want %v", calls, wantCalls)
	}
}

func TestRun_NoAutomaticSweep(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	enqueuer := jobqtestutil.NewRecordingEnqueuer()

	var eventsCalls int32
	fake := mirakctest.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(rw http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&eventsCalls, 1)
		rw.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := rw.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter is not Flusher")
		}
		flusher.Flush()
		<-r.Context().Done()
	})
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			mux.ServeHTTP(rw, r)
			return
		}
		fake.ServeHTTP(rw, r)
	}))
	t.Cleanup(server.Close)

	mc := mirakc.NewClient(server.URL, nil)
	w := New(DefaultSite, mc, pgstoretest.Recordings(pool), pgstoretest.Reservations(pool), enqueuer, nil)

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()

	// SSE 接続が確立するまで少し待ってから、Run が自発的に全量突き合わせを
	// 呼んでいないことを確認する。
	deadline := time.After(500 * time.Millisecond)
	for atomic.LoadInt32(&eventsCalls) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for SSE connection")
		case <-time.After(10 * time.Millisecond):
		}
	}

	runCancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Watcher.Run() after cancellation = %v, want context.Canceled", err)
	}

	recordsCalls := fake.CallCount(http.MethodGet, "/api/recording/records")
	if recordsCalls != 0 {
		t.Errorf("GET /api/recording/records call count = %d, want 0 "+
			"(Watcher.Run は SSE 購読だけの常駐になり、全量突き合わせは record_sweep ジョブの仕事のはず)", recordsCalls)
	}
}

type commitCheckingEnqueuer struct {
	target           store.IngestTarget
	calls            int
	recordingVisible bool
	err              error
}

func (e *commitCheckingEnqueuer) Enqueue(ctx context.Context, args jobq.Args, opts ...jobq.EnqueueOpt) (jobq.Result, error) {
	e.calls++
	ingestArgs, ok := args.(jobs.IngestJobArgs)
	if !ok {
		return jobq.Result{}, fmt.Errorf("enqueue args = %T, want jobs.IngestJobArgs", args)
	}
	// 別の接続から読む。commit 前なら record_sync の行が見えない。
	target, err := e.target.GetRecordSyncIngestTarget(ctx, ingestArgs.Site, ingestArgs.RecordID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return jobq.Result{}, fmt.Errorf("checking committed recording during enqueue: %w", err)
	}
	e.recordingVisible = err == nil && target.RecordingID != nil
	return jobq.Result{}, e.err
}

// TestProcessRecord_EnqueueFollowsCommit は commit 後の投入と、store が失敗した場合に
// ingest のヒントを出さないことを検証する。
func TestProcessRecord_EnqueueFollowsCommit(t *testing.T) {
	w, pool := setupTest(t)
	ctx := context.Background()
	const programID int64 = 327360102415397
	createTestReservation(t, pool, programID)
	enqueueErr := errors.New("injected post-commit enqueue error")
	enqueuer := &commitCheckingEnqueuer{target: pgstoretest.Recordings(pool), err: enqueueErr}
	w.enqueuer = enqueuer

	record := testRecord("commit-hint-1", programID, "finished")
	if err := w.processRecord(ctx, record); err != nil {
		t.Fatalf("processRecord = %v, want commit to succeed despite enqueue error", err)
	}
	if enqueuer.calls != 1 {
		t.Fatalf("enqueue calls after one observation = %d, want 1", enqueuer.calls)
	}
	if !enqueuer.recordingVisible {
		t.Fatal("recording was not visible from another connection during enqueue, want enqueue after commit")
	}
	duplicate := testRecord("commit-hint-conflict", programID, "finished")
	duplicate.Program.StartAt = record.Program.StartAt
	if err := w.processRecord(ctx, duplicate); err == nil {
		t.Fatal("processRecord(duplicate event) returned nil, want store failure")
	}
	if enqueuer.calls != 1 {
		t.Errorf("enqueue calls after store failure = %d, want still 1", enqueuer.calls)
	}
}
