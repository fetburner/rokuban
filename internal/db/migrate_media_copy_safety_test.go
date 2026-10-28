package db

import (
	"context"
	"strings"
	"testing"
)

func TestMediaCopyMigrationRequiresStoppedWritersAtLegacyCopyVersions(t *testing.T) {
	for _, tc := range []struct {
		version int64
		want    bool
	}{
		{version: 36, want: false},
		{version: 37, want: true},
		{version: 39, want: true},
		{version: 40, want: false},
		{version: 41, want: false},
	} {
		if got := mediaCopyMigrationRequiresStoppedWriters(tc.version); got != tc.want {
			t.Errorf("mediaCopyMigrationRequiresStoppedWriters(%d) = %t, want %t", tc.version, got, tc.want)
		}
	}
}

func TestCheckMediaCopyMigrationSafetyRequiresAcknowledgement(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	migrateDownTo(t, dbURL, 39)
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	err := CheckMediaCopyMigrationSafety(ctx, dbURL, false)
	if err == nil || !strings.Contains(err.Error(), "--media-copy-writers-stopped") {
		t.Fatalf("CheckMediaCopyMigrationSafety without acknowledgement = %v, want a stopped-writers error", err)
	}
	if err := CheckMediaCopyMigrationSafety(ctx, dbURL, true); err != nil {
		t.Fatalf("CheckMediaCopyMigrationSafety with acknowledgement: %v", err)
	}
}
