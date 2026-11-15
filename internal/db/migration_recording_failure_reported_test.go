package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

func TestRecordingFailureReportedMigration(t *testing.T) {
	ctx := context.Background()
	url := testDatabaseURL(t)
	if err := MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	migrateDownTo(t, url, 72)
	t.Cleanup(func() {
		if err := MigrateUp(ctx, url); err != nil {
			t.Error(err)
		}
	})
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ids := []int64{dbtest.Recording(t, pool), dbtest.Recording(t, pool), dbtest.Recording(t, pool)}
	events := []byte(`[{"event":"recording.failed","reason":{"type":"signal-loss"}}]`)
	q := sqlcgen.New(pool)
	if err := q.AppendQualityEvents(ctx, sqlcgen.AppendQualityEventsParams{ID: ids[0], Events: events}); err != nil {
		t.Fatal(err)
	}
	if err := q.AppendQualityEvents(ctx, sqlcgen.AppendQualityEventsParams{ID: ids[1], Events: []byte(`[{"event":"recording.record-broken"}]`)}); err != nil {
		t.Fatal(err)
	}
	if err := MigrateUp(ctx, url); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		var reported bool
		if err := pool.QueryRow(ctx, `SELECT failure_reported FROM recordings WHERE id=$1`, id).Scan(&reported); err != nil {
			t.Fatal(err)
		}
		if reported != (i == 0) {
			t.Fatalf("fixture %d reported=%t", i, reported)
		}
		for pass := 0; pass < 3; pass++ {
			n, err := q.AppendQualityEventsIfMissing(ctx, sqlcgen.AppendQualityEventsIfMissingParams{ID: id, Events: events})
			if err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if i != 0 && pass == 0 {
				want = 1
			}
			if n != want {
				t.Fatalf("fixture %d sweep %d rows=%d want=%d", i, pass, n, want)
			}
		}
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT quality_events FROM recordings WHERE id=$1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var got []json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		want := 1
		if i == 1 {
			want = 2
		}
		if len(got) != want {
			t.Fatalf("fixture %d events=%d want=%d", i, len(got), want)
		}
	}
}
