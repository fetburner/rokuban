package reconciler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/reconciler"
	"github.com/fetburner/rokuban/internal/testutil"
)

type mockMirakc struct {
	mu        sync.Mutex
	schedules map[int64]mirakc.Schedule
}

func newMockMirakc() *mockMirakc {
	return &mockMirakc{schedules: make(map[int64]mirakc.Schedule)}
}

func (m *mockMirakc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case r.Method == "GET" && r.URL.Path == "/api/recording/schedules":
		var list []mirakc.Schedule
		for _, s := range m.schedules {
			list = append(list, s)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)

	case r.Method == "POST" && r.URL.Path == "/api/recording/schedules":
		var input mirakc.ScheduleInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s := mirakc.Schedule{
			State: "scheduled",
			Program: mirakc.Program{
				ID:       input.ProgramID,
				EventID:  int(input.ProgramID % 100000),
				Duration: ptrInt64(300000),
			},
			Options: input.Options,
			Tags:    input.Tags,
		}
		m.schedules[input.ProgramID] = s
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(s)

	case r.Method == "DELETE" && len(r.URL.Path) > len("/api/recording/schedules/"):
		var programID int64
		_, _ = fmt.Sscanf(r.URL.Path, "/api/recording/schedules/%d", &programID)
		delete(m.schedules, programID)
		w.WriteHeader(200)

	default:
		http.Error(w, "not found", 404)
	}
}

func (m *mockMirakc) getSchedules() map[int64]mirakc.Schedule {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[int64]mirakc.Schedule, len(m.schedules))
	for k, v := range m.schedules {
		cp[k] = v
	}
	return cp
}

func ptrInt64(v int64) *int64 { return &v }

func TestReconciler_CreatesSchedule(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	res, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:              "default",
		ProgramID:         100000500011234,
		Overrides:         json.RawMessage(`{}`),
		Title:             "テスト番組",
		ProgramStartAt:    startAt,
		ProgramDurationMs: 1800000,
	})
	if err != nil {
		t.Fatalf("creating reservation: %v", err)
	}

	rec := reconciler.New("default", mc, pool, &reconciler.Config{
		ReconcileInterval: time.Hour,
	})

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() { _ = rec.Run(runCtx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()

	schedules := mock.getSchedules()
	if len(schedules) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(schedules))
	}

	s, ok := schedules[100000500011234]
	if !ok {
		t.Fatal("schedule not found for programID 100000500011234")
	}

	resID, found := mirakc.FindReservationID(s.Tags)
	if !found {
		t.Fatal("reservation tag not found in schedule tags")
	}
	if resID != res.ID {
		t.Errorf("tag reservation ID = %d, want %d", resID, res.ID)
	}
	if s.Options.ContentPath == nil || *s.Options.ContentPath == "" {
		t.Error("contentPath is empty")
	}
}

func TestReconciler_DeletesOrphanedSchedule(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	mock.schedules[999] = mirakc.Schedule{
		State:   "scheduled",
		Program: mirakc.Program{ID: 999},
		Tags:    []string{"rokuban:reservation=9999"},
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)

	rec := reconciler.New("default", mc, pool, &reconciler.Config{
		ReconcileInterval: time.Hour,
		MaxDeletesPerPass: 10,
	})

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() { _ = rec.Run(runCtx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()

	schedules := mock.getSchedules()
	if len(schedules) != 0 {
		t.Errorf("expected 0 schedules after cleanup, got %d", len(schedules))
	}
}

func TestReconciler_CircuitBreaker(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	for i := int64(1); i <= 20; i++ {
		mock.schedules[i] = mirakc.Schedule{
			State:   "scheduled",
			Program: mirakc.Program{ID: i},
			Tags:    []string{fmt.Sprintf("rokuban:reservation=%d", i)},
		}
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)

	rec := reconciler.New("default", mc, pool, &reconciler.Config{
		ReconcileInterval: time.Hour,
		MaxDeletesPerPass: 5,
	})

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() { _ = rec.Run(runCtx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()

	schedules := mock.getSchedules()
	if len(schedules) != 20 {
		t.Errorf("circuit breaker should prevent deletion, got %d schedules (expected 20)", len(schedules))
	}
}

func TestReconciler_IgnoresNonRokubanSchedules(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	mock.schedules[888] = mirakc.Schedule{
		State:   "scheduled",
		Program: mirakc.Program{ID: 888},
		Tags:    []string{"epgstation:rule=42"},
	}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)

	rec := reconciler.New("default", mc, pool, &reconciler.Config{
		ReconcileInterval: time.Hour,
	})

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() { _ = rec.Run(runCtx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()

	schedules := mock.getSchedules()
	if len(schedules) != 1 {
		t.Errorf("non-rokuban schedule should not be deleted, got %d", len(schedules))
	}
}

func TestReconciler_SkippedReservationNotScheduled(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	mock := newMockMirakc()
	srv := httptest.NewServer(mock)
	defer srv.Close()

	mc := mirakc.NewClient(srv.URL, nil)
	q := sqlcgen.New(pool)

	startAt := time.Now().Add(1 * time.Hour)
	_, err := q.CreateManualReservation(ctx, sqlcgen.CreateManualReservationParams{
		Site:              "default",
		ProgramID:         200000500011234,
		Overrides:         json.RawMessage(`{"skip":true}`),
		Title:             "スキップ番組",
		ProgramStartAt:    startAt,
		ProgramDurationMs: 1800000,
	})
	if err != nil {
		t.Fatalf("creating reservation: %v", err)
	}

	rec := reconciler.New("default", mc, pool, &reconciler.Config{
		ReconcileInterval: time.Hour,
	})

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() { _ = rec.Run(runCtx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()

	schedules := mock.getSchedules()
	if len(schedules) != 0 {
		t.Errorf("skipped reservation should not create schedule, got %d", len(schedules))
	}
}
