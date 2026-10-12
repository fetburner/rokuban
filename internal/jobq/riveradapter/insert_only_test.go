package riveradapter

import (
	"context"
	"testing"

	"github.com/riverqueue/river/rivertype"

	riveradaptertest "github.com/fetburner/rokuban/internal/jobq/riveradapter/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestNewInsertOnlyEnqueuerDoesNotStartWorkers(t *testing.T) {
	pool := testutil.SetupDB(t)
	adapter, err := NewInsertOnlyEnqueuer(pool)
	if err != nil {
		t.Fatalf("NewInsertOnlyEnqueuer: %v", err)
	}
	if adapter.started {
		t.Fatal("insert-only adapter started delivery during construction")
	}

	if _, err := adapter.Enqueue(context.Background(), jobs.EpgSyncArgs{Site: "tokyo"}); err != nil {
		t.Fatalf("enqueueing EPG sync: %v", err)
	}
	if adapter.started {
		t.Fatal("enqueue started delivery")
	}
	row := riveradaptertest.MustSingleRiverJobOfKind(t, context.Background(), pool, "epg_sync")
	if row.State != rivertype.JobStateAvailable {
		t.Errorf("job state after insert-only enqueue = %q, want %q", row.State, rivertype.JobStateAvailable)
	}
}
