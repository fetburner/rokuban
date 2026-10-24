package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/store"
)

// RulerProgram は ruler 契約テストが使う EPG 番組である。
type RulerProgram struct {
	Site      string
	ProgramID int64
	EventID   int32
	Title     string
	StartAt   time.Time
}

// RulerPassFixture は ruler pass と、実装が使う永続状態の仕込み口をまとめる。
type RulerPassFixture struct {
	Pass                 store.RulerPass
	Bumper               store.RulerInputBumper
	Reservations         store.Reservations
	Breakers             store.Breakers
	NewProgram           func(t *testing.T, title string) RulerProgram
	NewRule              func(t *testing.T, name string, dedupeEnabled bool, threshold float32) int64
	NewFinishedRecording func(t *testing.T, ruleID int64, program RulerProgram, title string) int64
	SetProgramStarts     func(t *testing.T, program RulerProgram, snapshotStart, projectionStart time.Time)
	SetDedupeWindow      func(t *testing.T, ruleID int64, window time.Duration)
	SetRecordingStartAt  func(t *testing.T, recordingID int64, start time.Time)
	FailBreakerTrip      func(t *testing.T, name string)
}

// NewRulerPassStore は ruler pass 契約の実装と fixture を作る。
type NewRulerPassStore func(t *testing.T) RulerPassFixture

// RunRulerPass は予約の適用、削除ガード、breaker、dedupe の契約を実装に対して実行する。
func RunRulerPass(t *testing.T, newStore NewRulerPassStore) {
	t.Helper()
	checkedStore := func(t *testing.T) RulerPassFixture {
		f := newStore(t)
		if f.Pass == nil || f.Bumper == nil || f.Reservations == nil || f.Breakers == nil || f.NewProgram == nil || f.NewRule == nil || f.NewFinishedRecording == nil || f.SetProgramStarts == nil || f.SetDedupeWindow == nil || f.SetRecordingStartAt == nil || f.FailBreakerTrip == nil {
			t.Fatal("incomplete ruler pass fixture")
		}
		return f
	}
	t.Run("matching input revision commits the pass", func(t *testing.T) { runRulerRevisionMatch(t, checkedStore) })
	t.Run("stale input revision writes nothing", func(t *testing.T) { runRulerRevisionMismatch(t, checkedStore) })
	t.Run("bump advances the input revision by one", func(t *testing.T) { runRulerBump(t, checkedStore) })
	t.Run("delete rechecks investment after the read", func(t *testing.T) { runRulerDeleteRechecksInvestment(t, checkedStore) })
	t.Run("delete threshold trips breaker and keeps reservations", func(t *testing.T) { runRulerDeleteThreshold(t, checkedStore) })
	t.Run("breaker write failure rolls back the pass", func(t *testing.T) { runRulerBreakerRollback(t, checkedStore) })
	t.Run("upsert reports changed rows only", func(t *testing.T) { runRulerUpsertCounts(t, checkedStore) })
	t.Run("dedupe uses the configured title similarity", func(t *testing.T) { runRulerDedupeSimilarity(t, checkedStore) })
	t.Run("dedupe time window uses the supplied time", func(t *testing.T) { runRulerDedupeWindow(t, checkedStore) })
	t.Run("program ID reuse detection uses the supplied time", func(t *testing.T) { runRulerProgramIDReuseTime(t, checkedStore) })
	t.Run("snapshot pruning uses the supplied cutoff", func(t *testing.T) { runRulerSnapshotPruneTime(t, checkedStore) })
}

func runRulerDeleteRechecksInvestment(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	program := f.NewProgram(t, "stale intent guard")
	ruleID := f.NewRule(t, "stale intent guard", false, 0)
	seedRulerReservation(t, f, program, ruleID, json.RawMessage(`{"priority":1}`))

	// This read is the old pass's investment snapshot. The user intent commits after it,
	// before the delayed delete reaches ApplyPass.
	if ids, err := f.Pass.ListInvestmentProgramIDs(ctx, program.Site); err != nil || len(ids) != 0 {
		t.Fatalf("ListInvestmentProgramIDs(before intent) = (%v, %v), want no investment", ids, err)
	}
	if err := f.Reservations.PutIntent(ctx, program.Site, program.ProgramID, "record"); err != nil {
		t.Fatalf("PutIntent(after stale read): %v", err)
	}
	result, err := f.Pass.ApplyPass(ctx, program.Site, store.RulerPassPlan{
		InputRevision:     currentRulerInputRevision(t, f.Pass, program.Site),
		DeleteProgramIDs:  []int64{program.ProgramID},
		Now:               time.Now().UTC(),
		MaxDeletesPerPass: 10,
		BreakerName:       breaker.RulerDeletes,
	})
	if err != nil {
		t.Fatalf("ApplyPass(stale delete) = %v", err)
	}
	if result.Deleted != 0 || len(result.ReleasedProgramIDs) != 0 {
		t.Fatalf("ApplyPass(stale delete) result = %+v, want no deletion", result)
	}
	if _, err := f.Reservations.GetReservation(ctx, program.Site, program.ProgramID); err != nil {
		t.Fatalf("GetReservation(after stale delete) = %v, want row preserved", err)
	}
}

