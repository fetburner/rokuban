package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// ReservationProgramOptions は、予約の契約テストの 1 ケースが必要とする永続状態を表す。
type ReservationProgramOptions struct {
	Projection  bool
	Snapshot    bool
	Reservation bool
	Overrides   json.RawMessage
	StartAt     time.Time
}

// NewReservationProgram は番組を仕込み、その site と安定した番組 ID を返す。
type NewReservationProgram func(t *testing.T, options ReservationProgramOptions) (string, int64)

// ReservationsFixture は予約契約の実装と、実装側が与える状態の仕込み・同期のフックをまとめる。
type ReservationsFixture struct {
	Store      store.Reservations
	NewProgram NewReservationProgram
	// HoldProgram は、その番組への PutIntent / PatchOverrides の直列化点を外から握り、
	// release で放す。握っている間、同じ番組への書き込みは直列化点で待つ。
	// テストが失敗で終わっても放されるよう、実装は t.Cleanup でも release する。
	HoldProgram func(t *testing.T, site string, programID int64) (release func())
	// WaitBlocked は、n 本の書き込みが HoldProgram の握る直列化点で待ち始めるまで待つ。
	// n 本に達しなければ t を失敗させる。
	WaitBlocked func(t *testing.T, n int)
}

// NewReservationsStore は予約契約の実装とフィクスチャを作る。
type NewReservationsStore func(t *testing.T) ReservationsFixture

// RunReservations は予約の契約を実装に対して実行する。
func RunReservations(t *testing.T, newStore NewReservationsStore) {
	t.Helper()
	t.Run("intent snapshots projection and rejects missing program", func(t *testing.T) {
		runIntentProjection(t, newStore)
	})
	t.Run("empty overrides delete their row", func(t *testing.T) {
		runEmptyOverrides(t, newStore)
	})
	t.Run("patch rejects a missing program", func(t *testing.T) {
		runPatchMissingProgram(t, newStore)
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
	if err := f.Store.PatchOverrides(ctx, site, programID, reservation.OverridesPatch{
		Reset: []reservation.OverrideField{reservation.OverridePriority},
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

func runPatchMissingProgram(t *testing.T, newStore NewReservationsStore) {
	f := newStore(t)
	site, programID := f.NewProgram(t, ReservationProgramOptions{})
	priority := 1
	if err := f.Store.PatchOverrides(context.Background(), site, programID, reservation.OverridesPatch{Priority: &priority}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PatchOverrides(missing program) = %v, want ErrNotFound", err)
	}
}

// runConcurrentOverrides は、既存の上書きがある番組に別々のフィールドを同時に PATCH しても、
// 両方の値と既存の値が残ることを確かめる。直列化点を握った状態で 2 本を待たせ、放してから
// 結果を見るので、既存の上書きを直列化の前に読む実装は決定的に値を失う。
// 射影にある番組と、射影から外れて凍結スナップショットだけが残る番組の両方で行う。
func runConcurrentOverrides(t *testing.T, newStore NewReservationsStore) {
	for _, c := range []struct {
		name       string
		projection bool
	}{
		{"projected program", true},
		{"frozen snapshot", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			runConcurrentOverridesCase(t, newStore, c.projection)
		})
	}
}

func runConcurrentOverridesCase(t *testing.T, newStore NewReservationsStore, projection bool) {
	f := newStore(t)
	ctx := context.Background()
	site, programID := f.NewProgram(t, ReservationProgramOptions{
		Projection: projection, Snapshot: true, Reservation: true, Overrides: json.RawMessage(`{"priority":7}`),
	})
	contentPath := "recordings/concurrent.ts"
	filenameTemplate := "{{.Title}}"
	patches := []reservation.OverridesPatch{
		{ContentPath: &contentPath},
		{FilenameTemplate: &filenameTemplate},
	}

	release := f.HoldProgram(t, site, programID)
	results := make(chan error, len(patches))
	for _, patch := range patches {
		go func() { results <- f.Store.PatchOverrides(ctx, site, programID, patch) }()
	}
	f.WaitBlocked(t, len(patches))
	release()
	for range patches {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent PatchOverrides() = %v, want success", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent PatchOverrides() did not finish after the serialization point was released")
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
		!hasPriorityOverride(overlaps[0].Overrides, 3) {
		t.Fatalf("ListOverlaps() = %+v, want one overlapping reservation with its override", overlaps)
	}
}

func hasPriorityOverride(raw json.RawMessage, want int) bool {
	var options reservation.Options
	if err := json.Unmarshal(raw, &options); err != nil || options.Priority == nil {
		return false
	}
	return *options.Priority == want
}
