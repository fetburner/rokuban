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

// ReservationProgramOptions は、予約の契約テストの 1 ケースが必要とする永続状態を表す。
type ReservationProgramOptions struct {
	Projection  bool
	Snapshot    bool
	Reservation bool
	Overrides   json.RawMessage
	StartAt     time.Time
	// Site は番組の site で、この番組のすべての行に使う。空なら store.DefaultSite にする。
	Site string
	// Base は予約行の reservations.base に入れる値。Reservation が true のときだけ使える。
	// nil なら NULL のまま。
	Base json.RawMessage
	// NeverScheduled は、この番組の放送イベントを欠測（never_scheduled_events）として仕込む。
	// Snapshot が true のときだけ使える。欠測の判定に使う program_start_at は snapshot の開始時刻。
	NeverScheduled bool
}

// NewReservationProgram は番組を仕込み、その site と安定した番組 ID を返す。
type NewReservationProgram func(t *testing.T, options ReservationProgramOptions) (string, int64)

// ReservationsFixture は予約契約の実装と、実装側が与える状態の仕込み・同期のフックをまとめる。
type ReservationsFixture struct {
	Store store.Reservations
	// Pass は GetRulerInputRevision で ruler 入力の版を読むために使う。
	Pass       store.RulerPass
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
	t.Run("intent and override writes advance the ruler input revision", func(t *testing.T) {
		runReservationRevisions(t, newStore)
	})
	t.Run("reservation reads return details and overlaps", func(t *testing.T) {
		runReservationReads(t, newStore)
	})
	t.Run("patch rejects invalid effective options and leaves state unchanged", func(t *testing.T) {
		runPatchInvalidEffective(t, newStore)
	})
	t.Run("list reservations groups by site and orders by start time", func(t *testing.T) {
		runListReservationsOrder(t, newStore)
	})
	t.Run("list overlaps uses half-open windows and excludes self and missing events", func(t *testing.T) {
		runListOverlapsWindow(t, newStore)
	})
	t.Run("list overlaps rejects a program outside the projection", func(t *testing.T) {
		runListOverlapsNotFound(t, newStore)
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

// runReservationRevisions は、意図と上書きの 4 つの書き込みが、それぞれ対象 site の版を 1 進めることを確かめる。
func runReservationRevisions(t *testing.T, newStore NewReservationsStore) {
	f := newStore(t)
	if f.Pass == nil {
		t.Fatal("reservations fixture needs Pass")
	}
	ctx := context.Background()
	site, programID := f.NewProgram(t, ReservationProgramOptions{Projection: true, Snapshot: true, Reservation: true})
	priority := 3
	for _, c := range []struct {
		name string
		do   func() error
	}{
		{"PutIntent", func() error { return f.Store.PutIntent(ctx, site, programID, reservation.IntentRecord) }},
		{"PatchOverrides", func() error {
			return f.Store.PatchOverrides(ctx, site, programID, reservation.OverridesPatch{Priority: &priority})
		}},
		{"DeleteOverrides", func() error { return f.Store.DeleteOverrides(ctx, site, programID) }},
		{"DeleteIntent", func() error { return f.Store.DeleteIntent(ctx, site, programID) }},
	} {
		before := currentRulerInputRevision(t, f.Pass, site)
		if err := c.do(); err != nil {
			t.Fatalf("%s = %v", c.name, err)
		}
		if got := currentRulerInputRevision(t, f.Pass, site) - before; got != 1 {
			t.Errorf("%s: ruler input revision delta = %d, want 1", c.name, got)
		}
	}
}

// runPatchInvalidEffective は、マージ後の実効値が不正な PATCH が InvalidEffectiveOptionsError で失敗し、
// 既存の上書きと ruler 入力の版を変えないことを確かめる。成功する対照を 2 つ置き、
// base の profiles を無視して検証する実装を検出する。
func runPatchInvalidEffective(t *testing.T, newStore NewReservationsStore) {
	keepUntilEncoded := reservation.KeepOriginalUntilEncoded
	emptyProfiles := []string{}
	h264Overrides := json.RawMessage(`{"keepOriginal":"until_encoded","encodeProfiles":["h264"]}`)
	for _, c := range []struct {
		name      string
		options   ReservationProgramOptions
		patch     reservation.OverridesPatch
		invalid   bool
		wantAfter json.RawMessage // wantAfter は、成功するケースでのパッチ後の上書き。nil は上書きの行が無いことを表す。
	}{
		{
			name:    "no base and no overrides",
			options: ReservationProgramOptions{Projection: true, Snapshot: true, Reservation: true},
			patch:   reservation.OverridesPatch{KeepOriginal: &keepUntilEncoded},
			invalid: true,
		},
		{
			name: "reset of encode profiles with existing keep original",
			options: ReservationProgramOptions{
				Projection: true, Snapshot: true, Reservation: true, Overrides: h264Overrides,
			},
			patch:   reservation.OverridesPatch{Reset: []reservation.OverrideField{reservation.OverrideEncodeProfiles}},
			invalid: true,
		},
		{
			name: "explicit empty encode profiles with existing keep original",
			options: ReservationProgramOptions{
				Projection: true, Snapshot: true, Reservation: true, Overrides: h264Overrides,
			},
			patch:   reservation.OverridesPatch{EncodeProfiles: &emptyProfiles},
			invalid: true,
		},
		{
			name: "base encode profiles satisfy a keep original patch",
			options: ReservationProgramOptions{
				Projection: true, Snapshot: true, Reservation: true,
				Base: json.RawMessage(`{"encodeProfiles":["h264"]}`),
			},
			patch:     reservation.OverridesPatch{KeepOriginal: &keepUntilEncoded},
			wantAfter: json.RawMessage(`{"keepOriginal":"until_encoded"}`),
		},
		{
			name: "reset of encode profiles lets the base profiles take effect",
			options: ReservationProgramOptions{
				Projection: true, Snapshot: true, Reservation: true,
				Base:      json.RawMessage(`{"keepOriginal":"until_encoded","encodeProfiles":["h264"]}`),
				Overrides: json.RawMessage(`{"encodeProfiles":[]}`),
			},
			patch: reservation.OverridesPatch{Reset: []reservation.OverrideField{reservation.OverrideEncodeProfiles}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			runPatchInvalidEffectiveCase(t, newStore, c.options, c.patch, c.invalid, c.wantAfter)
		})
	}
}

func runPatchInvalidEffectiveCase(t *testing.T, newStore NewReservationsStore, options ReservationProgramOptions,
	patch reservation.OverridesPatch, invalid bool, wantAfter json.RawMessage) {
	f := newStore(t)
	if f.Pass == nil {
		t.Fatal("reservations fixture needs Pass")
	}
	ctx := context.Background()
	site, programID := f.NewProgram(t, options)
	before, err := f.Store.GetReservation(ctx, site, programID)
	if err != nil {
		t.Fatalf("GetReservation(before patch) = %v", err)
	}
	revisionBefore := currentRulerInputRevision(t, f.Pass, site)

	err = f.Store.PatchOverrides(ctx, site, programID, patch)
	var invalidErr *reservation.InvalidEffectiveOptionsError
	if invalid {
		if !errors.As(err, &invalidErr) {
			t.Fatalf("PatchOverrides() = %v, want *reservation.InvalidEffectiveOptionsError", err)
		}
	} else if err != nil {
		t.Fatalf("PatchOverrides() = %v, want success", err)
	}

	after, err := f.Store.GetReservation(ctx, site, programID)
	if err != nil {
		t.Fatalf("GetReservation(after patch) = %v", err)
	}
	want := decodeOverridesForCompare(t, wantAfter)
	if invalid {
		want = decodeOverridesForCompare(t, before.Overrides)
	}
	if got := decodeOverridesForCompare(t, after.Overrides); !reflect.DeepEqual(got, want) {
		t.Fatalf("overrides after patch = %s, want %s", after.Overrides, describeOverrides(want))
	}
	delta := currentRulerInputRevision(t, f.Pass, site) - revisionBefore
	if invalid && delta != 0 {
		t.Errorf("ruler input revision delta after rejected patch = %d, want 0", delta)
	}
	if !invalid && delta != 1 {
		t.Errorf("ruler input revision delta after accepted patch = %d, want 1", delta)
	}
}

// decodeOverridesForCompare は、JSON の整形（空白・キーの並び）に依らず上書きを比べられるよう、JSON を値に decode する。
// 空（行が無い）なら nil を返す。
func decodeOverridesForCompare(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding overrides %s: %v", raw, err)
	}
	return v
}

func describeOverrides(v any) string {
	if v == nil {
		return "no row"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// runListReservationsOrder は、ListReservations が site ごとにまとまり、各 site 内で開始時刻の昇順に返すことを確かめる。
// 挿入は開始時刻の降順にして、挿入順をそのまま返す実装でテストが失敗するようにする。site 間の並びと同時刻の並びは主張しない。
func runListReservationsOrder(t *testing.T, newStore NewReservationsStore) {
	f := newStore(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	seeds := []struct {
		site   string
		offset time.Duration
	}{
		{"beta", 4 * time.Hour},
		{"alpha", 3 * time.Hour},
		{"beta", 2 * time.Hour},
		{"beta", 2 * time.Hour},
		{"alpha", time.Hour},
	}
	startOf := map[int64]time.Time{}
	for _, s := range seeds {
		_, programID := f.NewProgram(t, ReservationProgramOptions{
			Site: s.site, Snapshot: true, Reservation: true, StartAt: start.Add(s.offset),
		})
		startOf[programID] = start.Add(s.offset)
	}

	rows, err := f.Store.ListReservations(ctx)
	if err != nil {
		t.Fatalf("ListReservations() = %v", err)
	}
	if len(rows) != len(seeds) {
		t.Fatalf("ListReservations() returned %d rows, want %d", len(rows), len(seeds))
	}
	closed := map[string]bool{}
	current := ""
	var lastStart time.Time
	seen := map[int64]bool{}
	for i, row := range rows {
		id := row.Reservation.ProgramID
		wantStart, ok := startOf[id]
		if !ok || seen[id] {
			t.Fatalf("ListReservations() row %d program %d is not a distinct seeded program", i, id)
		}
		seen[id] = true
		if !row.Snapshot.StartAt.Equal(wantStart) {
			t.Fatalf("ListReservations() row %d start = %s, want %s", i, row.Snapshot.StartAt, wantStart)
		}
		site := row.Reservation.Site
		if site != current {
			if closed[site] {
				t.Fatalf("ListReservations() site %q appears again at row %d; rows of one site must be contiguous", site, i)
			}
			if current != "" {
				closed[current] = true
			}
			current = site
			lastStart = time.Time{}
		}
		if row.Snapshot.StartAt.Before(lastStart) {
			t.Fatalf("ListReservations() row %d starts at %s after %s within site %q", i, row.Snapshot.StartAt, lastStart, site)
		}
		lastStart = row.Snapshot.StartAt
	}
}

// runListOverlapsWindow は、ListOverlaps が放送時間帯の半開区間で重なる予約だけを返し、
// 対象自身・欠測・別 site を除くことを確かめる。並びは主張しない。
func runListOverlapsWindow(t *testing.T, newStore NewReservationsStore) {
	f := newStore(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	targetSite, targetID := f.NewProgram(t, ReservationProgramOptions{
		Projection: true, Snapshot: true, Reservation: true, StartAt: start,
	})
	candidate := func(opts ReservationProgramOptions) int64 {
		t.Helper()
		opts.Snapshot, opts.Reservation = true, true
		_, id := f.NewProgram(t, opts)
		return id
	}

	// included は重なるので返る予約。
	included := map[int64]string{}
	included[candidate(ReservationProgramOptions{Site: targetSite, StartAt: start.Add(-30*time.Minute + time.Second)})] = "starts 30m-1s before the target"
	included[candidate(ReservationProgramOptions{Site: targetSite, StartAt: start})] = "starts with the target"
	included[candidate(ReservationProgramOptions{Site: targetSite, StartAt: start.Add(30*time.Minute - time.Second)})] = "starts 1s before the target end"
	skipID := candidate(ReservationProgramOptions{Site: targetSite, StartAt: start.Add(5 * time.Minute)})
	if err := f.Store.PutIntent(ctx, targetSite, skipID, reservation.IntentSkip); err != nil {
		t.Fatalf("PutIntent(skip candidate) = %v", err)
	}
	included[skipID] = "skip intent inside the window"

	// excluded は返ってはいけない予約。
	excluded := map[int64]string{}
	excluded[candidate(ReservationProgramOptions{Site: targetSite, StartAt: start.Add(-30 * time.Minute)})] = "ends exactly at the target start"
	excluded[candidate(ReservationProgramOptions{Site: targetSite, StartAt: start.Add(30 * time.Minute)})] = "starts exactly at the target end"
	excluded[candidate(ReservationProgramOptions{Site: targetSite, StartAt: start.Add(10 * time.Minute), NeverScheduled: true})] = "never scheduled inside the window"
	excluded[candidate(ReservationProgramOptions{Site: "gamma", StartAt: start.Add(10 * time.Minute)})] = "other site inside the window"
	excluded[targetID] = "the target itself"

	overlaps, err := f.Store.ListOverlaps(ctx, targetSite, targetID)
	if err != nil {
		t.Fatalf("ListOverlaps() = %v", err)
	}
	if len(overlaps) != len(included) {
		t.Fatalf("ListOverlaps() returned %d rows, want %d: %+v", len(overlaps), len(included), overlaps)
	}
	seen := map[int64]bool{}
	for _, o := range overlaps {
		if reason, ok := excluded[o.ProgramID]; ok {
			t.Fatalf("ListOverlaps() includes program %d (%s), want it excluded", o.ProgramID, reason)
		}
		if _, ok := included[o.ProgramID]; !ok || seen[o.ProgramID] {
			t.Fatalf("ListOverlaps() includes unexpected or duplicate program %d", o.ProgramID)
		}
		seen[o.ProgramID] = true
		if o.ProgramID == skipID && (o.IntentAction == nil || *o.IntentAction != reservation.IntentSkip) {
			t.Fatalf("ListOverlaps() skip candidate intent = %v, want %q (the caller decides skip)", o.IntentAction, reservation.IntentSkip)
		}
	}
}

// runListOverlapsNotFound は、射影に番組が無い対象で ListOverlaps が ErrNotFound を返すことを確かめる。
// 番組が何も無い場合と、凍結スナップショットだけが残る場合の両方で確かめる。
func runListOverlapsNotFound(t *testing.T, newStore NewReservationsStore) {
	f := newStore(t)
	ctx := context.Background()
	missingSite, missingID := f.NewProgram(t, ReservationProgramOptions{})
	if _, err := f.Store.ListOverlaps(ctx, missingSite, missingID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ListOverlaps(no program) = %v, want ErrNotFound", err)
	}
	frozenSite, frozenID := f.NewProgram(t, ReservationProgramOptions{Snapshot: true, Reservation: true})
	if _, err := f.Store.ListOverlaps(ctx, frozenSite, frozenID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ListOverlaps(frozen snapshot only) = %v, want ErrNotFound", err)
	}
}
