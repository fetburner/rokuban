package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// ReservationProgramOptions describes persistent state needed by a reservation contract case.
type ReservationProgramOptions struct {
	Projection  bool
	Snapshot    bool
	Reservation bool
	Overrides   json.RawMessage
	StartAt     time.Time
}

// NewReservationProgram seeds a program and returns its site and stable program ID.
type NewReservationProgram func(t *testing.T, options ReservationProgramOptions) (string, int64)

// ReservationsFixture contains a store and the implementation-specific state seeder.
type ReservationsFixture struct {
	Store      store.Reservations
	NewProgram NewReservationProgram
}

// NewReservationsStore creates the store and fixture for a reservation contract case.
type NewReservationsStore func(t *testing.T) ReservationsFixture

// RunReservationsContract checks the reservation store contract against a real implementation.
func RunReservationsContract(t *testing.T, newStore NewReservationsStore) {
	t.Helper()
	t.Run("intent snapshots projection and rejects missing program", func(t *testing.T) {
		runIntentProjection(t, newStore)
	})
	t.Run("empty overrides delete their row", func(t *testing.T) {
		runEmptyOverrides(t, newStore)
	})
	t.Run("concurrent field patches preserve both values", func(t *testing.T) {
		runConcurrentOverrides(t, newStore)
	})
	t.Run("reservation reads return details and overlaps", func(t *testing.T) {
		runReservationReads(t, newStore)
	})
}

func runIntentProjection(t *testing.T, newStore NewReservationsStore) {
	f := newStore(t)
	ctx := context.Background()
	site, programID := f.NewProgram(t, ReservationProgramOptions{
		Projection: true, Snapshot: true, Reservation: true,
	})
	if err := f.Store.PutIntent(ctx, site, programID, reservation.IntentRecord); err != nil {
		t.Fatalf("PutIntent(projected program) = %v, want success", err)
	}
	row, err := f.Store.GetReservation(ctx, site, programID)
	if err != nil {
		t.Fatalf("GetReservation(after PutIntent) = %v", err)
	}
	if row.IntentAction == nil || *row.IntentAction != reservation.IntentRecord || row.Snapshot.Title != "store contract program" {
		t.Fatalf("reservation after PutIntent = %+v, want projection snapshot and record intent", row)
	}

	frozenStartAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	frozenSite, frozenID := f.NewProgram(t, ReservationProgramOptions{
		Snapshot: true, Reservation: true, StartAt: frozenStartAt,
	})
	if err := f.Store.PutIntent(ctx, frozenSite, frozenID, reservation.IntentSkip); err != nil {
		t.Fatalf("PutIntent(frozen snapshot) = %v, want success", err)
	}
	frozenRow, err := f.Store.GetReservation(ctx, frozenSite, frozenID)
	if err != nil {
		t.Fatalf("GetReservation(after PutIntent on frozen snapshot) = %v", err)
	}
	if frozenRow.IntentAction == nil || *frozenRow.IntentAction != reservation.IntentSkip ||
		frozenRow.Snapshot.Title != "store contract program" || !frozenRow.Snapshot.StartAt.Equal(frozenStartAt) ||
		frozenRow.Snapshot.DurationMs != 1800000 {
		t.Fatalf("reservation after PutIntent(frozen snapshot) = %+v, want skip intent and unchanged snapshot", frozenRow)
	}

	missingSite, missingID := f.NewProgram(t, ReservationProgramOptions{})
	if err := f.Store.PutIntent(ctx, missingSite, missingID, reservation.IntentRecord); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PutIntent(missing program) = %v, want ErrNotFound", err)
	}
}

func runEmptyOverrides(t *testing.T, newStore NewReservationsStore) {
	f := newStore(t)
	ctx := context.Background()
	site, programID := f.NewProgram(t, ReservationProgramOptions{
		Projection: true, Snapshot: true, Reservation: true, Overrides: json.RawMessage(`{"priority":7}`),
	})
	if err := f.Store.PatchOverrides(ctx, site, programID, store.OverridesPatch{
		Reset: []store.OverrideField{store.OverridePriority},
	}); err != nil {
		t.Fatalf("PatchOverrides(reset last field) = %v, want success", err)
	}
	row, err := f.Store.GetReservation(ctx, site, programID)
	if err != nil {
		t.Fatalf("GetReservation after reset: %v", err)
	}
	if len(row.Overrides) != 0 {
		t.Fatalf("overrides after reset = %s, want no row", row.Overrides)
	}
}

