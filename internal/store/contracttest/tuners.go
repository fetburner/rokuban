package contracttest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/store"
)

// TunerFixture はチューナー store の実装と、その契約に必要な DB 状態を作る手段を持つ。
type TunerFixture struct {
	Sync     store.TunerSync
	Reader   store.TunerReader
	Capacity store.Capacity

	SeedTuner  func(t *testing.T, site string, tuner store.TunerInput, observedAt time.Time)
	SeedDemand func(t *testing.T, site string, programID int64, channelType, channel string,
		startAt time.Time, durationMs int64)
	SeedRule           func(t *testing.T, ruleID int64, enabled, dedupeEnabled bool)
	SeedPreviewProgram func(t *testing.T, ref store.CapacityProgramRef, channelType, channel string,
		startAt time.Time, durationMs int64)
}

// NewTunerStore はチューナー store の実装と fixture を作る。
type NewTunerStore func(t *testing.T) TunerFixture

// RunTuners はチューナー同期・一覧・容量読み取りの契約を実行する。
func RunTuners(t *testing.T, newStore NewTunerStore) {
	t.Helper()
	t.Run("sync steps and reader", func(t *testing.T) { runTunerSync(t, newStore) })
	t.Run("capacity reads preserve interval endpoints", func(t *testing.T) { runCapacityReads(t, newStore) })
	t.Run("preview returns rule state and candidates", func(t *testing.T) { runCapacityPreview(t, newStore) })
}

