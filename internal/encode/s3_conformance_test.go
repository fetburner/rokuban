//go:build s3conformance

package encode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore/s3store"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestS3EncodeRetryReservesNewGeneration は、実際の S3 API の実装（CI では RustFS）で、
// PUT 後に中断した encode の再試行が新しい世代を公開し、先のコピーを pending のまま残すことを測る。
func TestS3EncodeRetryReservesNewGeneration(t *testing.T) {
	ctx := t.Context()
	endpoint := os.Getenv("ROKUBAN_S3_CONFORMANCE_ENDPOINT")
	bucket := os.Getenv("ROKUBAN_S3_CONFORMANCE_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("ROKUBAN_S3_CONFORMANCE_ENDPOINT and ROKUBAN_S3_CONFORMANCE_BUCKET are required")
	}
	region := os.Getenv("ROKUBAN_S3_CONFORMANCE_REGION")
	if region == "" {
		region = "us-east-1"
	}
	objects, err := s3store.New(ctx, s3store.Config{Endpoint: endpoint, Region: region, Bucket: bucket})
	testutil.RequireNoError(t, err, "creating S3 object store")
	env := newObjectEnvWith(t, objects)
	pool := env.pool
	media := env.media

	recordingID := dbtest.Recording(t, pool)
	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName), "writing local media store marker")
	sourceRel := fmt.Sprintf("s3-retry-%d/source.m2ts", time.Now().UnixNano())
	prepareEncodeInput(t, pool, recordingID, mediaDir, sourceRel, []string{"h264"}, []byte("original source"))
	initialKey, err := EncodedRelPath(sourceRel, "h264", "mp4", 0, false)
	testutil.RequireNoError(t, err, "building initial encoded key")
	retryKey, err := EncodedRelPath(sourceRel, "h264", "mp4", 1, false)
	testutil.RequireNoError(t, err, "building retry encoded key")
	t.Cleanup(func() {
		for _, key := range []string{initialKey, retryKey} {
			if err := objects.Delete(context.Background(), key); err != nil {
				t.Errorf("deleting S3 test key %q: %v", key, err)
			}
		}
	})

	worker := newH264Worker(pool, mediaDir, t.TempDir(), installCountingFFmpeg(t))
	worker.MediaStoreName = dbtest.StoreName
	worker.Objects = objects
	worker.ObjectPublisher = media
	worker.Media = media
	worker.Attempts = media

	originalHook := beforeEncodeCommit
	t.Cleanup(func() { beforeEncodeCommit = originalHook })
	failOnce := true
	beforeEncodeCommit = func(string) error {
		if failOnce {
			failOnce = false
			return errors.New("simulated stop after PUT and before DB publication")
		}
		return nil
	}
	args := jobs.EncodeJobArgs{RecordingID: recordingID, Profile: "h264"}
	if err := worker.Work(ctx, args); err == nil {
		t.Fatal("first encode succeeded, want a failure after the S3 PUT")
	}
	firstBytes := env.readObject(t, initialKey)
	testutil.RequireNoError(t, worker.Work(ctx, args), "retrying encode with a new generation")
	secondBytes := env.readObject(t, retryKey)
	if bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("two ffmpeg attempts produced identical fixtures %q; want differing bytes", firstBytes)
	}
	if got := env.copyState(t, retryKey); got != "active" {
		t.Fatalf("retry copy state = %q, want active", got)
	}
	if got := env.copyState(t, initialKey); got != "pending" {
		t.Errorf("first copy state = %q, want pending", got)
	}
}
