package worker

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/testutil"
)

func TestNoOpJob(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleaning river_job: %v", err)
	}

	workers := NewWorkers()
	client, err := NewClient(pool, workers)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	subscribeCh, subscribeCancel := client.Subscribe(river.EventKindJobCompleted)
	defer subscribeCancel()

	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()

	if err := client.Start(clientCtx); err != nil {
		t.Fatalf("starting client: %v", err)
	}
	defer func() {
		clientCancel()
		<-client.Stopped()
	}()

	_, err = client.Insert(ctx, NoOpArgs{}, nil)
	if err != nil {
		t.Fatalf("inserting job: %v", err)
	}

	select {
	case event := <-subscribeCh:
		if event.Job.Kind != "noop" {
			t.Errorf("job kind = %q, want %q", event.Job.Kind, "noop")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for job completion")
	}
}