func runRulerDeleteThreshold(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	ruleID := f.NewRule(t, "delete threshold", false, 0)
	programs := []RulerProgram{f.NewProgram(t, "first stale row"), f.NewProgram(t, "second stale row")}
	ids := make([]int64, len(programs))
	for i, program := range programs {
		ids[i] = program.ProgramID
		seedRulerReservation(t, f, program, ruleID, json.RawMessage(`{"priority":1}`))
	}
	result, err := f.Pass.ApplyPass(ctx, programs[0].Site, store.RulerPassPlan{
		InputRevision:     currentRulerInputRevision(t, f.Pass, programs[0].Site),
		DeleteProgramIDs:  ids,
		Now:               time.Now().UTC(),
		MaxDeletesPerPass: 1,
		BreakerName:       breaker.RulerDeletes,
	})
	if err != nil {
		t.Fatalf("ApplyPass(over threshold) = %v", err)
	}
	if !result.BreakerTripped || result.Deleted != 0 {
		t.Fatalf("ApplyPass(over threshold) = %+v, want breaker trip and no deletion", result)
	}
	if tripped, err := f.Breakers.IsTripped(ctx, programs[0].Site, breaker.RulerDeletes); err != nil || !tripped {
		t.Fatalf("IsTripped(after threshold) = (%t, %v), want true", tripped, err)
	}
	remaining, err := f.Pass.ListReservationProgramIDs(ctx, programs[0].Site)
	if err != nil {
		t.Fatalf("ListReservationProgramIDs(after threshold): %v", err)
	}
	if len(remaining) != len(ids) {
		t.Fatalf("reservations after threshold = %v, want both %v preserved", remaining, ids)
	}
}

func runRulerBreakerRollback(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	ruleID := f.NewRule(t, "breaker rollback", false, 0)
	desired := f.NewProgram(t, "desired row")
	stale := f.NewProgram(t, "stale row")
	snapshotStart := desired.StartAt.Add(-48 * time.Hour)
	projectionStart := desired.StartAt.Add(24 * time.Hour)
	oldBase := json.RawMessage(`{"priority":1}`)
	seedRulerReservation(t, f, desired, ruleID, oldBase)
	seedRulerReservation(t, f, stale, ruleID, oldBase)
	f.SetProgramStarts(t, desired, snapshotStart, projectionStart)
	f.FailBreakerTrip(t, breaker.RulerDeletes)

	newBase := json.RawMessage(`{"priority":2}`)
	_, err := f.Pass.ApplyPass(ctx, desired.Site, store.RulerPassPlan{
		InputRevision:      currentRulerInputRevision(t, f.Pass, desired.Site),
		SnapshotProgramIDs: []int64{desired.ProgramID},
		ExistingProgramIDs: []int64{desired.ProgramID},
		Reservations: []store.RulerReservationInput{{
			ProgramID: desired.ProgramID, RuleID: &ruleID, Base: newBase,
		}},
		DeleteProgramIDs:  []int64{stale.ProgramID},
		Now:               time.Now().UTC(),
		MaxDeletesPerPass: 0,
		BreakerName:       breaker.RulerDeletes,
	})
	if err == nil {
		t.Fatal("ApplyPass(breaker write failure) = nil, want error")
	}
	got, err := f.Reservations.GetReservation(ctx, desired.Site, desired.ProgramID)
	if err != nil {
		t.Fatalf("GetReservation(desired after rollback): %v", err)
	}
	var gotBase map[string]int
	if err := json.Unmarshal(got.Reservation.Base, &gotBase); err != nil {
		t.Fatalf("decoding desired base after rollback: %v", err)
	}
	if gotBase["priority"] != 1 || len(gotBase) != 1 {
		t.Fatalf("desired base after rollback = %s, want %s", got.Reservation.Base, oldBase)
	}
	if !got.Snapshot.StartAt.Equal(snapshotStart) {
		t.Fatalf("GetReservation(desired).Snapshot.StartAt after rollback = %v, want %v", got.Snapshot.StartAt, snapshotStart)
	}
	if _, err := f.Reservations.GetReservation(ctx, stale.Site, stale.ProgramID); err != nil {
		t.Fatalf("GetReservation(stale after rollback) = %v, want row preserved", err)
	}
	if tripped, err := f.Breakers.IsTripped(ctx, desired.Site, breaker.RulerDeletes); err != nil || tripped {
		t.Fatalf("IsTripped(after failed transaction) = (%t, %v), want false", tripped, err)
	}
}

