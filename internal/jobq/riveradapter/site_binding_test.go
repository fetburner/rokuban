package riveradapter

import (
	"context"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
)

type argsMissingQueueSite struct{}

func (argsMissingQueueSite) Kind() string { return "invalid_site_bound" }
func (argsMissingQueueSite) Spec() jobq.Spec {
	return jobq.Spec{Queue: jobs.IngestQueue, Timeout: func(jobq.Args) time.Duration { return time.Second }}
}

type missingQueueSiteWorker struct{}

func (missingQueueSiteWorker) Work(context.Context, argsMissingQueueSite) error { return nil }

type recordSweepRegistrationWorker struct{}

func (recordSweepRegistrationWorker) Work(context.Context, jobs.RecordSweepArgs) error { return nil }

func TestRegisterRejectsSiteBoundArgsWithoutQueueSite(t *testing.T) {
	if _, err := makeInsertOpts(argsMissingQueueSite{}); err == nil {
		t.Fatal("makeInsertOpts() error = nil, want missing QueueSite error")
	}
	registry := jobq.NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("Register() did not panic for a site-bound Args type without QueueSite")
		}
	}()
	Register(registry, missingQueueSiteWorker{})
}

func TestRegisterAcceptsRecordSweepAndRulerArgs(t *testing.T) {
	t.Run("record_sweep has QueueSite", func(t *testing.T) {
		registry := jobq.NewRegistry()
		Register(registry, recordSweepRegistrationWorker{})
		if err := registry.Work(context.Background(), jobs.RecordSweepArgs{Site: "tokyo"}); err != nil {
			t.Fatalf("registry.Work(record_sweep): %v", err)
		}
	})

	t.Run("ruler is site independent despite carrying Site", func(t *testing.T) {
		registry := jobq.NewRegistry()
		Register(registry, rulerRegistrationWorker{})
		if err := registry.Work(context.Background(), jobs.RulerPassArgs{Site: "tokyo"}); err != nil {
			t.Fatalf("registry.Work(ruler_pass): %v", err)
		}
	})
}

type rulerRegistrationWorker struct{}

func (rulerRegistrationWorker) Work(context.Context, jobs.RulerPassArgs) error { return nil }
