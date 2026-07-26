package shadowdiff

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/epgstation"
)

// allowlist が「差分を見逃す方向」に働かないことを確かめる。
//
// このレポートは M2 の出口基準（予約差分ゼロ or 全件説明可能）を測る道具なので、
// 説明できない差分を Expected に落としてしまうのが最悪の壊れ方になる。
// 過剰に報告する分には害がないが、見逃しは並走の判断を誤らせる。
func TestCompare_AllowlistDoesNotHideRealDiffs(t *testing.T) {
	now := time.Now()
	pid := int64(327360102415397)
	// isConflict だけの EPGStation 予約は説明にならない
	rep := Compare(nil, []epgstation.Reserve{{
		ID: 1, ProgramID: &pid, IsConflict: true, StartAt: now.UnixMilli(), Name: "競合のみ",
	}})
	if len(rep.EPGStationOnly) != 1 {
		t.Errorf("isConflict は allowlist に入れてはならない: %+v", rep)
	}
	if !rep.HasUnexplained() {
		t.Error("HasUnexplained should be true")
	}

	// Rokuban 側が skip でない普通の予約なのに EPGStation に無ければ差分
	rep = Compare([]RokubanReservation{{ProgramID: pid, Title: "Rokuban のみ", StartAt: now}}, nil)
	if len(rep.RokubanOnly) != 1 {
		t.Errorf("RokubanOnly が検出されない: %+v", rep)
	}

	// 両方にあるが Rokuban 側が skip の場合、EPGStation 側は Expected でよいが
	// 「一致」に数えてはならない（録画されるのは EPGStation だけなので）
	rep = Compare(
		[]RokubanReservation{{ProgramID: pid, Title: "skip 済み", StartAt: now, Skipped: true}},
		[]epgstation.Reserve{{ID: 1, ProgramID: &pid, StartAt: now.UnixMilli(), Name: "EPGStation 側は生きている"}},
	)
	if len(rep.Both) != 0 {
		t.Errorf("Rokuban 側が skip なら Both に数えてはならない: %+v", rep)
	}
	if len(rep.Expected) != 1 {
		t.Errorf("Expected に落ちるべき: %+v", rep)
	}
}
