package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/config"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// policyRecordingStore は、NewWorkers が組み立てた worker の経路で store に渡された方針を記録する。
// 方針が零値のまま届くと、転送も削除も背圧も起きないので、記録した値で配線を確かめる。
type policyRecordingStore struct {
	store.StorageTransfer
	clearPolicies    []store.StoragePolicy
	preparePolicies  []store.StoragePolicy
	backlogPolicies  []store.StoragePolicy
	obsoletePolicies []store.StoragePolicy
}

var errPolicyRecorded = errors.New("policy recorded")

func (s *policyRecordingStore) ListActiveDerivedCopiesInNonDerivedS3(context.Context, []string, string) ([]store.DerivedS3CopyCount, error) {
	return nil, nil
}

func (s *policyRecordingStore) ListAbandonedTransferUploads(context.Context, string, int32) ([]store.AbandonedTransferUpload, error) {
	return nil, nil
}

func (s *policyRecordingStore) ClearObsoleteMediaAssetTransferAttempts(_ context.Context, policy store.StoragePolicy) error {
	s.clearPolicies = append(s.clearPolicies, policy)
	return errPolicyRecorded
}

func (s *policyRecordingStore) PrepareMediaAssetTransfer(_ context.Context, _ int64, _ string, policy store.StoragePolicy, _, _, _ []string, _ int32) (store.MediaAssetTransfer, bool, error) {
	s.preparePolicies = append(s.preparePolicies, policy)
	return store.MediaAssetTransfer{}, false, errPolicyRecorded
}

func (s *policyRecordingStore) DatabaseClock(context.Context) (time.Time, error) {
	return time.Now(), nil
}

func (s *policyRecordingStore) CountMediaAssetTransferBacklogBySourceStore(_ context.Context, policy store.StoragePolicy, _ []string, _ int32) (map[string]int64, error) {
	s.backlogPolicies = append(s.backlogPolicies, policy)
	return nil, errPolicyRecorded
}

func (s *policyRecordingStore) ListObsoletePendingMediaAssetTransferCopies(_ context.Context, policy store.StoragePolicy, _ int32) ([]store.MediaAssetCopyDeleteCandidate, error) {
	s.obsoletePolicies = append(s.obsoletePolicies, policy)
	return nil, errPolicyRecorded
}

// emptyMediaReconcile は encode reconcile が最初に読む候補を空にする。背圧の判定までは進める。
type emptyMediaReconcile struct {
	store.MediaReconcile
}

func (emptyMediaReconcile) ListMissingEncodeProfiles(context.Context, store.MissingEncodeProfilesParams) ([]store.MissingEncodeProfile, error) {
	return nil, nil
}

func (emptyMediaReconcile) OriginalStores(context.Context, []int64) (map[int64]string, error) {
	return map[int64]string{}, nil
}

// TestNewWorkersPassesStoragePolicyToEveryConsumer は、NewWorkers の 4 つの consumer が Deps の方針を受け取ることを
// 実際のジョブ経路で確かめる。期待値は設定から手で書いた値である。
// 配線が抜けると consumer は零値の方針で動き、転送・削除・背圧が黙って止まるので、ここで落とす。
func TestNewWorkersPassesStoragePolicyToEveryConsumer(t *testing.T) {
	t.Parallel()

	tokyoRoot, nagoyaRoot := t.TempDir(), t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(tokyoRoot, "tokyo"), "marking Tokyo root")
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(nagoyaRoot, "nagoya"), "marking Nagoya root")
	cfg := config.StorageConfig{
		Stores:    map[string]config.StoreConfig{"tokyo": {Kind: "fs"}, "nagoya": {Kind: "fs"}},
		Originals: []config.OriginalStoreRule{{Store: "tokyo"}},
		Derived:   "tokyo",
		Transfer:  config.StorageTransferConfig{BacklogLimit: map[string]int{"tokyo": 1}},
	}
	// 期待値は設定から手で書く。FS 置き場は名前順に並ぶ。
	want := store.StoragePolicy{
		Originals: []store.OriginalRule{{Store: "tokyo"}},
		FSStores:  []string{"nagoya", "tokyo"},
		Derived:   "tokyo",
	}
	policy, err := cfg.StoragePolicy()
	testutil.RequireNoError(t, err, "building the storage policy")
	if !reflect.DeepEqual(policy, want) {
		t.Fatalf("StoragePolicy() = %+v, want %+v (test fixture is wrong)", policy, want)
	}

	recorder := &policyRecordingStore{}
	registry := NewWorkers(&Deps{
		Enqueuer:                &jobqfaketest.RecordingEnqueuer{},
		Breakers:                deleteRouteBreakers{},
		UnscopedMediaReconcile:  emptyMediaReconcile{},
		UnscopedStorageTransfer: recorder,
		StorageTransferPolicy:   cfg,
		StoragePolicy:           policy,
		StorageRoots:            map[string]string{"tokyo": tokyoRoot, "nagoya": nagoyaRoot},
		PerStore: map[string]*StoreDeps{
			"tokyo":  {MediaDir: tokyoRoot, MediaStoreName: "tokyo", StorageTransfer: recorder},
			"nagoya": {MediaDir: nagoyaRoot, MediaStoreName: "nagoya", StorageTransfer: recorder},
		},
	})

	fingerprint, err := cfg.PolicyFingerprint()
	testutil.RequireNoError(t, err, "fingerprinting storage policy")
	for _, job := range []struct {
		kind, payload string
	}{
		{"encode_reconcile", `{}`},
		{"storage_transfer_reconcile", `{}`},
		{"storage_transfer", `{"store":"tokyo","media_asset_id":1,"policy_fingerprint":"` + fingerprint + `"}`},
		{"delete_reconcile", `{"store":"tokyo"}`},
	} {
		args, err := registry.Decode(job.kind, []byte(job.payload))
		testutil.RequireNoError(t, err, "decoding "+job.kind)
		err = registry.Work(t.Context(), args)
		if !errors.Is(err, errPolicyRecorded) {
			// The recorded store error is wrapped by each consumer; a different error means the path never reached the store.
			t.Fatalf("%s Work error = %v, want the recorded store error", job.kind, err)
		}
	}

	checks := []struct {
		name     string
		recorded []store.StoragePolicy
	}{
		{"EncodeReconcileWorker (backlog count)", recorder.backlogPolicies},
		{"StorageTransferReconcileWorker (clear obsolete attempts)", recorder.clearPolicies},
		{"StorageTransferWorker (prepare transfer)", recorder.preparePolicies},
		{"DeleteReconcileWorker (obsolete pending transfer copies)", recorder.obsoletePolicies},
	}
	for _, c := range checks {
		if len(c.recorded) != 1 {
			t.Errorf("%s: store got %d calls, want 1", c.name, len(c.recorded))
			continue
		}
		if !reflect.DeepEqual(c.recorded[0], want) {
			t.Errorf("%s: policy = %+v, want %+v", c.name, c.recorded[0], want)
		}
	}
}
