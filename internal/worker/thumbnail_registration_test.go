package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

type thumbnailRouteMedia struct {
	store.MediaReconcile
	err error
}

func (m thumbnailRouteMedia) HasActiveThumbnailMediaAsset(context.Context, int64) (bool, error) {
	return false, m.err
}

func (m thumbnailRouteMedia) HasActiveSeekTilesMediaAsset(context.Context, int64) (bool, error) {
	return false, m.err
}

func TestNewWorkersRegistersThumbnailJobs(t *testing.T) {
	t.Parallel()

	tokyoErr := errors.New("Tokyo media queried")
	nagoyaErr := errors.New("Nagoya media queried")
	unscopedErr := errors.New("unscoped media queried")
	tokyoRoot, nagoyaRoot := t.TempDir(), t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(tokyoRoot, "tokyo"), "marking Tokyo root")
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(nagoyaRoot, "nagoya"), "marking Nagoya root")
	registry := NewWorkers(&Deps{
		UnscopedMediaReconcile: thumbnailRouteMedia{err: unscopedErr},
		PerStore: map[string]*StoreDeps{
			"tokyo":   {MediaDir: tokyoRoot, MediaStoreName: "tokyo", MediaReconcile: thumbnailRouteMedia{err: tokyoErr}},
			"nagoya":  {MediaDir: nagoyaRoot, MediaStoreName: "nagoya", MediaReconcile: thumbnailRouteMedia{err: nagoyaErr}},
			"swapped": {MediaDir: tokyoRoot, MediaStoreName: "swapped", MediaReconcile: thumbnailRouteMedia{err: tokyoErr}},
		},
	})

	for _, tc := range []struct {
		kind, payload, queue string
		wantErr              error
		wantMessage          string
	}{
		{"thumbnail", `{"store":"tokyo","recording_id":1}`, "thumbnail_tokyo", tokyoErr, ""},
		{"thumbnail", `{"store":"nagoya","recording_id":1}`, "thumbnail_nagoya", nagoyaErr, ""},
		{"seek_tiles", `{"store":"tokyo","recording_id":1}`, "thumbnail_tokyo", tokyoErr, ""},
		{"seek_tiles", `{"store":"nagoya","recording_id":1}`, "thumbnail_nagoya", nagoyaErr, ""},
		{"thumbnail", `{"store":"swapped","recording_id":1}`, "thumbnail_swapped", mediapath.ErrStoreMarkerMismatch, ""},
		{"seek_tiles", `{"store":"swapped","recording_id":1}`, "thumbnail_swapped", mediapath.ErrStoreMarkerMismatch, ""},
		{"thumbnail", `{"store":"unmounted","recording_id":1}`, "thumbnail_unmounted", nil, `store "unmounted" is not among this worker's mounted stores [nagoya swapped tokyo]`},
		{"seek_tiles", `{"store":"unmounted","recording_id":1}`, "thumbnail_unmounted", nil, `store "unmounted" is not among this worker's mounted stores [nagoya swapped tokyo]`},
		{"thumbnail_reconcile", `{}`, "cleanup", nil, "thumbnail reconcile: job enqueuer is nil"},
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
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Work error = %v, want %v", err, tc.wantErr)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("Work error = %v, want %q", err, tc.wantMessage)
			}
		})
	}
}