func runRulerUpsertCounts(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	ruleID := f.NewRule(t, "upsert counts", false, 0)
	updated := f.NewProgram(t, "updated")
	unchanged := f.NewProgram(t, "unchanged")
	created := f.NewProgram(t, "created")
	oldBase := json.RawMessage(`{"priority":1}`)
	for _, program := range []RulerProgram{updated, unchanged} {
		seedRulerReservation(t, f, program, ruleID, oldBase)
	}
	newBase := json.RawMessage(`{"priority":2}`)
	result, err := f.Pass.ApplyPass(ctx, updated.Site, store.RulerPassPlan{
		InputRevision:      currentRulerInputRevision(t, f.Pass, updated.Site),
		SnapshotProgramIDs: []int64{updated.ProgramID, unchanged.ProgramID, created.ProgramID},
		ExistingProgramIDs: []int64{updated.ProgramID, unchanged.ProgramID},
		Reservations: []store.RulerReservationInput{
			{ProgramID: updated.ProgramID, RuleID: &ruleID, Base: newBase},
			{ProgramID: unchanged.ProgramID, RuleID: &ruleID, Base: oldBase},
			{ProgramID: created.ProgramID, RuleID: &ruleID, Base: oldBase},
		},
		Now:               time.Now().UTC(),
		MaxDeletesPerPass: 10,
		BreakerName:       breaker.RulerDeletes,
	})
	if err != nil {
		t.Fatalf("ApplyPass(upsert counts) = %v", err)
	}
	if result.Created != 1 || result.Updated != 1 {
		t.Fatalf("ApplyPass(upsert counts) = %+v, want one create and one changed update", result)
	}
}

