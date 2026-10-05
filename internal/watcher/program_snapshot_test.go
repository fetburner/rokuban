package watcher

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
)

func TestSnapshotFromRecord(t *testing.T) {
	startAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	mirakcStartAt := mirakc.Milliseconds(startAt)
	duration := int64(180_000)
	title := "record title"
	description := "record description"

	input := mirakc.Record{
		Program: mirakc.Program{
			StartAt:     &mirakcStartAt,
			Duration:    &duration,
			IsFree:      true,
			Name:        &title,
			Description: &description,
			Extended:    map[string]string{"cast": "record cast"},
			Genres:      []mirakc.Genre{{LV1: 1, LV2: 2, UN1: 3, UN2: 4}},
		},
		Service: mirakc.Service{
			Name:    "record service",
			Channel: mirakc.ServiceChannel{Type: "GR", Channel: "27"},
		},
	}
	got := snapshotFromRecord(input)
	want := programSnapshot{
		serviceName:       "record service",
		channelType:       "GR",
		channel:           "27",
		title:             "record title",
		description:       &description,
		extended:          json.RawMessage(`{"cast":"record cast"}`),
		genres:            json.RawMessage(`[{"lv1":1,"lv2":2,"un1":3,"un2":4}]`),
		isFree:            true,
		programStartAt:    startAt,
		programDurationMs: duration,
	}
	assertProgramSnapshotEqual(t, got, want)

	input.Program.IsFree = false
	want.isFree = false
	assertProgramSnapshotEqual(t, snapshotFromRecord(input), want)
}

func TestSnapshotFromSchedule(t *testing.T) {
	startAt := time.Date(2026, time.February, 3, 4, 5, 6, 0, time.UTC)
	mirakcStartAt := mirakc.Milliseconds(startAt)
	duration := int64(240_000)
	title := "schedule title"
	description := "schedule description"

	input := mirakc.Schedule{
		Program: mirakc.Program{
			StartAt:     &mirakcStartAt,
			Duration:    &duration,
			IsFree:      true,
			Name:        &title,
			Description: &description,
			Extended:    map[string]string{"cast": "schedule cast"},
			Genres:      []mirakc.Genre{{LV1: 5, LV2: 6, UN1: 7, UN2: 8}},
		},
	}
	service := mirakc.Service{
		Name:    "schedule service",
		Channel: mirakc.ServiceChannel{Type: "BS", Channel: "101"},
	}
	got := snapshotFromSchedule(input, service)
	want := programSnapshot{
		serviceName:       "schedule service",
		channelType:       "BS",
		channel:           "101",
		title:             "schedule title",
		description:       &description,
		extended:          json.RawMessage(`{"cast":"schedule cast"}`),
		genres:            json.RawMessage(`[{"lv1":5,"lv2":6,"un1":7,"un2":8}]`),
		isFree:            true,
		programStartAt:    startAt,
		programDurationMs: duration,
	}
	assertProgramSnapshotEqual(t, got, want)

	input.Program.IsFree = false
	want.isFree = false
	assertProgramSnapshotEqual(t, snapshotFromSchedule(input, service), want)
}

func TestProgramSnapshotParamsAdapter(t *testing.T) {
	startAt := time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)
	description := "description"
	ruleID := int64(81)
	snapshot := programSnapshot{
		serviceName:       "service",
		channelType:       "GR",
		channel:           "27",
		title:             "title",
		description:       &description,
		extended:          json.RawMessage(`{"key":"value"}`),
		genres:            json.RawMessage(`[{"lv1":1}]`),
		isFree:            true,
		programStartAt:    startAt,
		programDurationMs: 300_000,
	}

	base := sqlcgen.CreateOrGetFailedRecordingParams{
		RuleID: &ruleID, Source: "rule", Site: "site", NetworkID: 1, ServiceID: 2, EventID: 3,
	}
	wantCreateOrGet := base
	wantCreateOrGet.ServiceName = "service"
	wantCreateOrGet.ChannelType = "GR"
	wantCreateOrGet.Channel = "27"
	wantCreateOrGet.Title = "title"
	wantCreateOrGet.Description = &description
	wantCreateOrGet.Extended = json.RawMessage(`{"key":"value"}`)
	wantCreateOrGet.Genres = json.RawMessage(`[{"lv1":1}]`)
	wantCreateOrGet.IsFree = true
	wantCreateOrGet.ProgramStartAt = startAt
	wantCreateOrGet.ProgramDurationMs = 300_000
	if got := snapshot.withCreateOrGetFailedRecordingParams(base); !reflect.DeepEqual(got, wantCreateOrGet) {
		t.Errorf("withCreateOrGetFailedRecordingParams() = %#v, want %#v", got, wantCreateOrGet)
	}
}

func assertProgramSnapshotEqual(t *testing.T, got, want programSnapshot) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("program snapshot = %#v, want %#v", got, want)
	}
}
