package riveradapter

import (
	"context"
	"errors"
	"strings"
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

// site 単位のキューに載る Args が QueueSite を持たないことは、Register ではなく
// NewAdapter（の worker 構築）が、注入された queueFor で拒む。
func TestNewAdapterRejectsSiteBoundArgsWithoutQueueSite(t *testing.T) {
	if _, err := makeInsertOpts(testQueueFor, argsMissingQueueSite{}); err == nil {
		t.Fatal("makeInsertOpts() error = nil, want missing QueueSite error")
	}
	registry := jobq.NewRegistry()
	Register(registry, missingQueueSiteWorker{}) // 登録時には panic しない
	_, err := buildRegistryConfig(registry, testQueueFor, Config{Spec: siteSpec(1, "tokyo")})
	if err == nil || !strings.Contains(err.Error(), "QueueSite") {
		t.Fatalf("buildRegistryConfig() error = %v, want missing QueueSite error", err)
	}
}

// 起動時の検査は注入された queueFor を通る（製品の規則を adapter が持っていない証拠）。
// 通る queueFor なら起動できる --- 反対方向。
func TestNewAdapterChecksRegisteredKindsWithInjectedQueueFor(t *testing.T) {
	registry := jobq.NewRegistry()
	Register(registry, missingQueueSiteWorker{})
	spec := siteSpec(1, "tokyo")
	lenient := func(args jobq.Args) (string, error) { return args.Spec().Queue, nil }
	if _, err := buildRegistryConfig(registry, lenient, Config{Spec: spec}); err != nil {
		t.Fatalf("lenient queueFor: %v", err)
	}
	strict := func(jobq.Args) (string, error) { return "", errors.New("injected refusal") }
	_, err := buildRegistryConfig(registry, strict, Config{Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "injected refusal") {
		t.Fatalf("strict queueFor: error = %v, want injected refusal", err)
	}
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
