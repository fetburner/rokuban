package jobq

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type testArgs struct {
	N int
}

func (testArgs) Kind() string { return "test_kind" }
func (testArgs) Spec() Spec {
	return Spec{Queue: "q", Timeout: func(Args) time.Duration { return time.Minute }}
}

type emptyKindArgs struct{}

func (emptyKindArgs) Kind() string { return "" }
func (emptyKindArgs) Spec() Spec {
	return Spec{Timeout: func(Args) time.Duration { return time.Minute }}
}

type secondTestArgs struct{}

func (secondTestArgs) Kind() string { return "another_test_kind" }
func (secondTestArgs) Spec() Spec {
	return Spec{Queue: "q", Timeout: func(Args) time.Duration { return time.Minute }}
}

type testWorker struct{ got chan Args }

func (w testWorker) Work(_ context.Context, args testArgs) error {
	w.got <- args
	return nil
}

type ptrWorker struct{ got chan Args }

func (w ptrWorker) Work(_ context.Context, args *testArgs) error {
	w.got <- args
	return nil
}

type emptyKindWorker struct{}

func (emptyKindWorker) Work(context.Context, emptyKindArgs) error { return nil }

type secondTestWorker struct{}

func (secondTestWorker) Work(context.Context, secondTestArgs) error { return nil }

// recorder is an Enqueuer that remembers the last Args it saw.
type recorder struct {
	args Args
}

func (r *recorder) Enqueue(_ context.Context, args Args, _ ...EnqueueOpt) (Result, error) {
	r.args = args
	return Result{Coalesced: true}, nil
}

func TestDeferred(t *testing.T) {
	var deferred Deferred
	ctx := context.Background()
	if _, err := deferred.Enqueue(ctx, testArgs{}); err == nil {
		t.Error("Enqueue before Set returned nil error")
	}

	rec := &recorder{}
	deferred.Set(rec)
	got, err := deferred.Enqueue(ctx, testArgs{N: 7})
	if err != nil || !got.Coalesced {
		t.Fatalf("Enqueue after Set = %+v, %v; want forwarded result", got, err)
	}
	if rec.args != (testArgs{N: 7}) {
		t.Errorf("forwarded args = %#v, want testArgs{N: 7}", rec.args)
	}
	deferred.Set(nil)
	if _, err := deferred.Enqueue(ctx, testArgs{}); err == nil {
		t.Error("Enqueue after Set(nil) returned nil error")
	}
}

func TestRegisterAndDecode(t *testing.T) {
	registry := NewRegistry()
	got := make(chan Args, 1)
	Register[testArgs](registry, testWorker{got: got})

	args, err := registry.Decode("test_kind", []byte(`{"N":5}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if args != (testArgs{N: 5}) {
		t.Errorf("Decode() = %#v, want testArgs{N: 5}", args)
	}
	if err := registry.Work(context.Background(), args); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if v := <-got; v != (testArgs{N: 5}) {
		t.Errorf("worker received %#v, want testArgs{N: 5}", v)
	}
}

func TestRegisterPointerArgs(t *testing.T) {
	registry := NewRegistry()
	got := make(chan Args, 1)
	Register[*testArgs](registry, ptrWorker{got: got})

	args, err := registry.Decode("test_kind", []byte(`{"N":9}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	ptr, ok := args.(*testArgs)
	if !ok || ptr.N != 9 {
		t.Fatalf("Decode() = %#v, want *testArgs{N: 9}", args)
	}
	if err := registry.Work(context.Background(), args); err != nil {
		t.Fatalf("Work() error = %v", err)
	}
	if v := (<-got).(*testArgs); v.N != 9 {
		t.Errorf("worker received N = %d, want literal 9", v.N)
	}
}

func TestRegisterDuplicateKindPanics(t *testing.T) {
	registry := NewRegistry()
	Register[testArgs](registry, testWorker{})
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "already registered") {
			t.Errorf("recover() = %v, want already-registered panic", r)
		}
	}()
	Register[testArgs](registry, testWorker{})
}

