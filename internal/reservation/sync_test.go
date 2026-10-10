package reservation

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// TestEvaluateSyncCandidates_FiltersBaseSkip は skip 済み候補にも正しい判定が付くことを確認する。
func TestEvaluateSyncCandidates_FiltersBaseSkip(t *testing.T) {
	rows := []sqlcgen.ListReservationsForSyncEvaluationRow{
		{
			// M2-6 の重複排除が base.skip = true を立てた想定の行。
			Reservation: sqlcgen.Reservation{ProgramID: 100, Base: []byte(`{"skip":true}`)},
		},
		{
			// 通常の（skip されていない）予約行。
			Reservation: sqlcgen.Reservation{ProgramID: 200},
		},
	}

	candidates := EvaluateSyncCandidates(rows)

	if len(candidates) != 2 {
		t.Fatalf("len(candidates) = %d, want 2 (skip フラグ付きの形では両方取得できること)", len(candidates))
	}

	byProgramID := make(map[int64]SyncCandidate, len(candidates))
	for _, c := range candidates {
		if c.Err != nil {
			t.Fatalf("candidate for program %d has unexpected error: %v", c.Reservation.ProgramID, c.Err)
		}
		byProgramID[c.Reservation.ProgramID] = c
	}

	if !byProgramID[100].Skipped {
		t.Error("program 100 (base.skip=true) should be Skipped=true in the unfiltered candidate list")
	}
	if byProgramID[200].Skipped {
		t.Error("program 200 (no skip) should be Skipped=false")
	}

	// 絞り込み済みリスト: reconciler.listDesired と同じ絞り込み（Skipped を除く）。
	var filtered []SyncCandidate
	for _, c := range candidates {
		if c.Skipped {
			continue
		}
		filtered = append(filtered, c)
	}
	if len(filtered) != 1 || filtered[0].Reservation.ProgramID != 200 {
		t.Fatalf("filtered candidates = %+v, want only program_id=200 "+
			"(base.skip=true の予約が絞り込み済みリストに混ざってはならない)", filtered)
	}
}

// TestEvaluateSyncCandidates_BrokenJSONReturnsErr は base の jsonb が壊れている
// 行を Skipped: false のまま握りつぶさないこと（不変条件: jsonb の Unmarshal
// 失敗を握りつぶさない）を確認する。呼び出し元がどう扱うか（ログして除外する/
// 全体を失敗させる）はこの関数の責務ではないため、Err に載せて返すところまでを
// 検証する。
func TestEvaluateSyncCandidates_BrokenJSONReturnsErr(t *testing.T) {
	rows := []sqlcgen.ListReservationsForSyncEvaluationRow{
		{
			Reservation: sqlcgen.Reservation{ProgramID: 300, Base: []byte(`not json`)},
		},
	}

	candidates := EvaluateSyncCandidates(rows)
	if len(candidates) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(candidates))
	}
	if candidates[0].Err == nil {
		t.Fatal("expected an error for broken base jsonb, got nil (a broken row must not be silently treated as Skipped=false)")
	}
	if candidates[0].Skipped {
		t.Error("Skipped should be false (meaningless) when Err is set, not true")
	}
}
