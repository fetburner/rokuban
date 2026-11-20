package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// recordingStoreWorker は、どの置き場のワーカーが呼ばれたかを記録する。
type recordingStoreWorker struct {
	store string
	calls *[]string
}

func (w recordingStoreWorker) Work(context.Context, jobs.EncodeJobArgs) error {
	*w.calls = append(*w.calls, w.store)
	return nil
}

func newTestEncodeRoute(calls *[]string, stores ...string) *storeRoute[jobs.EncodeJobArgs] {
	perStore := make(map[string]*StoreDeps, len(stores))
	for _, name := range stores {
		perStore[name] = &StoreDeps{MediaStoreName: name}
	}
	return newStoreRoute("encode", &Deps{}, perStore, func(_ *Deps, d *StoreDeps) jobq.Worker[jobs.EncodeJobArgs] {
		return recordingStoreWorker{store: d.MediaStoreName, calls: calls}
	})
}

// 置き場で修飾したキューのジョブは、Args の置き場に束ねたワーカーだけが実行する。
func TestStoreRouteRunsTheWorkerBoundToTheJobsStore(t *testing.T) {
	t.Parallel()
	var calls []string
	route := newTestEncodeRoute(&calls, "tokyo", "takamatsu")
	testutil.RequireNoError(t, route.Work(context.Background(), jobs.EncodeJobArgs{Store: "takamatsu", RecordingID: 1, Profile: "p"}), "Work")
	testutil.RequireNoError(t, route.Work(context.Background(), jobs.EncodeJobArgs{Store: "tokyo", RecordingID: 2, Profile: "p"}), "Work")
	if got, want := strings.Join(calls, ","), "takamatsu,tokyo"; got != want {
		t.Fatalf("workers called = %s, want literal %s", got, want)
	}
}

// マウントしていない置き場のジョブは、ファイルシステムに触れる前に失敗にし、メトリクスを出す。
func TestStoreRouteFailsAndCountsAJobForAnUnmountedStore(t *testing.T) {
	var calls []string
	route := newTestEncodeRoute(&calls, "tokyo")
	before := promtestutil.ToFloat64(metrics.StoreRouteMismatches.WithLabelValues("encode", "takamatsu"))
	err := route.Work(context.Background(), jobs.EncodeJobArgs{Store: "takamatsu", RecordingID: 1, Profile: "p"})
	if err == nil || !strings.Contains(err.Error(), `store "takamatsu" is not among this worker's mounted stores [tokyo]`) {
		t.Fatalf("Work error = %v, want a mounted-store mismatch naming takamatsu and [tokyo]", err)
	}
	if len(calls) != 0 {
		t.Fatalf("a worker ran for an unmounted store: %v", calls)
	}
	if got := promtestutil.ToFloat64(metrics.StoreRouteMismatches.WithLabelValues("encode", "takamatsu")); got != before+1 {
		t.Fatalf("mismatch counter = %v, want %v", got, before+1)
	}
}

// 空の置き場（投入側の選び損ね）はどの置き場の名前にも一致しない。
func TestStoreRouteRejectsAnEmptyStore(t *testing.T) {
	t.Parallel()
	var calls []string
	route := newTestEncodeRoute(&calls, "tokyo")
	if err := route.Work(context.Background(), jobs.EncodeJobArgs{RecordingID: 1, Profile: "p"}); err == nil || len(calls) != 0 {
		t.Fatalf("Work error = %v, calls = %v; want an error and no worker call", err, calls)
	}
}

type fixedStoreChooser struct{ name string }

func (c fixedStoreChooser) DesiredOriginalStoreAtIngest(context.Context, store.StoragePolicy, string) (string, error) {
	return c.name, nil
}

type ingestRecorder struct {
	store string
	calls *[]string
}

func (w ingestRecorder) Work(context.Context, jobs.IngestJobArgs) error {
	*w.calls = append(*w.calls, w.store)
	return nil
}

// fallback の destination は記録し、desired を使える場合は fallback のログを出さない。
func TestIngestRouteLogsFallbackDestination(t *testing.T) {
	policy := config.StorageConfig{
		Stores:    map[string]config.StoreConfig{"tokyo": {Kind: "fs"}, "nagoya": {Kind: "fs"}},
		Originals: []config.OriginalStoreRule{{Store: "nagoya"}},
		Derived:   "tokyo",
	}
	tests := []struct {
		name    string
		chosen  string
		mounted []string
		want    string
		fields  []string
	}{
		{
			name:    "desired store is not mounted",
			chosen:  "nagoya",
			mounted: []string{"tokyo"},
			want:    "tokyo",
			fields:  []string{"level=WARN", "desired_store=nagoya", "actual_store=tokyo"},
		},
		{
			name:    "rules select no store",
			mounted: []string{"tokyo"},
			want:    "tokyo",
			fields:  []string{"level=WARN", `desired_store=""`, "actual_store=tokyo"},
		},
		{
			name:    "desired store is mounted",
			chosen:  "nagoya",
			mounted: []string{"tokyo", "nagoya"},
			want:    "nagoya",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			perStore := make(map[string]*StoreDeps, len(tt.mounted))
			for _, name := range tt.mounted {
				perStore[name] = &StoreDeps{MediaStoreName: name}
			}
			var calls []string
			deps := &Deps{
				PerStore:              perStore,
				MountedStores:         tt.mounted,
				IngestStores:          fixedStoreChooser{tt.chosen},
				StorageTransferPolicy: policy, StoragePolicy: policyOf(policy),
			}
			route := newIngestRoute(deps, func(_ *Deps, d *StoreDeps) jobq.Worker[jobs.IngestJobArgs] {
				return ingestRecorder{store: d.MediaStoreName, calls: &calls}
			})
			testutil.RequireNoError(t, route.Work(context.Background(), jobs.IngestJobArgs{Site: "home", RecordID: "r"}), "Work")
			if got := strings.Join(calls, ","); got != tt.want {
				t.Fatalf("ingest wrote to %q, want literal %q", got, tt.want)
			}
			if len(tt.fields) == 0 {
				if got := logs.String(); got != "" {
					t.Fatalf("logs = %q, want no fallback log", got)
				}
				return
			}
			for _, field := range tt.fields {
				if !strings.Contains(logs.String(), field) {
					t.Errorf("log %q does not include %q", logs.String(), field)
				}
			}
		})
	}
}