func runTunerSync(t *testing.T, newStore NewTunerStore) {
	f := newStore(t)
	ctx := context.Background()
	mark, err := f.Sync.SweepMark(ctx)
	if err != nil {
		t.Fatalf("SweepMark: %v", err)
	}
	if mark.IsZero() {
		t.Fatal("SweepMark returned zero time")
	}

	old := store.TunerInput{Index: 0, Name: "old", Types: []string{"GR"}, IsAvailable: true}
	boundary := store.TunerInput{Index: 1, Name: "boundary", Types: []string{"BS"}, IsAvailable: true}
	f.SeedTuner(t, "site-a", old, mark.Add(-time.Second))
	f.SeedTuner(t, "site-a", boundary, mark)
	f.SeedTuner(t, "site-b", old, mark.Add(-time.Second))

	fresh := store.TunerInput{Index: 2, Name: "fresh", Types: []string{"GR", "BS"}, IsFault: true}
	if err := f.Sync.Upsert(ctx, "site-a", []store.TunerInput{fresh}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	deleted, err := f.Sync.DeleteStale(ctx, "site-a", mark)
	if err != nil {
		t.Fatalf("DeleteStale: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteStale removed %d rows, want 1", deleted)
	}

	rows, err := f.Reader.List(ctx, "site-a")
	if err != nil {
		t.Fatalf("List(site-a): %v", err)
	}
	if len(rows) != 2 || rows[0].Index != 1 || rows[1].Index != 2 {
		t.Fatalf("List(site-a) = %+v, want boundary and fresh rows in index order", rows)
	}
	if !rows[0].ObservedAt.Equal(mark) {
		t.Errorf("boundary ObservedAt = %v, want exact cutoff %v to remain", rows[0].ObservedAt, mark)
	}
	if rows[1].Name != fresh.Name || !reflect.DeepEqual(rows[1].Types, fresh.Types) ||
		rows[1].IsAvailable != fresh.IsAvailable || rows[1].IsFault != fresh.IsFault || rows[1].ObservedAt.Before(mark) {
		t.Errorf("fresh tuner = %+v, want projected values and an observation at or after %v", rows[1], mark)
	}
	otherSite, err := f.Reader.List(ctx, "site-b")
	if err != nil {
		t.Fatalf("List(site-b): %v", err)
	}
	if len(otherSite) != 1 || otherSite[0].Name != old.Name {
		t.Errorf("List(site-b) = %+v, want its stale row untouched", otherSite)
	}
}

func runCapacityReads(t *testing.T, newStore NewTunerStore) {
	f := newStore(t)
	ctx := context.Background()
	start := time.Date(2026, time.January, 2, 3, 4, 5, 678000000, time.UTC)
	durationMs := int64(90_001)
	f.SeedDemand(t, "site-a", 7001, "GR", "27", start, durationMs)
	f.SeedDemand(t, "site-b", 7002, "BS", "BS1", start.Add(time.Hour), 60_000)
	f.SeedTuner(t, "site-a", store.TunerInput{Index: 0, Name: "site-a tuner", Types: []string{"GR"}, IsAvailable: false}, start)
	f.SeedTuner(t, "site-b", store.TunerInput{Index: 0, Name: "site-b tuner", Types: []string{"BS"}, IsFault: true}, start)

	got, err := f.Capacity.ReadSite(ctx, "site-a")
	if err != nil {
		t.Fatalf("ReadSite: %v", err)
	}
	if len(got.Demands) != 1 {
		t.Fatalf("ReadSite demands = %+v, want only site-a demand", got.Demands)
	}
	if demand := got.Demands[0]; demand.Site != "site-a" || demand.ChannelType != "GR" || demand.Channel != "27" ||
		!demand.StartAt.Equal(start) || !demand.EndAt.Equal(start.Add(time.Duration(durationMs)*time.Millisecond)) {
		t.Errorf("ReadSite demand = %+v, want exact half-open interval ending at start + %dms", demand, durationMs)
	}
	if len(got.Tuners) != 1 || got.Tuners[0].Site != "site-a" || got.Tuners[0].IsAvailable {
		t.Errorf("ReadSite tuners = %+v, want the unavailable site-a row retained", got.Tuners)
	}

	all, err := f.Capacity.ReadAllSites(ctx)
	if err != nil {
		t.Fatalf("ReadAllSites: %v", err)
	}
	if len(all.Demands) != 2 || len(all.Tuners) != 2 {
		t.Fatalf("ReadAllSites = %+v, want both site inputs", all)
	}
	if all.Demands[0].RuleID != nil || all.Demands[1].RuleID != nil {
		t.Errorf("ReadAllSites rule ids = (%v, %v), want nil for unruled reservations", all.Demands[0].RuleID, all.Demands[1].RuleID)
	}
}

func runCapacityPreview(t *testing.T, newStore NewTunerStore) {
	f := newStore(t)
	ctx := context.Background()
	ruleID := int64(7003)
	f.SeedRule(t, ruleID, false, true)
	start := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Millisecond)
	ref := store.CapacityProgramRef{Site: "site-a", ProgramID: 7004}
	f.SeedPreviewProgram(t, ref, "GR", "27", start, 30*60*1000)

	got, err := f.Capacity.PreviewCandidates(ctx, []store.CapacityProgramRef{ref}, &ruleID)
	if err != nil {
		t.Fatalf("PreviewCandidates: %v", err)
	}
	if got.Rule == nil || got.Rule.Enabled || !got.Rule.DedupeEnabled {
		t.Errorf("PreviewCandidates rule = %+v, want disabled and dedupe enabled", got.Rule)
	}
	if len(got.Candidates) != 1 {
		t.Fatalf("PreviewCandidates candidates = %+v, want one", got.Candidates)
	}
	candidate := got.Candidates[0]
	if candidate.Site != ref.Site || candidate.ProgramID != ref.ProgramID || candidate.ChannelType != "GR" ||
		candidate.Channel != "27" || !candidate.StartAt.Equal(start) || !candidate.EndAt.Equal(start.Add(30*time.Minute)) {
		t.Errorf("PreviewCandidates candidate = %+v, want seeded program", candidate)
	}

	missingID := ruleID + 1
	if _, err := f.Capacity.PreviewCandidates(ctx, nil, &missingID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PreviewCandidates(missing rule) error = %v, want ErrNotFound", err)
	}
}
