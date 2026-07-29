package db

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// TestEvaluateSyncCandidates_FiltersBaseSkip は issue #54 の回帰テスト。
//
// ListReservationsForSyncEvaluation（旧 ListSyncableReservationsBySite）は
// state <> 'orphaned' の候補を返すだけで、base.skip = true の行（M2-6 の
// 重複排除が立てる）を絞り落とさない。以前の shadow-diff はこのクエリの結果を
// そのまま「同期される予約」として扱ってしまい（Skipped: false を決め打ち）、
// Rokuban が実際には録らない予約を EPGStation と「一致」と誤報告した。
//
// このテストは「新しい呼び出し元が EvaluateSyncCandidates を使わず素の
// ListReservationsForSyncEvaluation の結果だけで済ませると、skip 済みの予約が
// 混ざる」という同じ形のミスを EvaluateSyncCandidates 自体が捕まえることを
// 確認する:
//   - 絞り込み済みリスト（reconciler.listDesired が使う: Skipped == false だけ）に
//     base.skip = true の行が含まれないこと（両方向のうち「除外される」側）
//   - skip フラグ付きの全件（shadow-diff が使う）には含まれ、Skipped == true で
//     判定されていること（両方向のうち「取得できる」側）
func TestEvaluateSyncCandidates_FiltersBaseSkip(t *testing.T) {
	rows := []sqlcgen.ListReservationsForSyncEvaluationRow{
		{
			// M2-6 の重複排除が base.skip = true を立てた想定の行。
			Reservation: sqlcgen.Reservation{ID: 1, ProgramID: 100, Base: []byte(`{"skip":true}`)},
		},
		{
			// 通常の（skip されていない）予約行。
			Reservation: sqlcgen.Reservation{ID: 2, ProgramID: 200},
		},
	}

	candidates := EvaluateSyncCandidates(rows)

	if len(candidates) != 2 {
		t.Fatalf("len(candidates) = %d, want 2 (skip フラグ付きの形では両方取得できること)", len(candidates))
	}

	byID := make(map[int64]SyncCandidate, len(candidates))
	for _, c := range candidates {
		if c.Err != nil {
			t.Fatalf("candidate for reservation %d has unexpected error: %v", c.Reservation.ID, c.Err)
		}
		byID[c.Reservation.ID] = c
	}

	if !byID[1].Skipped {
		t.Error("reservation 1 (base.skip=true) should be Skipped=true in the unfiltered candidate list")
	}
	if byID[2].Skipped {
		t.Error("reservation 2 (no skip) should be Skipped=false")
	}

	// 絞り込み済みリスト: reconciler.listDesired と同じ絞り込み（Skipped を除く）。
	var filtered []SyncCandidate
	for _, c := range candidates {
		if c.Skipped {
			continue
		}
		filtered = append(filtered, c)
	}
	if len(filtered) != 1 || filtered[0].Reservation.ID != 2 {
		t.Fatalf("filtered candidates = %+v, want only reservation id=2 "+
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
			Reservation: sqlcgen.Reservation{ID: 3, ProgramID: 300, Base: []byte(`not json`)},
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
