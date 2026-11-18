package encode

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// 窓が満杯で継続を積むパスでは、完走の記録が失敗しても継続は積まれたままでなければならない。
// 記録を継続の投入より前に置くと、記録の失敗で連鎖が止まる。
func TestEncodeReconcileMarker_ContinuationIsEnqueuedBeforeMarkerWrite(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	var ids []int64
	for i := range 3 {
		ids = append(ids, prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir,
			fmt.Sprintf("marker/window%d.m2ts", i), []string{"h265"}, []byte("x")))
	}
	markers := &markerRecorder{err: errors.New("marker write rejected")}
	enqueuer := jobqfaketest.NewRecordingEnqueuer()
	w := &EncodeReconcileWorker{Enqueuer: enqueuer, Profiles: encodeConfigWith("h265"),
		Media: pgstoretest.Media(pool), RowLimit: 2, Markers: markers}
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.EncodeReconcileArgs{}); err == nil {
		t.Fatal("EncodeReconcileWorker.Work = nil, want the marker write error")
	}
	continuations := recordedArgs[jobs.EncodeReconcileArgs](t, enqueuer.CallsOfKind(jobs.EncodeReconcileArgs{}.Kind()))
	if len(continuations) != 1 || continuations[0].AfterRecordingID != ids[1] {
		t.Fatalf("continuations = %+v, want one continuation after recording %d", continuations, ids[1])
	}
}
