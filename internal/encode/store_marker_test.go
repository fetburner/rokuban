package encode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestGeneratedObjectPublishDoesNotWriteWhenStoreMarkerIsMissing(t *testing.T) {
	t.Parallel()
	mediaDir := t.TempDir()
	worker := &EncodeWorker{MediaDir: mediaDir, MediaStoreName: "tokyo"}
	_, _, err := worker.publishEncoded(context.Background(), encodePublishInput{})
	if err == nil {
		t.Fatal("publishEncoded() succeeded without a store marker")
	}
	// fsstore.Put へ達する前に止まり、派生物ディレクトリも作らない。
	if _, err := os.Stat(filepath.Join(mediaDir, "thumbnails")); !os.IsNotExist(err) {
		t.Fatalf("generated output directory exists after marker rejection: stat error = %v", err)
	}
}

type markerGuardPublisher struct {
	store.DerivedMediaPublisher
}

func (*markerGuardPublisher) PreparePendingMediaAssetCopy(context.Context, store.PendingMediaAssetCopy) error {
	return nil
}

// hash の後に目印が消えたら、オブジェクト置き場へ Put する前に検出する。
func TestPublishEncodedRechecksStoreMarkerImmediatelyBeforePut(t *testing.T) {
	for _, state := range []string{"missing", "different store"} {
		t.Run(state, func(t *testing.T) {
			mediaDir := t.TempDir()
			testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, "tokyo"), "mediapath.WriteStoreMarker")
			scratch := filepath.Join(t.TempDir(), "out.mp4")
			testutil.RequireNoError(t, os.WriteFile(scratch, []byte("data"), 0o600), "os.WriteFile")
			relPath := "sites/tokyo/out.mp4"
			w := &EncodeWorker{
				Objects: testFSObjectStore(mediaDir), ObjectPublisher: &markerGuardPublisher{},
				MediaDir: mediaDir, MediaStoreName: "tokyo",
			}
			previous := beforeEncodeObjectPut
			beforeEncodeObjectPut = func(string) { mismatchStoreMarker(t, mediaDir, state) }
			t.Cleanup(func() { beforeEncodeObjectPut = previous })
			metricBefore := promtestutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo"))

			_, published, err := w.publishEncoded(context.Background(), encodePublishInput{
				relPath: relPath, scratchOut: scratch,
			})
			if !errors.Is(err, mediapath.ErrStoreMarkerMismatch) || published {
				t.Fatalf("publishEncoded() = published:%t err:%v, want marker mismatch", published, err)
			}
			if got := promtestutil.ToFloat64(metrics.StoreMarkerMismatch.WithLabelValues("tokyo")); got != metricBefore+1 {
				t.Errorf("store marker metric = %v, want %v", got, metricBefore+1)
			}
			if _, err := os.Stat(filepath.Join(mediaDir, filepath.FromSlash(relPath))); !os.IsNotExist(err) {
				t.Errorf("object exists after marker rejection (stat error = %v)", err)
			}
		})
	}
}

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
