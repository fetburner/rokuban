package jobq

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func validSpec() WorkerSpec {
	return WorkerSpec{
		Queues:       []QueueSpec{{Logical: "q", Physical: []string{"q"}, MaxWorkers: 2}, {Logical: "r", Physical: []string{"r"}, MaxWorkers: 1}},
		PeriodicJobs: []PeriodicJobSpec{{Interval: time.Minute, Args: testArgs{}}},
	}
}

func TestWorkerSpecValidate(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("Validate(valid) = %v, want nil", err)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*WorkerSpec)
		want   string
	}{
		{"empty logical", func(s *WorkerSpec) { s.Queues[1].Logical = "" }, "empty logical name"},
		{"duplicate logical", func(s *WorkerSpec) { s.Queues[1].Logical = "q" }, `queue "q" more than once`},
		{"zero MaxWorkers", func(s *WorkerSpec) { s.Queues[0].MaxWorkers = 0 }, "MaxWorkers must be >= 1"},
		{"negative MaxWorkers", func(s *WorkerSpec) { s.Queues[0].MaxWorkers = -1 }, "MaxWorkers must be >= 1"},
		{"zero interval", func(s *WorkerSpec) { s.PeriodicJobs[0].Interval = 0 }, "interval must be positive"},
		{"negative interval", func(s *WorkerSpec) { s.PeriodicJobs[0].Interval = -time.Second }, "interval must be positive"},
		{"nil args", func(s *WorkerSpec) { s.PeriodicJobs[0].Args = nil }, "nil args"},
		{"queue not in Queues", func(s *WorkerSpec) { s.PeriodicJobs[0].Args = emptyKindArgsInQueue{} }, `queue "other" which is not in Queues`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := validSpec()
			tt.mutate(&spec)
			err := spec.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

type emptyKindArgsInQueue struct{}

func (emptyKindArgsInQueue) Kind() string { return "other_kind" }
func (emptyKindArgsInQueue) Spec() Spec {
	return Spec{Queue: "other", Timeout: func(Args) time.Duration { return time.Minute }}
}

func TestWorkerSpecSelect(t *testing.T) {
	spec := validSpec()
	got, err := spec.Select([]string{"r"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got.Queues) != 1 || got.Queues[0].Logical != "r" || got.Queues[0].MaxWorkers != 1 {
		t.Errorf("Select([r]).Queues = %+v, want only r", got.Queues)
	}
	if len(got.PeriodicJobs) != 1 {
		t.Errorf("Select must keep periodic jobs, got %d", len(got.PeriodicJobs))
	}

	all, err := spec.Select(nil)
	if err != nil {
		t.Fatalf("Select(nil): %v", err)
	}
	if len(all.Queues) != 2 {
		t.Errorf("Select(nil) kept %d queues, want 2", len(all.Queues))
	}
	// 返した Queues を書き換えても元の仕様は変わらない。
	all.Queues[0].MaxWorkers = 99
	if spec.Queues[0].MaxWorkers != 2 {
		t.Errorf("Select(nil) shares Queues with the original spec")
	}

	_, err = spec.Select([]string{"q", "bogus"})
	want := `worker.queues: unknown queue "bogus" (valid: q, r)`
	if err == nil || err.Error() != want {
		t.Errorf("Select(unknown) = %v, want %q", err, want)
	}
	if !slices.Equal([]string{spec.Queues[0].Logical, spec.Queues[1].Logical}, []string{"q", "r"}) {
		t.Error("Select reordered the original spec")
	}
}

func TestWorkerSpecCheckKinds(t *testing.T) {
	registry := NewRegistry()
	spec := validSpec()
	err := spec.CheckKinds(registry)
	if err == nil || !strings.Contains(err.Error(), `"test_kind"`) {
		t.Fatalf("CheckKinds(unregistered) = %v, want an error naming test_kind", err)
	}
	Register(registry, testWorker{got: make(chan Args, 1)})
	if err := spec.CheckKinds(registry); err != nil {
		t.Errorf("CheckKinds(registered) = %v, want nil", err)
	}
	if err := (WorkerSpec{}).CheckKinds(NewRegistry()); err != nil {
		t.Errorf("CheckKinds(no periodic jobs) = %v, want nil", err)
	}
}
