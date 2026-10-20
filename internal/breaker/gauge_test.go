package breaker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
)

type fakeBreakers struct {
	tripped    bool
	tripErr    error
	gotSample  store.BreakerSample
	trippedAt  time.Time
	isTrippedE error
}

func (f *fakeBreakers) Trip(_ context.Context, _, _ string, _ int, s store.BreakerSample) (time.Time, error) {
	f.gotSample = s
	return f.trippedAt, f.tripErr
}

func (f *fakeBreakers) IsTripped(context.Context, string, string) (bool, error) {
	return f.tripped, f.isTrippedE
}

func TestObserveState_SetsGaugeFromStore(t *testing.T) {
	f := &fakeBreakers{}
	gauge := metrics.CircuitBreakerTripped.WithLabelValues("gauge-observe", breaker.RulerDeletes)
	gauge.Set(1)
	if tripped, err := breaker.ObserveState(context.Background(), f, "gauge-observe", breaker.RulerDeletes); err != nil || tripped {
		t.Fatalf("ObserveState(untripped) = (%t, %v), want (false, nil)", tripped, err)
	}
	if got := promtest.ToFloat64(gauge); got != 0 {
		t.Errorf("gauge after untripped observation = %v, want 0", got)
	}
	f.tripped = true
	if tripped, err := breaker.ObserveState(context.Background(), f, "gauge-observe", breaker.RulerDeletes); err != nil || !tripped {
		t.Fatalf("ObserveState(tripped) = (%t, %v), want (true, nil)", tripped, err)
	}
	if got := promtest.ToFloat64(gauge); got != 1 {
		t.Errorf("gauge after tripped observation = %v, want 1", got)
	}
}

func TestTrip_SetsGaugeOnlyOnSuccessAndTruncatesSample(t *testing.T) {
	gauge := metrics.CircuitBreakerTripped.WithLabelValues("gauge-trip", breaker.ReconcileTotalLoss)
	gauge.Set(0)
	f := &fakeBreakers{tripErr: errors.New("boom")}
	if err := breaker.Trip(context.Background(), f, "gauge-trip", breaker.ReconcileTotalLoss, 0, breaker.Sample{Total: 1}); err == nil {
		t.Fatal("Trip must return the store error")
	}
	if got := promtest.ToFloat64(gauge); got != 0 {
		t.Errorf("gauge after failed trip = %v, want 0", got)
	}

	f.tripErr = nil
	sample := breaker.Sample{Total: breaker.MaxSampleSize + 5}
	for i := range breaker.MaxSampleSize + 5 {
		sample.Programs = append(sample.Programs, breaker.SampleProgram{ProgramID: int64(i)})
	}
	if err := breaker.Trip(context.Background(), f, "gauge-trip", breaker.ReconcileTotalLoss, 0, sample); err != nil {
		t.Fatalf("Trip: %v", err)
	}
	if got := promtest.ToFloat64(gauge); got != 1 {
		t.Errorf("gauge after successful trip = %v, want 1", got)
	}
	if len(f.gotSample.Programs) != 20 || f.gotSample.Total != breaker.MaxSampleSize+5 {
		t.Errorf("sample passed to store = %d programs, total %d; want 20 programs, total %d",
			len(f.gotSample.Programs), f.gotSample.Total, breaker.MaxSampleSize+5)
	}
}
