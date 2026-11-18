package encode

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

// failingEncodeListing は候補の列挙だけを失敗させる。
type failingEncodeListing struct {
	store.MediaReconcile
}

func (failingEncodeListing) ListMissingEncodeProfiles(context.Context, store.MissingEncodeProfilesParams) ([]store.MissingEncodeProfile, error) {
	return nil, errors.New("listing rejected")
}

func TestEncodeReconcileMarker_FullPassMarksOnlyAfterSuccess(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	w := &EncodeReconcileWorker{Enqueuer: jobqfaketest.NewRecordingEnqueuer(), Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.EncodeReconcileArgs{}), "EncodeReconcileWorker.Work")
	if got := markers.recordedPasses(); !slices.Equal(got, []string{store.ReconcilePassEncode}) {
		t.Fatalf("recorded passes = %v, want [encode] after a completed full pass", got)
	}
}

func TestEncodeReconcileMarker_FailedListingDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{}
	w := &EncodeReconcileWorker{Enqueuer: jobqfaketest.NewRecordingEnqueuer(), Profiles: encodeConfigWith("h265"),
		Media: failingEncodeListing{pgstoretest.Media(pool)}, Markers: markers}
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.EncodeReconcileArgs{}); err == nil {
		t.Fatal("EncodeReconcileWorker.Work = nil, want the listing error")
	}
	if got := markers.recordedPasses(); len(got) != 0 {
		t.Fatalf("recorded passes = %v, want none for an incomplete pass", got)
	}
}

func TestEncodeReconcileMarker_TargetedPassDoesNotMark(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	recordingID := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "marker/targeted.m2ts",
		[]string{"h265"}, []byte("x"))
	markers := &markerRecorder{}
	w := &EncodeReconcileWorker{Enqueuer: jobqfaketest.NewRecordingEnqueuer(), Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.EncodeReconcileArgs{RecordingID: recordingID}), "EncodeReconcileWorker.Work(targeted)")
	if got := markers.recordedPasses(); len(got) != 0 {
		t.Fatalf("recorded passes = %v after a targeted pass, want none", got)
	}
}

// 候補の投入が全部失敗しても、一覧を完走したパスは記録する（候補ごとの失敗でパスを止めない）。
func TestEncodeReconcileMarker_CandidateEnqueueFailureStillMarks(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	prepareEncodeInput(t, pool, dbtest.Recording(t, pool), t.TempDir(), "marker/enqueue.m2ts",
		[]string{"h265"}, []byte("x"))
	markers := &markerRecorder{}
	w := &EncodeReconcileWorker{Enqueuer: markerRejectingEnqueuer{}, Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Markers: markers}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.EncodeReconcileArgs{}), "EncodeReconcileWorker.Work, want nil when only candidate enqueues fail")
	if got := markers.recordedPasses(); !slices.Equal(got, []string{store.ReconcilePassEncode}) {
		t.Fatalf("recorded passes = %v, want [encode]", got)
	}
}

func TestEncodeReconcileMarker_WriteFailureFailsThePass(t *testing.T) {
	t.Parallel()
	pool := testutil.SetupDB(t)
	if pool == nil {
		return
	}
	markers := &markerRecorder{err: errors.New("marker write rejected")}
	w := &EncodeReconcileWorker{Enqueuer: jobqfaketest.NewRecordingEnqueuer(), Profiles: encodeConfigWith("h265"), Media: pgstoretest.Media(pool), Markers: markers}
	if err := jobqfaketest.RunWorkerOnce(context.Background(), w, jobs.EncodeReconcileArgs{}); err == nil {
		t.Fatal("EncodeReconcileWorker.Work = nil, want the marker write error")
	}
}

// markerRecorder は全件パスの完走記録を残す。err を設定すると書き込みが失敗する。
type markerRecorder struct {
	store.LoopMarkers
	mu     sync.Mutex
	passes []string
	err    error
}

func (m *markerRecorder) MarkReconcilePass(_ context.Context, pass string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.passes = append(m.passes, pass)
	return nil
}

func (m *markerRecorder) recordedPasses() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.passes)
}

// markerRejectingEnqueuer は全件の投入を失敗させる。候補ごとの失敗がパスを止めないことを見る。
type markerRejectingEnqueuer struct{}

func (markerRejectingEnqueuer) Enqueue(context.Context, jobq.Args, ...jobq.EnqueueOpt) (jobq.Result, error) {
	return jobq.Result{}, errors.New("enqueue rejected")
}
