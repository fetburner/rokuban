package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
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
	perStore := make(map[string]*Deps, len(stores))
	for _, name := range stores {
		perStore[name] = &Deps{MediaStoreName: name}
	}
	return newStoreRoute("encode", perStore, func(d *Deps) jobq.Worker[jobs.EncodeJobArgs] {
		return recordingStoreWorker{store: d.MediaStoreName, calls: calls}
	})
}

// 置き場で修飾したキューのジョブは、Args の置き場に束ねたワーカーだけが実行する。
func TestStoreRouteRunsTheWorkerBoundToTheJobsStore(t *testing.T) {
	var calls []string
	route := newTestEncodeRoute(&calls, "tokyo", "takamatsu")
	if err := route.Work(context.Background(), jobs.EncodeJobArgs{Store: "takamatsu", RecordingID: 1, Profile: "p"}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if err := route.Work(context.Background(), jobs.EncodeJobArgs{Store: "tokyo", RecordingID: 2, Profile: "p"}); err != nil {
		t.Fatalf("Work: %v", err)
	}
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
	var calls []string
	route := newTestEncodeRoute(&calls, "tokyo")
	if err := route.Work(context.Background(), jobs.EncodeJobArgs{RecordingID: 1, Profile: "p"}); err == nil || len(calls) != 0 {
		t.Fatalf("Work error = %v, calls = %v; want an error and no worker call", err, calls)
	}
}

type fixedStoreChooser struct{ name string }

func (c fixedStoreChooser) DesiredOriginalStoreAtIngest(context.Context, []byte, string, string) (string, error) {
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

// ingest は、規則に合う置き場がマウント済みならそこへ、そうでなければ --stores の順で先頭のマウント済みの置き場へ書く。
func TestIngestRouteChoosesTheMountedStoreTheRulesSelect(t *testing.T) {
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
			perStore := map[string]*Deps{}
			for _, name := range tc.mounted {
				perStore[name] = &Deps{MediaStoreName: name}
			}
			deps := &Deps{PerStore: perStore, MountedStores: tc.mounted, IngestStores: fixedStoreChooser{tc.chosen}, StorageTransferPolicy: policy}
			route := newIngestRoute(deps, func(d *Deps) jobq.Worker[jobs.IngestJobArgs] {
				return ingestRecorder{store: d.MediaStoreName, calls: &calls}
			})
			if err := route.Work(context.Background(), jobs.IngestJobArgs{Site: "home", RecordID: "r"}); err != nil {
				t.Fatalf("Work: %v", err)
			}
			if len(calls) != 1 || calls[0] != tc.want {
				t.Fatalf("ingest wrote to %v, want literal [%s]", calls, tc.want)
			}
		})
	}
}

// 原本を読むジョブを積む側は、原本のコピーがある置き場を選ぶ（置き場を決め打ちしない）。
// コピーを別の置き場へ移した録画の encode は、その置き場のキューに積まれる。
func TestEncodeReconcileQueuesForTheStoreHoldingTheOriginal(t *testing.T) {
	pool := setupTestPool(t)
	if pool == nil {
		return
	}
	mediaDir := t.TempDir()
	id := prepareEncodeInput(t, pool, dbtest.Recording(t, pool), mediaDir, "route/moved.m2ts", []string{"h265"}, []byte("x"))
	if _, err := pool.Exec(context.Background(), `UPDATE media_asset_copies SET store = 'takamatsu' WHERE store = $1`, dbtest.StoreName); err != nil {
		t.Fatalf("moving the original copy to another store: %v", err)
	}
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

type failingStoreChooser struct{}

func (failingStoreChooser) DesiredOriginalStoreAtIngest(context.Context, []byte, string, string) (string, error) {
	return "", errors.New("rules query failed")
}

// 規則の評価が失敗しても ingest は止まらず、`--stores` の順で先頭のマウント済みの置き場へ書く。
// 名前順では takamatsu が先頭になるので、順序を名前順に戻すとこのテストが落ちる。
func TestIngestRouteFallsBackToTheFirstMountedStoreWhenTheRulesFail(t *testing.T) {
	policy := config.StorageConfig{
		Stores:    map[string]config.StoreConfig{"tokyo": {Kind: "fs"}, "takamatsu": {Kind: "fs"}, "nagoya": {Kind: "fs"}},
		Originals: []config.OriginalStoreRule{{Store: "nagoya"}},
		Derived:   "tokyo",
	}
	var calls []string
	perStore := map[string]*Deps{"tokyo": {MediaStoreName: "tokyo"}, "takamatsu": {MediaStoreName: "takamatsu"}}
	deps := &Deps{PerStore: perStore, MountedStores: []string{"tokyo", "takamatsu"}, IngestStores: failingStoreChooser{}, StorageTransferPolicy: policy}
	route := newIngestRoute(deps, func(d *Deps) jobq.Worker[jobs.IngestJobArgs] {
		return ingestRecorder{store: d.MediaStoreName, calls: &calls}
	})
	if err := route.Work(context.Background(), jobs.IngestJobArgs{Site: "home", RecordID: "r"}); err != nil {
		t.Fatalf("Work = %v, want ingest to proceed past a failed store choice", err)
	}
	if got, want := strings.Join(calls, ","), "tokyo"; got != want {
		t.Fatalf("ingest wrote to %s, want literal %s (the first mounted store in --stores order)", got, want)
	}
}
