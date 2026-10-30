package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

func TestRecordingIngestProgressNotify(t *testing.T) {
	ctx := context.Background()
	dbURL := testDatabaseURL(t)
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("migrating to latest before setup: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE recordings CASCADE`); err != nil {
		t.Fatalf("clearing prior recording fixtures: %v", err)
	}

	migrateDownTo(t, dbURL, 43)
	t.Cleanup(func() {
		if err := MigrateUp(context.Background(), dbURL); err != nil {
			t.Errorf("restoring latest schema: %v", err)
		}
	})

	recordingID := dbtest.Recording(t, pool)
	listener, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring notification listener: %v", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, "LISTEN rokuban"); err != nil {
		t.Fatalf("listening for notifications: %v", err)
	}

	q := sqlcgen.New(pool)
	expected := int64(100)
	upsertProgress := func(generation int64, writtenBytes int64) {
		t.Helper()
		if err := q.UpsertRecordingIngestProgress(ctx, sqlcgen.UpsertRecordingIngestProgressParams{
			RecordingID: recordingID, WrittenBytes: writtenBytes, ExpectedBytes: &expected,
			TempRelPath: "ingest.tmp", TempGeneration: generation, TempStore: "media",
		}); err != nil {
			t.Fatalf("upserting ingest progress: %v", err)
		}
	}
	deleteProgress := func() {
		t.Helper()
		if err := q.DeleteRecordingIngestProgress(ctx, recordingID); err != nil {
			t.Fatalf("deleting ingest progress: %v", err)
		}
	}

	upsertProgress(1, 10)
	expectNoRecordingNotification(t, listener)
	deleteProgress()
	if err := MigrateUp(ctx, dbURL); err != nil {
		t.Fatalf("applying ingest progress notification migration: %v", err)
	}

	upsertProgress(2, 10)
	expectRecordingNotification(t, listener)
	upsertProgress(2, 20)
	expectNoRecordingNotification(t, listener)
	upsertProgress(3, 20)
	expectRecordingNotification(t, listener)
	deleteProgress()
	expectRecordingNotification(t, listener)
}

func expectRecordingNotification(t *testing.T, listener *pgxpool.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notification, err := listener.Conn().WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("waiting for recordings notification: %v", err)
	}
	if notification.Channel != "rokuban" || notification.Payload != "recordings" {
		t.Fatalf("notification = %s/%s, want rokuban/recordings", notification.Channel, notification.Payload)
	}
}

func expectNoRecordingNotification(t *testing.T, listener *pgxpool.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if notification, err := listener.Conn().WaitForNotification(ctx); err == nil {
		t.Fatalf("unexpected notification = %s/%s", notification.Channel, notification.Payload)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for absent notification: %v", err)
	}
}