func runRulerDedupeSimilarity(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	sameTitle := f.NewProgram(t, "再放送テスト 第1話")
	widthDifference := f.NewProgram(t, "NEWS 123")
	unrelated := f.NewProgram(t, "ニュース天気予報")
	sameRule := f.NewRule(t, "same title", true, 0.5)
	widthRule := f.NewRule(t, "fullwidth and halfwidth", true, 0.9)
	unrelatedRule := f.NewRule(t, "unrelated title", true, 0.5)
	sameRecording := f.NewFinishedRecording(t, sameRule, sameTitle, "再放送テスト 第1話")
	f.NewFinishedRecording(t, widthRule, widthDifference, "ＮＥＷＳ １２３")
	f.NewFinishedRecording(t, unrelatedRule, unrelated, "完全に別の題名")

	matches, err := f.Pass.EvaluateDedupe(ctx, sameTitle.Site, []store.RulerDedupeCandidate{
		{ProgramID: sameTitle.ProgramID, RuleID: sameRule},
		{ProgramID: widthDifference.ProgramID, RuleID: widthRule},
		{ProgramID: unrelated.ProgramID, RuleID: unrelatedRule},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("EvaluateDedupe: %v", err)
	}
	if len(matches) != 1 || matches[sameTitle.ProgramID].RecordingID != sameRecording || matches[sameTitle.ProgramID].Similarity != 1 {
		t.Fatalf("EvaluateDedupe matches = %+v, want only exact-title match %d with similarity 1", matches, sameRecording)
	}
}

func runRulerDedupeWindow(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	now := time.Date(2020, time.April, 5, 6, 7, 8, 0, time.UTC)
	inside := f.NewProgram(t, "alpha bravo charlie")
	outside := f.NewProgram(t, "xray yankee zulu")
	ruleID := f.NewRule(t, "dedupe supplied time", true, 0.5)
	f.SetDedupeWindow(t, ruleID, 7*24*time.Hour)
	insideRecording := f.NewFinishedRecording(t, ruleID, inside, "alpha bravo charlie")
	f.SetRecordingStartAt(t, insideRecording, now.Add(-6*24*time.Hour))
	outOfWindowRecording := f.NewFinishedRecording(t, ruleID, outside, "xray yankee zulu")
	f.SetRecordingStartAt(t, outOfWindowRecording, now.Add(-8*24*time.Hour))

	matches, err := f.Pass.EvaluateDedupe(ctx, inside.Site, []store.RulerDedupeCandidate{
		{ProgramID: inside.ProgramID, RuleID: ruleID},
		{ProgramID: outside.ProgramID, RuleID: ruleID},
	}, now)
	if err != nil {
		t.Fatalf("EvaluateDedupe(window): %v", err)
	}
	if got := matches[inside.ProgramID].RecordingID; got != insideRecording {
		t.Fatalf("inside-window match = %d, want recording %d", got, insideRecording)
	}
	if _, ok := matches[outside.ProgramID]; ok {
		t.Fatalf("outside-window candidate matched recording %d", outOfWindowRecording)
	}
}

func runRulerProgramIDReuseTime(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	program := f.NewProgram(t, "program ID reuse")
	oldStart := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	projectionStart := oldStart.Add(25 * time.Hour)
	f.SetProgramStarts(t, program, oldStart, projectionStart)

	beforeEnd := oldStart.Add(29 * time.Minute)
	rows, err := f.Pass.ListProgramIDReuses(ctx, program.Site, beforeEnd)
	if err != nil {
		t.Fatalf("ListProgramIDReuses(before end): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ListProgramIDReuses(before end) = %+v, want none", rows)
	}
	afterEnd := oldStart.Add(31 * time.Minute)
	rows, err = f.Pass.ListProgramIDReuses(ctx, program.Site, afterEnd)
	if err != nil {
		t.Fatalf("ListProgramIDReuses(after end): %v", err)
	}
	if len(rows) != 1 || rows[0].ProgramID != program.ProgramID || !rows[0].SnapshotStartAt.Equal(oldStart) || !rows[0].ProjectionStartAt.Equal(projectionStart) {
		t.Fatalf("ListProgramIDReuses(after end) = %+v, want program %d from %v to %v", rows, program.ProgramID, oldStart, projectionStart)
	}
}

func runRulerSnapshotPruneTime(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	oldProgram := f.NewProgram(t, "ended snapshot")
	newProgram := f.NewProgram(t, "retained snapshot")
	oldStart := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	newStart := oldStart.Add(48 * time.Hour)
	f.SetProgramStarts(t, oldProgram, oldStart, oldProgram.StartAt)
	f.SetProgramStarts(t, newProgram, newStart, newProgram.StartAt)

	cutoff := oldStart.Add(24 * time.Hour)
	deleted, err := f.Pass.PruneEndedSnapshots(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneEndedSnapshots(cutoff): %v", err)
	}
	if deleted != 1 {
		t.Fatalf("PruneEndedSnapshots(cutoff) = %d, want one snapshot before supplied cutoff", deleted)
	}
	if again, err := f.Pass.PruneEndedSnapshots(ctx, cutoff); err != nil || again != 0 {
		t.Fatalf("second PruneEndedSnapshots(cutoff) = (%d, %v), want (0, nil): snapshot for program %d remains before supplied cutoff", again, err, oldProgram.ProgramID)
	}
	if later, err := f.Pass.PruneEndedSnapshots(ctx, newStart.Add(24*time.Hour)); err != nil || later != 1 {
		t.Fatalf("PruneEndedSnapshots(after new start) = (%d, %v), want (1, nil): snapshot for program %d was pruned by the earlier cutoff", later, err, newProgram.ProgramID)
	}
}

func seedRulerReservation(t *testing.T, f RulerPassFixture, program RulerProgram, ruleID int64, base json.RawMessage) {
	t.Helper()
	_, err := f.Pass.ApplyPass(context.Background(), program.Site, store.RulerPassPlan{
		InputRevision:      currentRulerInputRevision(t, f.Pass, program.Site),
		SnapshotProgramIDs: []int64{program.ProgramID},
		Reservations: []store.RulerReservationInput{{
			ProgramID: program.ProgramID, RuleID: &ruleID, Base: base,
		}},
		Now:               time.Now().UTC(),
		MaxDeletesPerPass: 10,
		BreakerName:       breaker.RulerDeletes,
	})
	if err != nil {
		t.Fatalf("seeding ruler reservation: %v", err)
	}
}

func currentRulerInputRevision(t *testing.T, pass store.RulerPass, site string) int64 {
	t.Helper()
	revision, err := pass.GetRulerInputRevision(context.Background(), site)
	if err != nil {
		t.Fatalf("GetRulerInputRevision(%q): %v", site, err)
	}
	return revision
}

func runRulerRevisionMatch(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	program := f.NewProgram(t, "matching revision")
	ruleID := f.NewRule(t, "matching revision", false, 0)
	if err := f.Bumper.BumpRulerInputRevision(ctx, program.Site); err != nil {
		t.Fatalf("BumpRulerInputRevision: %v", err)
	}
	result, err := f.Pass.ApplyPass(ctx, program.Site, store.RulerPassPlan{
		InputRevision:      currentRulerInputRevision(t, f.Pass, program.Site),
		SnapshotProgramIDs: []int64{program.ProgramID},
		Reservations: []store.RulerReservationInput{{
			ProgramID: program.ProgramID, RuleID: &ruleID, Base: json.RawMessage(`{"priority":1}`),
		}},
		Now:               time.Now().UTC(),
		MaxDeletesPerPass: 10,
		BreakerName:       breaker.RulerDeletes,
	})
	if err != nil || result.InputRevisionChanged || result.Created != 1 {
		t.Fatalf("ApplyPass(matching revision) = %+v, %v, want one created reservation", result, err)
	}
	if _, err := f.Reservations.GetReservation(ctx, program.Site, program.ProgramID); err != nil {
		t.Fatalf("GetReservation(after matching pass) = %v, want committed row", err)
	}
}

func runRulerRevisionMismatch(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	ruleID := f.NewRule(t, "stale revision", false, 0)
	existing := f.NewProgram(t, "existing row")
	created := f.NewProgram(t, "would be created")
	seedRulerReservation(t, f, existing, ruleID, json.RawMessage(`{"priority":1}`))
	stale := currentRulerInputRevision(t, f.Pass, existing.Site)
	if err := f.Bumper.BumpRulerInputRevision(ctx, existing.Site); err != nil {
		t.Fatalf("BumpRulerInputRevision: %v", err)
	}
	snapshotStart := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	projectionStart := snapshotStart.Add(25 * time.Hour)
	f.SetProgramStarts(t, created, snapshotStart, projectionStart)
	result, err := f.Pass.ApplyPass(ctx, existing.Site, store.RulerPassPlan{
		InputRevision:      stale,
		SnapshotProgramIDs: []int64{created.ProgramID},
		ExistingProgramIDs: []int64{existing.ProgramID},
		Reservations: []store.RulerReservationInput{{
			ProgramID: created.ProgramID, RuleID: &ruleID, Base: json.RawMessage(`{"priority":2}`),
		}},
		DeleteProgramIDs:  []int64{existing.ProgramID},
		Now:               time.Now().UTC(),
		MaxDeletesPerPass: 0,
		BreakerName:       breaker.RulerDeletes,
	})
	if err != nil || !result.InputRevisionChanged {
		t.Fatalf("ApplyPass(stale revision) = %+v, %v, want InputRevisionChanged", result, err)
	}
	if _, err := f.Reservations.GetReservation(ctx, created.Site, created.ProgramID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetReservation(created by stale pass) = %v, want ErrNotFound", err)
	}
	if _, err := f.Reservations.GetReservation(ctx, existing.Site, existing.ProgramID); err != nil {
		t.Fatalf("GetReservation(existing after stale pass) = %v, want row preserved", err)
	}
	// スナップショットは射影と開始がずれているので、再利用の候補として SnapshotStartAt が読める。
	// 古い pass がスナップショットを upsert していたら、SnapshotStartAt は射影の開始へ変わる。
	rows, err := f.Pass.ListProgramIDReuses(ctx, created.Site, snapshotStart.Add(31*time.Minute))
	if err != nil {
		t.Fatalf("ListProgramIDReuses(after stale pass): %v", err)
	}
	if len(rows) != 1 || rows[0].ProgramID != created.ProgramID || !rows[0].SnapshotStartAt.Equal(snapshotStart) {
		t.Fatalf("ListProgramIDReuses(after stale pass) = %d rows, rows[0].ProgramID/SnapshotStartAt want %d/%v (snapshot unchanged)", len(rows), created.ProgramID, snapshotStart)
	}
	if tripped, err := f.Breakers.IsTripped(ctx, existing.Site, breaker.RulerDeletes); err != nil || tripped {
		t.Fatalf("IsTripped(after stale pass) = (%t, %v), want false", tripped, err)
	}
}

func runRulerBump(t *testing.T, newStore NewRulerPassStore) {
	f := newStore(t)
	ctx := context.Background()
	program := f.NewProgram(t, "bump")
	before := currentRulerInputRevision(t, f.Pass, program.Site)
	if err := f.Bumper.BumpRulerInputRevision(ctx, program.Site); err != nil {
		t.Fatalf("BumpRulerInputRevision: %v", err)
	}
	if got := currentRulerInputRevision(t, f.Pass, program.Site) - before; got != 1 {
		t.Fatalf("revision delta after one bump = %d, want 1", got)
	}
}
