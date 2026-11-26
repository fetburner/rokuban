//go:build s3conformance

package thumbnail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/s3store"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// TestS3ThumbnailReselectionReadsEncodedInputFromS3 は S3 にしかない入力で再選択し、FS に公開する。
func TestS3ThumbnailReselectionReadsEncodedInputFromS3(t *testing.T) {
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
	pool := testutil.SetupDB(t)
	const objectStoreName = "s3main"
	media := pgstore.NewMediaInStore(pool, dbtest.StoreName, store.NewStoragePolicy(nil, []string{dbtest.StoreName}, objectStoreName))
	prefix := fmt.Sprintf("thumbnails/s3-conformance-%d", time.Now().UnixNano())
	thumbnailInputKey := fmt.Sprintf("encoded/s3-conformance-%d-thumbnail-source.mp4", time.Now().UnixNano())
	t.Cleanup(func() {
		if err := objects.Delete(context.Background(), thumbnailInputKey); err != nil {
			t.Errorf("deleting S3 test key %q: %v", thumbnailInputKey, err)
		}
	})
	recordingID := dbtest.Recording(t, pool)
	profile := "h264"
	encodedPayload := []byte("remote encoded thumbnail input")
	encodedDigest := sha256.Sum256(encodedPayload)
	encodedID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind, p.Profile, p.RelPath, p.SizeBytes = store.AssetKindEncoded, &profile, thumbnailInputKey, int64(len(encodedPayload))
	})
	testutil.MustExec(t, pool, "placing the encoded copy in the S3 store", `UPDATE media_asset_copies SET store = $2 WHERE media_asset_id = $1`, encodedID, objectStoreName)
	testutil.RequireNoError(t, objects.Put(ctx, thumbnailInputKey, strings.NewReader(string(encodedPayload)), int64(len(encodedPayload)), encodedDigest, "video/mp4"), "uploading encoded thumbnail input")

	oldThumbnailKey := prefix + "-reselect.jpg"
	newThumbnailKey := fmt.Sprintf("thumbnails/%d.g1.jpg", recordingID)
	t.Cleanup(func() {
		if err := objects.Delete(context.Background(), oldThumbnailKey); err != nil {
			t.Errorf("deleting S3 test key %q: %v", oldThumbnailKey, err)
		}
	})
	oldThumbnail := []byte("old thumbnail")
	oldDigest := sha256.Sum256(oldThumbnail)
	thumbnailID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind, p.RelPath, p.SizeBytes = store.AssetKindThumbnail, oldThumbnailKey, int64(len(oldThumbnail))
	})
	testutil.MustExec(t, pool, "placing the thumbnail copy in the S3 store", `UPDATE media_asset_copies SET store = $2 WHERE media_asset_id = $1`, thumbnailID, objectStoreName)
	setThumbnailSeek(t, pool, thumbnailID, 30000)
	// 状態: active な原本が無い（検出の後で原本が削除された）。CM 検出は原本が実体を持つ時点で必要なので、先に作って検出してから削除する。
	seedDeletedOriginalCMRanges(t, pool, t.TempDir(), recordingID, []chapters.Range{{StartMs: 0, EndMs: 60000}})
	testutil.RequireNoError(t, objects.Put(ctx, oldThumbnailKey, strings.NewReader(string(oldThumbnail)), int64(len(oldThumbnail)), oldDigest, "image/jpeg"), "uploading current thumbnail")

	mediaDir := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(mediaDir, dbtest.StoreName), "writing local media store marker")
	local := testFSObjectStore(mediaDir)
	verifiedRemoteInput := false
	worker := &ThumbnailWorker{
		Media: media, Objects: local, ObjectPublisher: media, MediaDir: mediaDir,
		InputObjects:   map[string]objstore.Store{objectStoreName: objects, dbtest.StoreName: local},
		MediaStoreName: dbtest.StoreName, ScratchDir: t.TempDir(),
		runCmd: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if containsArg(args, "format=duration") {
				t.Fatal("chapter-aware reselection must not probe duration")
			}
			inputIndex := indexOfArg(args, "-i")
			if inputIndex < 0 || inputIndex+1 >= len(args) {
				t.Fatalf("ffmpeg args have no input path: %v", args)
			}
			got, err := os.ReadFile(args[inputIndex+1])
			if err != nil {
				t.Fatalf("reading materialized S3 thumbnail input %q: %v", args[inputIndex+1], err)
			}
			if !bytes.Equal(got, encodedPayload) {
				t.Fatalf("materialized thumbnail input = %q, want remote object bytes %q", got, encodedPayload)
			}
			verifiedRemoteInput = true
			testutil.RequireNoError(t, os.WriteFile(args[len(args)-1], tinyJPEG, 0o600), "writing generated thumbnail")
			return nil, nil
		},
	}
	testutil.RequireNoError(t, worker.Work(ctx, jobs.ThumbnailJobArgs{RecordingID: recordingID}), "reselecting thumbnail from S3 encoded input")
	if !verifiedRemoteInput {
		t.Fatal("thumbnail generation did not read the S3 encoded input")
	}
	row := requireThumbnailState(t, pool, recordingID)
	if row.RelPath != newThumbnailKey || row.SeekMs == nil || *row.SeekMs != 89993 {
		t.Fatalf("replacement thumbnail = (%q, %v), want (%q, 89993)", row.RelPath, row.SeekMs, newThumbnailKey)
	}
	object, err := local.Head(ctx, newThumbnailKey)
	if err != nil || object.Size != int64(len(tinyJPEG)) {
		t.Fatalf("replacement FS thumbnail = (%+v, %v), want published JPEG", object, err)
	}
}
