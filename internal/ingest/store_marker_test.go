package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestIngestWorkerWithholdsWhenStoreMarkerDoesNotMatch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		markerPresent bool
		writeStore    string
	}{
		{name: "missing"},
		{name: "different store", markerPresent: true, writeStore: "osaka"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mediaDir := t.TempDir()
			if tc.writeStore != "" {
				testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, tc.writeStore), "mediapath.WriteStoreMarker")
			}
			worker := &IngestWorker{
				MirakcClients:  map[string]*mirakc.Client{"tokyo": nil},
				MediaDir:       mediaDir,
				MediaStoreName: "tokyo",
			}
			before := promtestutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))
			err := worker.Work(context.Background(), jobs.IngestJobArgs{Site: "tokyo"})
			if err == nil {
				t.Fatal("Work() succeeded with an unverified media store, want retryable error")
			}
			if got := promtestutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != before+1 {
				t.Errorf("store marker metric = %v, want %v", got, before+1)
			}
			if entries, err := os.ReadDir(mediaDir); err != nil {
				t.Fatal(err)
			} else {
				wantEntries := 0
				if tc.markerPresent {
					wantEntries = 1
					if len(entries) > 0 && entries[0].Name() != mediapath.StoreMarkerFileName {
						t.Errorf("media directory entry = %q, want only marker %q", entries[0].Name(), mediapath.StoreMarkerFileName)
					}
				}
				if len(entries) != wantEntries {
					t.Errorf("media directory entries = %v, want %d entries and no ingest writes", entries, wantEntries)
				}
			}
		})
	}
}

type ingestCheckpointSaveCapture struct {
	store.MediaPublish
	saves int
}

func (c *ingestCheckpointSaveCapture) SaveIngestCheckpoint(context.Context, int64, uuid.UUID, int64, int64, []byte) error {
	c.saves++
	return nil
}

func TestIngestWriterHandoffWithholdsCheckpointWhenMarkerDisappears(t *testing.T) {
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "tokyo"), "mediapath.WriteStoreMarker")
	tempPath := filepath.Join(mediaDir, "sites", "tokyo", ".rokuban-ingest-test-g1")
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(tempPath), 0o755), "os.MkdirAll")
	testutil.RequireNoError(t, os.WriteFile(tempPath, []byte("partial ingest data"), 0o600), "os.WriteFile")
	file, err := os.OpenFile(tempPath, os.O_RDWR, 0o600)
	testutil.RequireNoError(t, err, "os.OpenFile")
	defer func() { _ = file.Close() }()
	capture := &ingestCheckpointSaveCapture{}
	worker := &IngestWorker{Publisher: capture, MediaDir: mediaDir, MediaStoreName: "tokyo"}
	metricBefore := promtestutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))
	mismatchStoreMarker(t, mediaDir, "missing")
	err = worker.saveIngestWriterHandoff(context.Background(), file, 1, uuid.New(), 1, int64(len("partial ingest data")), nil)
	if !errors.Is(err, mediapath.ErrStoreMarkerMismatch) {
		t.Fatalf("saving handoff after marker disappeared = %v, want marker mismatch", err)
	}
	if capture.saves != 0 {
		t.Fatalf("SaveIngestCheckpoint calls = %d, want 0 without the matching marker", capture.saves)
	}
	if got := promtestutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
		t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
	}
}

type failingIngestTarget struct{ calls int }

func (f *failingIngestTarget) GetRecordSyncIngestTarget(context.Context, string, string) (store.RecordIngestTarget, error) {
	f.calls++
	return store.RecordIngestTarget{}, errors.New("stop after the storage probe")
}

