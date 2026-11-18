package encode

import (
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 原本を読むジョブを積む側は、原本のコピーがある置き場を選ぶ（置き場を決め打ちしない）。
// コピーを別の置き場へ移した録画の encode は、その置き場のキューに積まれる。
func TestEncodeReconcileQueuesForTheStoreHoldingTheOriginal(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "route/moved.m2ts", []string{"h265"}, []byte("x"))
	testutil.MustExec(t, pool, "moving the original copy to another store", `UPDATE media_asset_copies SET store = 'takamatsu' WHERE store = $1`, dbtest.StoreName)
	w := &EncodeReconcileWorker{Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool)}
	enqueuer := runEncodeReconcilePass(t, w)
	got := findEncodeJob(t, enqueuer, id, "h265")
	if got.Store != "takamatsu" {
		t.Fatalf("encode queued for store %q, want literal %q", got.Store, "takamatsu")
	}
	queue, err := jobs.PhysicalQueueFor(got)
	if err != nil || queue != "encode_takamatsu" {
		t.Fatalf("PhysicalQueueFor = %q, %v; want encode_takamatsu", queue, err)
	}
}
