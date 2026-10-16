package riveradapter

import (
	"errors"
	"testing"

	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
)

func TestMakeInsertOptsQueueAndUnique(t *testing.T) {
	bound, err := makeInsertOpts(testQueueFor, jobs.IngestJobArgs{Site: "tokyo", RecordID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Queue != "ingest_tokyo" {
		t.Errorf("site-bound Queue = %q, want ingest_tokyo", bound.Queue)
	}
	if !bound.UniqueOpts.ByArgs || !bound.UniqueOpts.ByQueue {
		t.Errorf("UniqueOpts = %+v, want ByArgs and ByQueue for a coalescing spec", bound.UniqueOpts)
	}
	wantStates := []rivertype.JobState{"available", "pending", "retryable", "running", "scheduled"}
	if len(bound.UniqueOpts.ByState) != len(wantStates) {
		t.Fatalf("UniqueOpts.ByState = %v, want %v", bound.UniqueOpts.ByState, wantStates)
	}
	for i, want := range wantStates {
		if got := bound.UniqueOpts.ByState[i]; got != want {
			t.Errorf("UniqueOpts.ByState[%d] = %q, want %q", i, got, want)
		}
	}

	ptr, err := makeInsertOpts(testQueueFor, &jobs.IngestJobArgs{Site: "tokyo"})
	if err != nil {
		t.Fatal(err)
	}
	if ptr.Queue != "ingest_tokyo" {
		t.Errorf("pointer Args Queue = %q, want ingest_tokyo", ptr.Queue)
	}

	unbound, err := makeInsertOpts(testQueueFor, jobs.RulerPassArgs{Site: "tokyo"})
	if err != nil {
		t.Fatal(err)
	}
	if unbound.Queue != "ruler" {
		t.Errorf("non-site-bound Queue = %q, want ruler", unbound.Queue)
	}

	nonCoalescing, err := makeInsertOpts(testQueueFor, jobs.LabelRuleReconcileArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if nonCoalescing.UniqueOpts.ByArgs || nonCoalescing.UniqueOpts.ByQueue || len(nonCoalescing.UniqueOpts.ByState) != 0 {
		t.Errorf("non-coalescing UniqueOpts = %+v, want zero options", nonCoalescing.UniqueOpts)
	}

	priority, err := makeInsertOpts(testQueueFor, jobs.SeekTilesJobArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if priority.Priority != 4 {
		t.Errorf("seek_tiles Priority = %d, want literal 4", priority.Priority)
	}
}

// 投入先の物理キューは注入された queueFor が決める（adapter は命名規則を持たない）。
// nil の queueFor は、論理名にそのまま倒さず拒む。
func TestQueueForIsInjected(t *testing.T) {
	custom := func(jobq.Args) (string, error) { return "injected_queue", nil }
	opts, err := makeInsertOpts(custom, jobs.IngestJobArgs{Site: "tokyo", RecordID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Queue != "injected_queue" {
		t.Errorf("Queue = %q, want injected_queue", opts.Queue)
	}
	failing := func(jobq.Args) (string, error) { return "", errors.New("no queue") }
	if _, err := makeInsertOpts(failing, jobs.IngestJobArgs{}); err == nil {
		t.Error("makeInsertOpts accepted a queueFor error")
	}
	if _, err := NewInsertOnlyEnqueuer(nil, nil); err == nil {
		t.Error("NewInsertOnlyEnqueuer accepted a nil queueFor")
	}
	if _, err := NewAdapter(nil, jobq.NewRegistry(), nil, Config{}); err == nil {
		t.Error("NewAdapter accepted a nil queueFor")
	}
}
