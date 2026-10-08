package db

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"
)

func TestMigrateRemoveBCASAnomalyQualityEvents(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()
	if err := MigrateReset(ctx, dbURL); err != nil {
		t.Fatalf("reset before migration test: %v", err)
	}
	t.Cleanup(func() { _ = MigrateReset(ctx, dbURL) })

	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.UpTo(ctx, 22)
		return err
	}); err != nil {
		t.Fatalf("migrate to version 22: %v", err)
	}

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	oldUpdatedAt := time.Date(2001, time.January, 2, 3, 4, 5, 0, time.UTC)
	withAnomalies := insertQualityEventsMigrationFixture(t, conn, 101, oldUpdatedAt, `[
		{"at":"2025-01-01T00:00:00Z","event":"recording.failed","reason":{"type":"earlier"}},
		{"at":"2025-01-02T00:00:00Z","event":"bcas_anomaly"},
		{"at":"2025-01-03T00:00:00Z","event":"recording.record-broken","reason":{"reason":"disk-error"}},
		{"at":"2025-01-03T00:00:01Z","event":"recording.record-broken","reason":{"reason":"disk-error"}},
		{"at":"2025-01-04T00:00:00Z","event":"recording.failed","reason":{"type":"later"}},
		{"at":"2025-01-05T00:00:00Z","event":"bcas_anomaly"},
		{"at":"2025-01-06T00:00:00Z","event":"future-event"}
	]`)
	bcasOnly := insertQualityEventsMigrationFixture(t, conn, 102, oldUpdatedAt, `[
		{"at":"2025-01-07T00:00:00Z","event":"bcas_anomaly"}
	]`)
	withoutAnomaly := insertQualityEventsMigrationFixture(t, conn, 103, oldUpdatedAt, `[
		{"at":"2025-01-08T00:00:00Z","event":"recording.record-broken","reason":{"reason":"io-error"}}
	]`)

	want := map[int64]string{
		withAnomalies: `[
			{"at":"2025-01-01T00:00:00Z","event":"recording.failed","reason":{"type":"earlier"}},
			{"at":"2025-01-03T00:00:00Z","event":"recording.record-broken","reason":{"reason":"disk-error"}},
			{"at":"2025-01-03T00:00:01Z","event":"recording.record-broken","reason":{"reason":"disk-error"}},
			{"at":"2025-01-04T00:00:00Z","event":"recording.failed","reason":{"type":"later"}},
			{"at":"2025-01-06T00:00:00Z","event":"future-event"}
		]`,
		bcasOnly: `[]`,
		withoutAnomaly: `[
			{"at":"2025-01-08T00:00:00Z","event":"recording.record-broken","reason":{"reason":"io-error"}}
		]`,
	}
	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.UpTo(ctx, 23)
		return err
	}); err != nil {
		t.Fatalf("migrate to version 23: %v", err)
	}

	updatedAtAfterUp := make(map[int64]time.Time, len(want))
	for id, expectedJSON := range want {
		var gotJSON []byte
		var gotUpdatedAt time.Time
		if err := conn.QueryRow(ctx,
			"SELECT quality_events, updated_at FROM recordings WHERE id = $1", id,
		).Scan(&gotJSON, &gotUpdatedAt); err != nil {
			t.Fatalf("querying recording %d after up: %v", id, err)
		}
		if !sameJSONValue(t, gotJSON, []byte(expectedJSON)) {
			t.Errorf("recording %d quality_events = %s, want %s", id, gotJSON, expectedJSON)
		}
		if id == withoutAnomaly {
			if !gotUpdatedAt.Equal(oldUpdatedAt) {
				t.Errorf("unchanged recording updated_at = %s, want %s", gotUpdatedAt, oldUpdatedAt)
			}
		} else if !gotUpdatedAt.After(oldUpdatedAt) {
			t.Errorf("changed recording updated_at = %s, want after %s", gotUpdatedAt, oldUpdatedAt)
		}
		updatedAtAfterUp[id] = gotUpdatedAt
	}

	if err := runGooseMigration(ctx, dbURL, func(ctx context.Context, p *goose.Provider) error {
		_, err := p.DownTo(ctx, 22)
		return err
	}); err != nil {
		t.Fatalf("migrate down to version 22: %v", err)
	}
	for id, expectedJSON := range want {
		var gotJSON []byte
		var gotUpdatedAt time.Time
		if err := conn.QueryRow(ctx,
			"SELECT quality_events, updated_at FROM recordings WHERE id = $1", id,
		).Scan(&gotJSON, &gotUpdatedAt); err != nil {
			t.Fatalf("querying recording %d after down: %v", id, err)
		}
		if !sameJSONValue(t, gotJSON, []byte(expectedJSON)) {
			t.Errorf("recording %d quality_events after down = %s, want %s", id, gotJSON, expectedJSON)
		}
		if !gotUpdatedAt.Equal(updatedAtAfterUp[id]) {
			t.Errorf("recording %d updated_at changed during down: got %s, want %s", id, gotUpdatedAt, updatedAtAfterUp[id])
		}
	}
}

func insertQualityEventsMigrationFixture(t *testing.T, conn *pgx.Conn, eventID int32, updatedAt time.Time, qualityEvents string) int64 {
	t.Helper()
	var id int64
	err := conn.QueryRow(context.Background(), `
		INSERT INTO recordings (
			source, site, network_id, service_id, event_id, service_name,
			channel_type, channel, program_start_at, program_duration_ms,
			status, quality_events, updated_at
		) VALUES ('manual', 'default', 1, 1, $1, 'Test', 'GR', '27', now(), 1800000,
			'finished', $2::jsonb, $3)
		RETURNING id`, eventID, qualityEvents, updatedAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("inserting migration fixture %d: %v", eventID, err)
	}
	return id
}

func sameJSONValue(t *testing.T, got, want []byte) bool {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("unmarshalling got JSON %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("unmarshalling want JSON %s: %v", want, err)
	}
	return reflect.DeepEqual(gotValue, wantValue)
}
