package db

import (
	"context"
	"testing"

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
		{name: "older than threshold uses first matching rule", kind: "original", site: "takamatsu", age: "30 days 1 second", wantStore: "tokyo"},
		{name: "exact threshold uses next rule", kind: "original", site: "takamatsu", age: "30 days", wantStore: "takamatsu"},
		{name: "younger program uses site rule", kind: "original", site: "takamatsu", age: "29 days", wantStore: "takamatsu"},
		{name: "site mismatch uses unconditional fallback", kind: "original", site: "tokyo", age: "40 days", wantStore: "fallback"},
		{name: "derived asset uses constant store", kind: "encoded", site: "takamatsu", age: "40 days", wantStore: "s3main"},
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
}