// ingest は、規則に合う置き場がマウント済みならそこへ、そうでなければ --stores の順で先頭のマウント済みの置き場へ書く。
func TestIngestRouteChoosesTheMountedStoreTheRulesSelect(t *testing.T) {
	t.Parallel()
	policy := config.StorageConfig{
		Stores:    map[string]config.StoreConfig{"tokyo": {Kind: "fs"}, "takamatsu": {Kind: "fs"}, "nagoya": {Kind: "fs"}},
		Originals: []config.OriginalStoreRule{{Store: "nagoya"}},
		Derived:   "tokyo",
	}
	for _, tc := range []struct {
		name    string
		chosen  string
		mounted []string
		want    string
	}{
		{"rule store is mounted", "takamatsu", []string{"tokyo", "takamatsu"}, "takamatsu"},
		{"rule store is not mounted, first mounted store in --stores order", "nagoya", []string{"tokyo", "takamatsu"}, "tokyo"},
		{"rule evaluates to nothing", "", []string{"tokyo"}, "tokyo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			perStore := map[string]*StoreDeps{}
			for _, name := range tc.mounted {
				perStore[name] = &StoreDeps{MediaStoreName: name}
			}
			deps := &Deps{PerStore: perStore, MountedStores: tc.mounted, IngestStores: fixedStoreChooser{tc.chosen}, StorageTransferPolicy: policy, StoragePolicy: policyOf(policy)}
			route := newIngestRoute(deps, func(_ *Deps, d *StoreDeps) jobq.Worker[jobs.IngestJobArgs] {
				return ingestRecorder{store: d.MediaStoreName, calls: &calls}
			})
			testutil.RequireNoError(t, route.Work(context.Background(), jobs.IngestJobArgs{Site: "home", RecordID: "r"}), "Work")
			if len(calls) != 1 || calls[0] != tc.want {
				t.Fatalf("ingest wrote to %v, want literal [%s]", calls, tc.want)
			}
		})
	}
}

type failingStoreChooser struct{}

func (failingStoreChooser) DesiredOriginalStoreAtIngest(context.Context, store.StoragePolicy, string) (string, error) {
	return "", errors.New("rules query failed")
}

// 規則の評価が失敗しても ingest は止まらず、`--stores` の順で先頭のマウント済みの置き場へ書く。
// 名前順では takamatsu が先頭になるので、順序を名前順に戻すとこのテストが落ちる。
func TestIngestRouteFallsBackToTheFirstMountedStoreWhenTheRulesFail(t *testing.T) {
	t.Parallel()
	policy := config.StorageConfig{
		Stores:    map[string]config.StoreConfig{"tokyo": {Kind: "fs"}, "takamatsu": {Kind: "fs"}, "nagoya": {Kind: "fs"}},
		Originals: []config.OriginalStoreRule{{Store: "nagoya"}},
		Derived:   "tokyo",
	}
	var calls []string
	perStore := map[string]*StoreDeps{"tokyo": {MediaStoreName: "tokyo"}, "takamatsu": {MediaStoreName: "takamatsu"}}
	deps := &Deps{PerStore: perStore, MountedStores: []string{"tokyo", "takamatsu"}, IngestStores: failingStoreChooser{}, StorageTransferPolicy: policy, StoragePolicy: policyOf(policy)}
	route := newIngestRoute(deps, func(_ *Deps, d *StoreDeps) jobq.Worker[jobs.IngestJobArgs] {
		return ingestRecorder{store: d.MediaStoreName, calls: &calls}
	})
	if err := route.Work(context.Background(), jobs.IngestJobArgs{Site: "home", RecordID: "r"}); err != nil {
		t.Fatalf("Work = %v, want ingest to proceed past a failed store choice", err)
	}
	if got, want := strings.Join(calls, ","), "tokyo"; got != want {
		t.Fatalf("ingest wrote to %s, want literal %s (the first mounted store in --stores order)", got, want)
	}
}

// policyOf は設定から置き場の方針を作る。設定が不正なテストの前提は作れないので、失敗は panic にする。
func policyOf(cfg config.StorageConfig) store.StoragePolicy {
	policy, err := cfg.StoragePolicy()
	if err != nil {
		panic(err)
	}
	return policy
}
