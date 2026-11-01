package worker

import (
	"testing"

	"github.com/fetburner/rokuban/internal/jobs"
)

// ジョブは一意化しない。ByState を使うと、実行中に来た 2 本目の編集が
// 投入されずに捨てられ、次の定期再評価（15 分）まで反映されない。
//
// **River は running を外した ByState を挿入時にエラーにする**ので、
// 「実行中を除いた状態集合」で代用はできない（river@v0.50.0 の
// requiredV3states）。docs/data/series.md §8。
func TestLabelRuleReconcileArgs_NotUnique(t *testing.T) {
	spec := jobs.LabelRuleReconcileArgs{}.Spec()
	if spec.Queue != "cleanup" {
		t.Errorf("Spec().Queue = %q, want %q", spec.Queue, "cleanup")
	}
	if spec.Coalesce {
		t.Error("Spec().Coalesce = true, want false (a second edit must not be dropped)")
	}
}
