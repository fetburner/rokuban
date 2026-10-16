package riveradapter

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/jobq"
	riveradaptertest "github.com/fetburner/rokuban/internal/jobq/riveradapter/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestIngestDuplicateEnqueuesCoalesceInAdapter(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	enqueuer, err := NewInsertOnlyEnqueuer(pool, testQueueFor)
	if err != nil {
		t.Fatalf("NewInsertOnlyEnqueuer: %v", err)
	}
	args := jobs.IngestJobArgs{Site: "tokyo", RecordID: "coalesce-1"}

	first, err := enqueuer.Enqueue(ctx, args, jobq.Priority(1))
	if err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if first.Coalesced {
		t.Fatal("first Enqueue was coalesced, want a new River job")
	}
	second, err := enqueuer.Enqueue(ctx, args, jobq.Priority(2))
	if err != nil {
		t.Fatalf("second Enqueue: %v", err)
	}
	if !second.Coalesced {
		t.Fatal("second Enqueue was not coalesced with the pending River job")
	}

	rows := riveradaptertest.MustListRiverJobsOfKind(t, ctx, pool, args.Kind())
	if len(rows) != 1 {
		t.Fatalf("River ingest jobs = %d, want 1 after duplicate enqueue", len(rows))
	}
	if rows[0].Queue != "ingest_tokyo" {
		t.Errorf("River queue = %q, want %q", rows[0].Queue, "ingest_tokyo")
	}
	if rows[0].Priority != 1 {
		t.Errorf("River priority after duplicate enqueue = %d, want the first request's priority 1", rows[0].Priority)
	}
}
