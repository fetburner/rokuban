package riveradapter

import (
	"testing"

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
}