func TestRegisterAdapterWorkers(t *testing.T) {
	registry := NewRegistry()
	Register[testArgs](registry, testWorker{})
	Register[secondTestArgs](registry, secondTestWorker{})

	var got []string
	for kind, value := range map[string]string{"test_kind": "z-kind", "another_test_kind": "a-kind"} {
		kind, value := kind, value
		if err := registry.AddAdapterWorker("test-adapter", kind, func(dst any) error {
			values, ok := dst.(*[]string)
			if !ok {
				return errors.New("unexpected adapter destination")
			}
			*values = append(*values, value)
			return nil
		}); err != nil {
			t.Fatalf("AddAdapterWorker(%q) error = %v", kind, err)
		}
	}
	if err := registry.AddAdapterWorker("test-adapter", "test_kind", func(any) error { return nil }); err == nil {
		t.Fatal("AddAdapterWorker accepted a duplicate builder")
	}
	if err := registry.AddAdapterWorker("test-adapter", "missing", func(any) error { return nil }); err == nil {
		t.Fatal("AddAdapterWorker accepted an unregistered job kind")
	}

	if err := registry.RegisterAdapterWorkers("missing-adapter", &got); err == nil {
		t.Fatal("RegisterAdapterWorkers accepted an adapter with no workers")
	}
	if err := registry.RegisterAdapterWorkers("test-adapter", &got); err != nil {
		t.Fatalf("RegisterAdapterWorkers() error = %v", err)
	}
	if len(got) != 2 || got[0] != "a-kind" || got[1] != "z-kind" {
		t.Errorf("adapter builders = %v, want [a-kind z-kind]", got)
	}
}

func TestRegisterAdapterWorkersRejectsKindWithoutAdapterWorker(t *testing.T) {
	registry := NewRegistry()
	Register[testArgs](registry, testWorker{})
	Register[secondTestArgs](registry, secondTestWorker{})
	if err := registry.AddAdapterWorker("test-adapter", "test_kind", func(any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	err := registry.RegisterAdapterWorkers("test-adapter", new([]string))
	if err == nil || !strings.Contains(err.Error(), "another_test_kind") {
		t.Fatalf("RegisterAdapterWorkers() error = %v, want it to name another_test_kind", err)
	}
}

func TestRegisterEmptyKindPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "empty kind") {
			t.Errorf("recover() = %v, want empty-kind panic", r)
		}
	}()
	Register[emptyKindArgs](NewRegistry(), emptyKindWorker{})
}

func TestDecodeUnregisteredKind(t *testing.T) {
	registry := NewRegistry()
	Register[testArgs](registry, testWorker{})
	_, err := registry.Decode("missing", []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Errorf("Decode(missing) error = %v, want error naming the kind", err)
	}
}

func TestSnooze(t *testing.T) {
	err := Snooze(-time.Second)
	if err == nil {
		t.Fatal("Snooze(-1s) = nil, want error")
	}
	if d, ok := SnoozeDelay(err); ok {
		t.Errorf("SnoozeDelay(negative snooze error) = %s, true; want false", d)
	}

	if d, ok := SnoozeDelay(Snooze(3 * time.Second)); !ok || d != 3*time.Second {
		t.Errorf("SnoozeDelay(Snooze(3s)) = %s, %v; want 3s, true", d, ok)
	}
	wrapped := errors.Join(errors.New("ctx"), Snooze(time.Second))
	if d, ok := SnoozeDelay(wrapped); !ok || d != time.Second {
		t.Errorf("SnoozeDelay(wrapped) = %s, %v; want 1s, true", d, ok)
	}
}

func TestEffectivePriority(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		opts []EnqueueOpt
		want int
	}{
		{"omitted is 1", Spec{}, nil, 1},
		{"spec value", Spec{Priority: 4}, nil, 4},
		{"option overrides spec", Spec{Priority: 4}, []EnqueueOpt{Priority(2)}, 2},
		{"option zero falls back to 1", Spec{Priority: 4}, []EnqueueOpt{Priority(0)}, 1},
		{"nil option ignored", Spec{Priority: 4}, []EnqueueOpt{nil}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectivePriority(tt.spec, tt.opts...); got != tt.want {
				t.Errorf("EffectivePriority() = %d, want %d", got, tt.want)
			}
		})
	}
}
