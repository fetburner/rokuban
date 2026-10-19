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
	SeedDemand func(t *testing.T, site string, programID int64, ruleID *int64, channelType, channel string,
		startAt time.Time, durationMs int64)
	SeedRule           func(t *testing.T, enabled, dedupeEnabled bool) int64
	SeedPreviewProgram func(t *testing.T, ref store.CapacityProgramRef, channelType, channel string,
		startAt time.Time, durationMs int64)
}

// NewTunerStore はチューナー store の実装と fixture を作る。
type NewTunerStore func(t *testing.T) TunerFixture

// RunTuners はチューナー同期・一覧・容量読み取りの契約を実行する。
func RunTuners(t *testing.T, newStore NewTunerStore) {
	t.Helper()
	t.Run("sync steps and reader", func(t *testing.T) { runTunerSync(t, newStore) })
	t.Run("upsert batch is atomic", func(t *testing.T) { runTunerUpsertAtomicity(t, newStore) })
	t.Run("capacity reads preserve interval endpoints", func(t *testing.T) { runCapacityReads(t, newStore) })
	t.Run("preview rule", func(t *testing.T) { runCapacityPreviewRule(t, newStore) })
	t.Run("preview candidates", func(t *testing.T) { runCapacityPreviewCandidates(t, newStore) })
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
	updated := store.TunerInput{Index: fresh.Index, Name: "updated", Types: []string{"BS"}, IsAvailable: false}
	if err := f.Sync.Upsert(ctx, "site-a", []store.TunerInput{updated}); err != nil {
		t.Fatalf("Upsert update: %v", err)
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
	if rows[1].Name != updated.Name || !reflect.DeepEqual(rows[1].Types, updated.Types) ||
		rows[1].IsAvailable != updated.IsAvailable || rows[1].IsFault != updated.IsFault || rows[1].ObservedAt.Before(mark) {
		t.Errorf("updated tuner = %+v, want latest projected values and an observation at or after %v", rows[1], mark)
	}
	otherSite, err := f.Reader.List(ctx, "site-b")
	if err != nil {
		t.Fatalf("List(site-b): %v", err)
	}
	if len(otherSite) != 1 || otherSite[0].Name != old.Name {
		t.Errorf("List(site-b) = %+v, want its stale row untouched", otherSite)
	}
}

func runTunerUpsertAtomicity(t *testing.T, newStore NewTunerStore) {
	f := newStore(t)
	ctx := context.Background()
	rows := []store.TunerInput{
		{Index: 0, Name: "valid", Types: []string{"GR"}, IsAvailable: true},
		{Index: 1, Name: "invalid", Types: []string{"INVALID"}, IsAvailable: true},
	}
	if err := f.Sync.Upsert(ctx, "site-a", rows); err == nil {
		t.Fatal("Upsert returned nil for a batch with an invalid tuner type")
	}
	got, err := f.Reader.List(ctx, "site-a")
	if err != nil {
		t.Fatalf("List after failed Upsert: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List after failed Upsert = %+v, want no partially applied rows", got)
	}
}

func runCapacityReads(t *testing.T, newStore NewTunerStore) {
	f := newStore(t)
	ctx := context.Background()
	start := time.Date(2026, time.January, 2, 3, 4, 5, 678000000, time.UTC)
	durationMs := int64(90_001)
	ruleID := f.SeedRule(t, true, false)
	f.SeedDemand(t, "site-a", 7001, &ruleID, "GR", "27", start, durationMs)
	f.SeedDemand(t, "site-a", 7003, nil, "GR", "27", start.Add(2*time.Hour), 60_000)
	f.SeedDemand(t, "site-b", 7002, nil, "BS", "BS1", start.Add(time.Hour), 60_000)
	f.SeedTuner(t, "site-a", store.TunerInput{Index: 0, Name: "site-a tuner", Types: []string{"GR"}, IsAvailable: false}, start)
	f.SeedTuner(t, "site-b", store.TunerInput{Index: 0, Name: "site-b tuner", Types: []string{"BS"}, IsFault: true}, start)

	got, err := f.Capacity.ReadSite(ctx, "site-a")
	if err != nil {
		t.Fatalf("ReadSite: %v", err)
	}
	if len(got.Demands) != 2 {
		t.Fatalf("ReadSite demands = %+v, want only the two site-a demands", got.Demands)
	}
	if demand := got.Demands[0]; demand.Site != "site-a" || demand.ChannelType != "GR" || demand.Channel != "27" ||
		!demand.StartAt.Equal(start) || !demand.EndAt.Equal(start.Add(time.Duration(durationMs)*time.Millisecond)) {
		t.Errorf("ReadSite demand = %+v, want exact half-open interval ending at start + %dms", demand, durationMs)
	}
	if got.Demands[0].RuleID == nil || *got.Demands[0].RuleID != ruleID {
		t.Errorf("ReadSite rule id = %v, want %d", got.Demands[0].RuleID, ruleID)
	}
	if got.Demands[1].RuleID != nil {
		t.Errorf("ReadSite unruled rule id = %v, want nil", got.Demands[1].RuleID)
	}
	if len(got.Tuners) != 1 || got.Tuners[0].Site != "site-a" || got.Tuners[0].IsAvailable {
		t.Errorf("ReadSite tuners = %+v, want the unavailable site-a row retained", got.Tuners)
	}

	all, err := f.Capacity.ReadAllSites(ctx)
	if err != nil {
		t.Fatalf("ReadAllSites: %v", err)
	}
	if len(all.Demands) != 3 || len(all.Tuners) != 2 {
		t.Fatalf("ReadAllSites = %+v, want both site inputs", all)
	}
	if all.Demands[0].RuleID == nil || *all.Demands[0].RuleID != ruleID {
		t.Errorf("ReadAllSites site-a rule id = %v, want %d", all.Demands[0].RuleID, ruleID)
	}
	if all.Demands[1].RuleID != nil || all.Demands[2].RuleID != nil {
		t.Errorf("ReadAllSites unruled rule ids = (%v, %v), want nil", all.Demands[1].RuleID, all.Demands[2].RuleID)
	}
}

func runCapacityPreviewRule(t *testing.T, newStore NewTunerStore) {
	f := newStore(t)
	ctx := context.Background()
	ruleID := f.SeedRule(t, false, true)

	got, err := f.Capacity.PreviewRule(ctx, ruleID)
	if err != nil {
		t.Fatalf("PreviewRule: %v", err)
	}
	if got.Enabled || !got.DedupeEnabled {
		t.Errorf("PreviewRule = %+v, want disabled and dedupe enabled", got)
	}
	if _, err := f.Capacity.PreviewRule(ctx, ruleID+1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PreviewRule(missing rule) error = %v, want ErrNotFound", err)
	}
}

func runCapacityPreviewCandidates(t *testing.T, newStore NewTunerStore) {
	f := newStore(t)
	ctx := context.Background()
	ruleID := f.SeedRule(t, false, true)
	start := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Millisecond)
	ref := store.CapacityProgramRef{Site: "site-a", ProgramID: 7004}
	f.SeedPreviewProgram(t, ref, "GR", "27", start, 30*60*1000)

	got, err := f.Capacity.PreviewCandidates(ctx, []store.CapacityProgramRef{ref}, &ruleID)
	if err != nil {
		t.Fatalf("PreviewCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("PreviewCandidates = %+v, want one", got)
	}
	candidate := got[0]
	if candidate.Site != ref.Site || candidate.ProgramID != ref.ProgramID || candidate.ChannelType != "GR" ||
		candidate.Channel != "27" || !candidate.StartAt.Equal(start) || !candidate.EndAt.Equal(start.Add(30*time.Minute)) {
		t.Errorf("PreviewCandidates candidate = %+v, want seeded program", candidate)
	}

	empty, err := f.Capacity.PreviewCandidates(ctx, nil, &ruleID)
	if err != nil || len(empty) != 0 {
		t.Errorf("PreviewCandidates(no refs) = (%+v, %v), want empty and no error", empty, err)
	}
	// ルールの存在確認は PreviewRule の責務で、候補の読み取りは存在しない ruleID でも失敗しない。
	missingID := ruleID + 1
	if _, err := f.Capacity.PreviewCandidates(ctx, []store.CapacityProgramRef{ref}, &missingID); err != nil {
		t.Errorf("PreviewCandidates(missing rule) error = %v, want nil", err)
	}
}
