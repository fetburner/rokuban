package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

type deleteRouteBreakers struct {
	store.Breakers
	errs map[string]error
}

func (b deleteRouteBreakers) IsTripped(_ context.Context, scope, _ string) (bool, error) {
	return false, b.errs[scope]
}

type transferRegistrationStore struct {
	store.StorageTransfer
	reconcileErr error
	transferErr  error
}

func (s transferRegistrationStore) ListActiveDerivedCopiesInNonDerivedS3(context.Context, []string, string) ([]store.DerivedS3CopyCount, error) {
	return nil, nil
}

func (s transferRegistrationStore) ClearObsoleteMediaAssetTransferAttempts(context.Context, store.StoragePolicy) error {
	return s.reconcileErr
}

func (s transferRegistrationStore) ListAbandonedTransferUploads(context.Context, string, int32) ([]store.AbandonedTransferUpload, error) {
	return nil, nil
}

func (s transferRegistrationStore) PrepareMediaAssetTransfer(context.Context, int64, string, store.StoragePolicy, []string, []string, []string, int32) (store.MediaAssetTransfer, bool, error) {
	return store.MediaAssetTransfer{}, false, s.transferErr
}

func TestNewWorkersRegistersStorageMaintenanceJobs(t *testing.T) {
	t.Parallel()

	tokyoErr := errors.New("Tokyo breakers queried")
	nagoyaErr := errors.New("Nagoya breakers queried")
	reconcileErr := errors.New("transfer reconcile queried")
	transferErr := errors.New("transfer prepared")
	tokyoRoot, nagoyaRoot := t.TempDir(), t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(tokyoRoot, "tokyo"), "marking Tokyo root")
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(nagoyaRoot, "nagoya"), "marking Nagoya root")
	policy := config.StorageConfig{
		Stores:    map[string]config.StoreConfig{"tokyo": {Kind: "fs"}, "nagoya": {Kind: "fs"}},
		Originals: []config.OriginalStoreRule{{Store: "tokyo"}},
		Derived:   "tokyo",
	}
	fingerprint, err := policy.PolicyFingerprint()
	testutil.RequireNoError(t, err, "fingerprinting storage policy")
	registry := NewWorkers(&Deps{
		Enqueuer:                &jobqfaketest.RecordingEnqueuer{},
		Breakers:                deleteRouteBreakers{errs: map[string]error{"tokyo": tokyoErr, "nagoya": nagoyaErr}},
		UnscopedStorageTransfer: transferRegistrationStore{reconcileErr: reconcileErr, transferErr: transferErr},
		StorageTransferPolicy:   policy,
		StoragePolicy:           policyOf(policy),
		StorageRoots:            map[string]string{"tokyo": tokyoRoot, "nagoya": nagoyaRoot},
		PerStore: map[string]*StoreDeps{
			"tokyo":   {MediaDir: tokyoRoot, MediaStoreName: "tokyo"},
			"nagoya":  {MediaDir: nagoyaRoot, MediaStoreName: "nagoya"},
			"swapped": {MediaDir: tokyoRoot, MediaStoreName: "swapped"},
		},
	})

	for _, tc := range []struct {
		kind, payload, queue string
		wantErr              error
		wantMessage          string
	}{
		{"delete_reconcile", `{"store":"tokyo"}`, "original_tokyo", tokyoErr, ""},
		{"delete_reconcile", `{"store":"nagoya"}`, "original_nagoya", nagoyaErr, ""},
		{"delete_reconcile", `{"store":"swapped"}`, "original_swapped", mediapath.ErrStoreMarkerMismatch, ""},
		{"delete_reconcile", `{"store":"unmounted"}`, "original_unmounted", nil, `store "unmounted" is not among this worker's mounted stores [nagoya swapped tokyo]`},
		{"storage_transfer_reconcile", `{}`, "cleanup", reconcileErr, ""},
		{"storage_transfer", `{"store":"tokyo","media_asset_id":1,"policy_fingerprint":"` + fingerprint + `"}`, "transfer_tokyo", transferErr, ""},
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
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Work error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantMessage != "" && (err == nil || !strings.Contains(err.Error(), tc.wantMessage)) {
				t.Fatalf("Work error = %v, want %q", err, tc.wantMessage)
			}
		})
	}
}
