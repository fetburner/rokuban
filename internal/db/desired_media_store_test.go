package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fetburner/rokuban/internal/config"
)

func TestDesiredMediaStore(t *testing.T) {
	dbURL := testDatabaseURL(t)
	ctx := context.Background()
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() { _ = MigrateReset(ctx, dbURL) })

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	rules, err := (config.StorageConfig{Originals: []config.OriginalStoreRule{
		{Match: &config.OriginalStoreMatch{Site: "takamatsu", OlderThan: "30d"}, Store: "tokyo"},
		{Match: &config.OriginalStoreMatch{Site: "takamatsu"}, Store: "takamatsu"},
		{Store: "fallback"},
	}}).OriginalRulesJSON()
	if err != nil {
		t.Fatalf("normalize original rules: %v", err)
	}

	tests := []struct {
		name      string
		kind      string
		site      string
		age       string
		wantStore string
	}{
		{name: "older than threshold uses first matching rule", kind: "original", site: "takamatsu", age: "720 hours 1 second", wantStore: "tokyo"},
		{name: "exact threshold uses next rule", kind: "original", site: "takamatsu", age: "720 hours", wantStore: "takamatsu"},
		{name: "younger program uses site rule", kind: "original", site: "takamatsu", age: "696 hours", wantStore: "takamatsu"},
		{name: "site mismatch uses unconditional fallback", kind: "original", site: "tokyo", age: "960 hours", wantStore: "fallback"},
		{name: "derived asset uses constant store", kind: "encoded", site: "takamatsu", age: "960 hours", wantStore: "s3main"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			query := `SELECT desired_media_store($1, $2::jsonb, $3, $4, now() - $5::interval)`
			if err := conn.QueryRow(ctx, query, tc.kind, string(rules), "s3main", tc.site, tc.age).Scan(&got); err != nil {
				t.Fatalf("desired_media_store: %v", err)
			}
			if got != tc.wantStore {
				t.Fatalf("desired store = %q, want %q", got, tc.wantStore)
			}
		})
	}

	t.Run("subsecond threshold compares exact boundary", func(t *testing.T) {
		fractionalRules, err := (config.StorageConfig{Originals: []config.OriginalStoreRule{
			{Match: &config.OriginalStoreMatch{Site: "takamatsu", OlderThan: "720h500ms"}, Store: "tokyo"},
			{Match: &config.OriginalStoreMatch{Site: "takamatsu"}, Store: "takamatsu"},
			{Store: "fallback"},
		}}).OriginalRulesJSON()
		if err != nil {
			t.Fatalf("normalize fractional rules: %v", err)
		}
		var got string
		if err := conn.QueryRow(ctx,
			`SELECT desired_media_store('original', $1::jsonb, 's3main', 'takamatsu', now() - interval '720 hours 500 milliseconds')`,
			string(fractionalRules),
		).Scan(&got); err != nil {
			t.Fatalf("desired_media_store at subsecond threshold: %v", err)
		}
		if got != "takamatsu" {
			t.Fatalf("desired store at 720h500ms boundary = %q, want %q", got, "takamatsu")
		}
	})

	t.Run("older_than is fixed elapsed time across daylight saving changes", func(t *testing.T) {
		// 暦の日数を引いた時刻が、同じ日数の 24 時間を引いた時刻とずれるタイムゾーンと日数を選ぶ。
		// 実行日に依らず決定的で、実物の desired_media_store を通る。
		var zone string
		var days int
		var deltaSeconds float64
		var programEndAt time.Time
		err := conn.QueryRow(ctx, `
			WITH candidates AS (
				SELECT name, n AS days,
				       (now() AT TIME ZONE name - make_interval(days => n)) AT TIME ZONE name AS cutoff
				FROM pg_timezone_names
				CROSS JOIN generate_series(1, 365) AS series(n)
			), shifted AS (
				SELECT name, days, cutoff,
				       extract(epoch FROM now() - cutoff) - days * 86400::double precision AS delta_seconds
				FROM candidates
			)
			SELECT name, days, delta_seconds,
			       cutoff + CASE WHEN delta_seconds > 0 THEN interval '1 minute' ELSE interval '-1 minute' END
			FROM shifted
			WHERE abs(delta_seconds) > 120
			ORDER BY abs(delta_seconds) DESC
			LIMIT 1`).Scan(&zone, &days, &deltaSeconds, &programEndAt)
		if err != nil {
			t.Fatalf("find a timezone with a recent DST shift: %v", err)
		}
		if _, err := conn.Exec(ctx, `SELECT set_config('TimeZone', $1, false)`, zone); err != nil {
			t.Fatalf("set session timezone to %s: %v", zone, err)
		}
		t.Cleanup(func() {
			_, _ = conn.Exec(ctx, `SELECT set_config('TimeZone', 'UTC', false)`)
		})

		rules, err := (config.StorageConfig{Originals: []config.OriginalStoreRule{
			{Match: &config.OriginalStoreMatch{Site: "takamatsu", OlderThan: fmt.Sprintf("%dd", days)}, Store: "tokyo"},
			{Match: &config.OriginalStoreMatch{Site: "takamatsu"}, Store: "takamatsu"},
			{Store: "fallback"},
		}}).OriginalRulesJSON()
		if err != nil {
			t.Fatalf("normalize DST test rules: %v", err)
		}
		var got string
		if err := conn.QueryRow(ctx,
			`SELECT desired_media_store('original', $1::jsonb, 's3main', 'takamatsu', $2::timestamptz)`,
			string(rules), programEndAt,
		).Scan(&got); err != nil {
			t.Fatalf("desired_media_store across DST: %v", err)
		}
		want := "takamatsu"
		if deltaSeconds > 0 {
			want = "tokyo"
		}
		if got != want {
			t.Fatalf("desired store across DST in %s for %dd (calendar shift %+0.fs) = %q, want %q", zone, days, deltaSeconds, got, want)
		}
	})
}
