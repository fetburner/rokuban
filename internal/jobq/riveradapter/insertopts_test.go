package riveradapter

import (
	"testing"

	"github.com/riverqueue/river/rivertype"

	"github.com/fetburner/rokuban/internal/jobs"
)

func TestMakeInsertOptsQueueAndUnique(t *testing.T) {
	bound, err := makeInsertOpts(jobs.IngestJobArgs{Site: "tokyo", RecordID: "r1"})
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

	ptr, err := makeInsertOpts(&jobs.IngestJobArgs{Site: "tokyo"})
	if err != nil {
		t.Fatal(err)
	}
	if ptr.Queue != "ingest_tokyo" {
		t.Errorf("pointer Args Queue = %q, want ingest_tokyo", ptr.Queue)
	}

	unbound, err := makeInsertOpts(jobs.RulerPassArgs{Site: "tokyo"})
	if err != nil {
		t.Fatal(err)
	}
	if unbound.Queue != "ruler" {
		t.Errorf("non-site-bound Queue = %q, want ruler", unbound.Queue)
	}

	nonCoalescing, err := makeInsertOpts(jobs.LabelRuleReconcileArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if nonCoalescing.UniqueOpts.ByArgs || nonCoalescing.UniqueOpts.ByQueue || len(nonCoalescing.UniqueOpts.ByState) != 0 {
		t.Errorf("non-coalescing UniqueOpts = %+v, want zero options", nonCoalescing.UniqueOpts)
	}

	priority, err := makeInsertOpts(jobs.SeekTilesJobArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if priority.Priority != 4 {
		t.Errorf("seek_tiles Priority = %d, want literal 4", priority.Priority)
	}
}
