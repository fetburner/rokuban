package worker

import (
	"context"
	"testing"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 枠を読む DB エラーは枠と解像度の不一致ではないので、stage は setup になる。
// 取り消し済みの ctx で GetCMLogoArea だけを失敗させる（それ以前の手順は DB を使わない）。
func TestCMDetectLoadingAreaDBErrorIsSetupStage(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	w := newCMDetectTestWorker(pool, mediaDir, newFakeCMTools(t, buildTestLGD(2, 2, 1000, 4080), 0, "Trim(0,299)", "10.010000"))
	rel := "cm/area-db-error.ts"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := w.detect(ctx, 1, sqlcgen.GetCMDetectionWorkItemRow{RelPath: &rel, NetworkID: 32736, ServiceID: 1024})
	stage := cmFailureStage(err)
	if stage == nil || *stage != "setup" {
		t.Fatalf("stage = %v (err %v), want setup", stage, err)
	}
}