// 起動時に probe を保留した worker は、目印の照合が通った最初の ingest で probe を走らせる。
// probe の失敗は固定せず、次のジョブが再試行する。
func TestIngestWorkerProbesStorageAfterMarkerCheckAndRetriesFailure(t *testing.T) {
	t.Parallel()
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "tokyo"), "mediapath.WriteStoreMarker")
	target := &failingIngestTarget{}
	worker := &IngestWorker{
		MirakcClients:  map[string]*mirakc.Client{"tokyo": nil},
		RecordTarget:   target,
		MediaDir:       mediaDir,
		MediaStoreName: "tokyo",
	}
	args := jobs.IngestJobArgs{Site: "tokyo", RecordID: "r1"}

	// probe は media_dir/sites を作る。通常ファイルが居座っていると失敗する。
	sitesPath := filepath.Join(mediaDir, "sites")
	testutil.RequireNoError(t, os.WriteFile(sitesPath, []byte("not a directory"), 0o600), "os.WriteFile")
	err := worker.Work(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "validating ingest storage") {
		t.Fatalf("Work() with a failing probe error = %v, want validating ingest storage", err)
	}
	if target.calls != 0 {
		t.Fatalf("ingest target lookups after a failed probe = %d, want 0", target.calls)
	}

	testutil.RequireNoError(t, os.Remove(sitesPath), "os.Remove")
	err = worker.Work(context.Background(), args)
	if err == nil || strings.Contains(err.Error(), "validating ingest storage") {
		t.Fatalf("Work() after the probe recovered error = %v, want the later lookup error", err)
	}
	if target.calls != 1 {
		t.Fatalf("ingest target lookups after the probe recovered = %d, want 1", target.calls)
	}
	if info, err := os.Stat(sitesPath); err != nil || !info.IsDir() {
		t.Fatalf("media_dir/sites after the probe = %v, %v, want a directory", info, err)
	}

	// 成功した後は probe を繰り返さない。sites を通常ファイルに戻しても probe は走らない。
	testutil.RequireNoError(t, os.RemoveAll(sitesPath), "os.RemoveAll")
	testutil.RequireNoError(t, os.WriteFile(sitesPath, []byte("not a directory"), 0o600), "os.WriteFile")
	err = worker.Work(context.Background(), args)
	if err == nil || strings.Contains(err.Error(), "validating ingest storage") {
		t.Fatalf("Work() after a successful probe error = %v, want the lookup error without a second probe", err)
	}
}

// 起動時に probe が成功していれば、最初の ingest で二重に走らせない。
func TestIngestWorkerSkipsStorageProbeAlreadyDoneAtStartup(t *testing.T) {
	t.Parallel()
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "tokyo"), "mediapath.WriteStoreMarker")
	testutil.RequireNoError(t, os.WriteFile(filepath.Join(mediaDir, "sites"), []byte("not a directory"), 0o600), "os.WriteFile")
	target := &failingIngestTarget{}
	worker := &IngestWorker{
		MirakcClients:  map[string]*mirakc.Client{"tokyo": nil},
		RecordTarget:   target,
		MediaDir:       mediaDir,
		MediaStoreName: "tokyo",
		StorageProbed:  true,
	}
	if err := worker.Work(context.Background(), jobs.IngestJobArgs{Site: "tokyo", RecordID: "r1"}); err == nil || strings.Contains(err.Error(), "validating ingest storage") {
		t.Fatalf("Work() error = %v, want the lookup error without a probe", err)
	}
	if target.calls != 1 {
		t.Fatalf("ingest target lookups = %d, want 1", target.calls)
	}
}

// 走査の結果を孤児候補として DB へ書く直前に、目印が消えていないことを照合する。

func mismatchStoreMarker(t *testing.T, mediaDir, state string) {
	t.Helper()
	markerPath := filepath.Join(mediaDir, mediapath.StoreMarkerFileName)
	switch state {
	case "missing":
		if err := os.Remove(markerPath); err != nil {
			t.Errorf("removing store marker: %v", err)
		}
	case "different store":
		if err := os.WriteFile(markerPath, []byte("osaka"), 0o600); err != nil {
			t.Errorf("changing store marker: %v", err)
		}
	default:
		t.Errorf("unknown marker mismatch state %q", state)
	}
}
