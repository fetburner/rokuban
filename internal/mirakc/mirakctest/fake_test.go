package mirakctest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/programid"
)

func TestFakeReturnsPlacedStateThroughClient(t *testing.T) {
	fake := New(t)
	ctx := context.Background()
	service := mirakc.Service{ID: 1, ServiceID: 1, NetworkID: 1, Name: "test"}
	program := mirakc.Program{ID: 11, EventID: 11, Name: stringPtr("program")}
	schedule := mirakc.Schedule{
		State:   mirakc.ScheduleStateScheduled,
		Program: program,
		Options: mirakc.Options{Priority: 7},
	}
	record := mirakc.Record{ID: "record-1", Program: program}
	fake.SetServices([]mirakc.Service{service})
	fake.SetPrograms([]mirakc.Program{program})
	fake.SetTuners([]mirakc.Tuner{{Index: 1, Name: "tuner-1", Types: []string{"GR"}}})
	fake.SetSchedules([]mirakc.Schedule{schedule})
	fake.SetRecords([]mirakc.Record{record})
	client := mirakc.NewClient(fake.URL(), nil)

	services, err := client.ListServices(ctx)
	if err != nil || len(services) != 1 || services[0].Name != "test" {
		t.Fatalf("ListServices() = %#v, %v; want the placed service", services, err)
	}
	programs, err := client.ListPrograms(ctx)
	if err != nil || len(programs) != 1 || programs[0].ID != program.ID {
		t.Fatalf("ListPrograms() = %#v, %v; want the placed program", programs, err)
	}
	tuners, err := client.ListTuners(ctx)
	if err != nil || len(tuners) != 1 || tuners[0].Name != "tuner-1" {
		t.Fatalf("ListTuners() = %#v, %v; want the placed tuner", tuners, err)
	}
	schedules, err := client.ListSchedules(ctx)
	if err != nil || len(schedules) != 1 || schedules[0].Options.Priority != 7 {
		t.Fatalf("ListSchedules() = %#v, %v; want the placed schedule", schedules, err)
	}
	records, err := client.ListRecords(ctx)
	if err != nil || len(records) != 1 || records[0].ID != record.ID {
		t.Fatalf("ListRecords() = %#v, %v; want the placed record", records, err)
	}
	gotRecord, err := client.GetRecord(ctx, record.ID)
	if err != nil || gotRecord.ID != record.ID {
		t.Fatalf("GetRecord() = %#v, %v; want record %q", gotRecord, err, record.ID)
	}

	response, err := http.Get(fake.URL() + servicesPath)
	if err != nil {
		t.Fatalf("GET %s: %v", servicesPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestFakeScheduleAndRecordDeletesUpdateStateAndCalls(t *testing.T) {
	fake := New(t)
	ctx := context.Background()
	programID := programid.ComposeProgramID(32736, 1024, 33)
	fake.SetRecords([]mirakc.Record{{ID: "record-delete"}})
	client := mirakc.NewClient(fake.URL(), nil)

	path := stringPtr("shows/test.ts")
	created, err := client.CreateSchedule(ctx, mirakc.ScheduleInput{
		ProgramID: programID,
		Options:   mirakc.Options{ContentPath: path, Priority: 4},
		Tags:      []string{"rokuban:test"},
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if created.Program.EventID != 33 || created.Program.Duration == nil || *created.Program.Duration != 300000 {
		t.Fatalf("created schedule program = %+v, want event 33 and 300000ms duration", created.Program)
	}
	if got := fake.Schedules()[programID]; got.Options.ContentPath == nil || *got.Options.ContentPath != *path {
		t.Fatalf("schedule state after POST = %+v, want contentPath %q", got, *path)
	}
	if err := client.DeleteSchedule(ctx, programID); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if got := fake.Schedules(); len(got) != 0 {
		t.Errorf("schedule state after DELETE = %+v, want empty", got)
	}
	if _, err := client.DeleteRecord(ctx, "record-delete", true); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if records, err := client.ListRecords(ctx); err != nil || len(records) != 0 {
		t.Errorf("ListRecords after DELETE = %#v, %v; want empty", records, err)
	}

	calls := fake.Calls()
	if !hasCall(calls, http.MethodPost, schedulesPath) || !hasCall(calls, http.MethodDelete, schedulesPath+"/"+formatInt(programID)) {
		t.Errorf("calls = %#v, want schedule POST and DELETE", calls)
	}
	if !hasCall(calls, http.MethodDelete, recordsPath+"/record-delete") {
		t.Errorf("calls = %#v, want record DELETE", calls)
	}
}

func TestFakeFailureInjectionAppliesExactlyNRequests(t *testing.T) {
	fake := New(t)
	client := mirakc.NewClient(fake.URL(), nil)
	programID := programid.ComposeProgramID(32736, 1024, 44)
	fake.FailNext(http.MethodPost, schedulesPath, 2, http.StatusServiceUnavailable)
	input := mirakc.ScheduleInput{ProgramID: programID}

	for i := 0; i < 2; i++ {
		if _, err := client.CreateSchedule(context.Background(), input); err == nil {
			t.Fatalf("CreateSchedule attempt %d succeeded, want injected failure", i+1)
		} else {
			var apiErr *mirakc.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("CreateSchedule attempt %d error = %#v, want status %d", i+1, err, http.StatusServiceUnavailable)
			}
		}
	}
	if got := fake.Schedules(); len(got) != 0 {
		t.Fatalf("schedule state after failures = %+v, want empty", got)
	}
	if _, err := client.CreateSchedule(context.Background(), input); err != nil {
		t.Fatalf("CreateSchedule after injected failures: %v", err)
	}
	if got := fake.Schedules(); len(got) != 1 {
		t.Errorf("schedule state after the third POST = %+v, want one schedule", got)
	}
	if got := len(fake.Calls()); got != 3 {
		t.Errorf("recorded calls = %d, want 3", got)
	}
}

func TestFakeRecordStreamUsesRangeOffsetAndRejectsOutOfRange(t *testing.T) {
	fake := New(t)
	fake.SetRecords([]mirakc.Record{{ID: "record-stream"}})
	data := []byte("0123456789")
	fake.SetRecordStream("record-stream", data)
	client := mirakc.NewClient(fake.URL(), nil)

	length, err := client.HeadRecordStream(context.Background(), "record-stream")
	if err != nil || length != int64(len(data)) {
		t.Fatalf("HeadRecordStream() = %d, %v; want %d", length, err, len(data))
	}
	body, length, err := client.StreamRecord(context.Background(), "record-stream", 4)
	if err != nil {
		t.Fatalf("StreamRecord(offset 4): %v", err)
	}
	got, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(got) != string(data[4:]) || length != int64(len(data)-4) {
		t.Fatalf("StreamRecord(offset 4) = %q, %d, %v; want %q and %d bytes", got, length, err, data[4:], len(data)-4)
	}
	if _, _, err := client.StreamRecord(context.Background(), "record-stream", int64(len(data))); !errors.Is(err, mirakc.ErrRangeNotSatisfiable) {
		t.Errorf("StreamRecord(out of range) error = %v, want ErrRangeNotSatisfiable", err)
	}
}

func hasCall(calls []Call, method, path string) bool {
	for _, call := range calls {
		if call.Method == method && call.Path == path {
			return true
		}
	}
	return false
}

func stringPtr(value string) *string { return &value }

func formatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
