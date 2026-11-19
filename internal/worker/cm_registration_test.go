package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

type cmDetectionRouteStore struct {
	store.CMDetection
	err error
}

func (s cmDetectionRouteStore) GetWorkItem(context.Context, int64) (store.CMDetectionWorkItem, error) {
	return store.CMDetectionWorkItem{}, s.err
}

type cmStationRouteStore struct {
	store.CMStation
	err error
}

func (s cmStationRouteStore) GetLogoArea(context.Context, store.CMStationRef) (store.CMLogoArea, error) {
	return store.CMLogoArea{}, s.err
}

func TestNewWorkersRegistersCMJobs(t *testing.T) {
	t.Parallel()

	tokyoErr := errors.New("Tokyo analysis store called")
	nagoyaErr := errors.New("Nagoya analysis store called")
	unscopedErr := errors.New("unscoped analysis store called")
	registry := NewWorkers(&Deps{
		UnscopedCMDetectionStore: cmDetectionRouteStore{err: unscopedErr},
		PerStore: map[string]*StoreDeps{
			"tokyo":  {MediaStoreName: "tokyo", CMDetectionStore: cmDetectionRouteStore{err: tokyoErr}, CMStationStore: cmStationRouteStore{err: tokyoErr}},
			"nagoya": {MediaStoreName: "nagoya", CMDetectionStore: cmDetectionRouteStore{err: nagoyaErr}, CMStationStore: cmStationRouteStore{err: nagoyaErr}},
		},
	})

	for _, tc := range []struct {
		kind, payload, queue string
		wantErr              error
	}{
		{"cm_detect", `{"store":"tokyo","recording_id":1}`, "cm_detect_tokyo", tokyoErr},
		{"cm_logo_candidate", `{"store":"nagoya","recording_id":1}`, "cm_detect_nagoya", nagoyaErr},
		{"cm_detect_reconcile", `{}`, "cleanup", nil},
		{"cm_detect", `{"store":"unmounted","recording_id":1}`, "cm_detect_unmounted", nil},
		{"cm_logo_candidate", `{"store":"unmounted","recording_id":1}`, "cm_detect_unmounted", nil},
	} {
		t.Run(tc.kind+"/"+tc.queue, func(t *testing.T) {
			args, err := registry.Decode(tc.kind, []byte(tc.payload))
			testutil.RequireNoError(t, err, "decoding registered job")
			queue, err := jobs.PhysicalQueueFor(args)
			testutil.RequireNoError(t, err, "choosing physical queue")
			if queue != tc.queue {
				t.Fatalf("physical queue = %q, want %q", queue, tc.queue)
			}
			err = registry.Work(t.Context(), args)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Work error = %v, want %v", err, tc.wantErr)
				}
			case tc.kind == "cm_detect_reconcile":
				if err == nil || err.Error() != "CM detection reconcile: job enqueuer is nil" {
					t.Fatalf("Work error = %v, want reconcile's nil-enqueuer error", err)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), `store "unmounted" is not among this worker's mounted stores [nagoya tokyo]`) {
					t.Fatalf("Work error = %v, want unmounted-store rejection", err)
				}
			}
		})
	}
}

func TestNewIngestWorkerUsesStoreScopedCMDetectionStore(t *testing.T) {
	t.Parallel()

	unscopedErr := errors.New("unscoped CM detection store called")
	scopedErr := errors.New("store-scoped CM detection store called")
	w := newIngestWorker(
		&Deps{UnscopedCMDetectionStore: cmDetectionRouteStore{err: unscopedErr}},
		&StoreDeps{CMDetectionStore: cmDetectionRouteStore{err: scopedErr}},
	)
	_, err := w.CMDetectionStore.GetWorkItem(t.Context(), 1)
	if !errors.Is(err, scopedErr) {
		t.Fatalf("ingest CM detection store error = %v, want store-scoped error %v", err, scopedErr)
	}
}