func runConcurrentOverrides(t *testing.T, newStore NewReservationsStore) {
	for range 20 {
		runConcurrentOverridesRound(t, newStore)
	}
}

func runConcurrentOverridesRound(t *testing.T, newStore NewReservationsStore) {
	f := newStore(t)
	ctx := context.Background()
	site, programID := f.NewProgram(t, ReservationProgramOptions{
		Projection: true, Snapshot: true, Reservation: true, Overrides: json.RawMessage(`{"priority":7}`),
	})
	contentPath := "recordings/concurrent.ts"
	filenameTemplate := "{{.Title}}"
	patches := []store.OverridesPatch{
		{ContentPath: &contentPath},
		{FilenameTemplate: &filenameTemplate},
	}
	start := make(chan struct{})
	results := make(chan error, len(patches))
	for _, patch := range patches {
		patch := patch
		go func() {
			<-start
			results <- f.Store.PatchOverrides(ctx, site, programID, patch)
		}()
	}
	close(start)
	for range patches {
		if err := <-results; err != nil {
			t.Fatalf("concurrent PatchOverrides() = %v, want success", err)
		}
	}
	row, err := f.Store.GetReservation(ctx, site, programID)
	if err != nil {
		t.Fatalf("GetReservation after concurrent patches: %v", err)
	}
	var got reservation.Options
	if err := json.Unmarshal(row.Overrides, &got); err != nil {
		t.Fatalf("decoding merged overrides %s: %v", row.Overrides, err)
	}
	if got.Priority == nil || *got.Priority != 7 || got.ContentPath == nil || *got.ContentPath != contentPath ||
		got.FilenameTemplate == nil || *got.FilenameTemplate != filenameTemplate {
		t.Fatalf("merged overrides = %+v, want priority and both concurrent fields", got)
	}
}

func runReservationReads(t *testing.T, newStore NewReservationsStore) {
	f := newStore(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	targetSite, targetID := f.NewProgram(t, ReservationProgramOptions{Projection: true, StartAt: start})
	reservationSite, reservationID := f.NewProgram(t, ReservationProgramOptions{
		Projection: true, Snapshot: true, Reservation: true,
		Overrides: json.RawMessage(`{"priority":3}`), StartAt: start.Add(20 * time.Minute),
	})
	if targetSite != reservationSite {
		t.Fatalf("fixture sites = %q and %q, want the same site", targetSite, reservationSite)
	}
	if err := f.Store.PutIntent(ctx, reservationSite, reservationID, reservation.IntentRecord); err != nil {
		t.Fatalf("PutIntent(read fixture) = %v", err)
	}

	got, err := f.Store.GetReservation(ctx, reservationSite, reservationID)
	if err != nil {
		t.Fatalf("GetReservation() = %v", err)
	}
	if got.Reservation.ProgramID != reservationID || got.Snapshot.Title == "" || len(got.Overrides) == 0 ||
		got.IntentAction == nil || *got.IntentAction != reservation.IntentRecord {
		t.Fatalf("GetReservation() = %+v, want snapshot, override, and intent", got)
	}
	rows, err := f.Store.ListReservations(ctx)
	if err != nil {
		t.Fatalf("ListReservations() = %v", err)
	}
	if len(rows) != 1 || rows[0].Reservation.ProgramID != reservationID {
		t.Fatalf("ListReservations() = %+v, want the seeded reservation", rows)
	}
	overlaps, err := f.Store.ListOverlaps(ctx, targetSite, targetID)
	if err != nil {
		t.Fatalf("ListOverlaps() = %v", err)
	}
	if len(overlaps) != 1 || overlaps[0].ProgramID != reservationID ||
		!reflect.DeepEqual(overlaps[0].Overrides, json.RawMessage(`{"priority":3}`)) {
		t.Fatalf("ListOverlaps() = %+v, want one overlapping reservation with its override", overlaps)
	}
}
