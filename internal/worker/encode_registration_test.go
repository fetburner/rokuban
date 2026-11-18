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

type encodeRouteAttempts struct {
	store.EncodeAttempts
	err error
}

func (a encodeRouteAttempts) Begin(context.Context, int64, string, int32) (store.EncodeAttemptStart, error) {
	return store.EncodeAttemptStart{}, a.err
}

func TestNewWorkersRegistersEncodeJobs(t *testing.T) {
	t.Parallel()

	tokyoErr := errors.New("Tokyo attempts queried")
	nagoyaErr := errors.New("Nagoya attempts queried")
	tokyoRoot, nagoyaRoot := t.TempDir(), t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(tokyoRoot, "tokyo"), "marking Tokyo root")
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(nagoyaRoot, "nagoya"), "marking Nagoya root")
	registry := NewWorkers(&Deps{PerStore: map[string]*Deps{
		"tokyo":   {MediaDir: tokyoRoot, MediaStoreName: "tokyo", EncodeAttempts: encodeRouteAttempts{err: tokyoErr}},
		"nagoya":  {MediaDir: nagoyaRoot, MediaStoreName: "nagoya", EncodeAttempts: encodeRouteAttempts{err: nagoyaErr}},
		"swapped": {MediaDir: tokyoRoot, MediaStoreName: "swapped", EncodeAttempts: encodeRouteAttempts{err: tokyoErr}},
	}})

	for _, tc := range []struct {
		kind, payload, queue string
		wantErr              error
		wantMessage          string
	}{
		{"encode", `{"store":"tokyo","recording_id":1,"profile":"h264"}`, "encode_tokyo", tokyoErr, ""},
		{"encode", `{"store":"nagoya","recording_id":1,"profile":"h264"}`, "encode_nagoya", nagoyaErr, ""},
		{"encode", `{"store":"swapped","recording_id":1,"profile":"h264"}`, "encode_swapped", mediapath.ErrStoreMarkerMismatch, ""},
		{"encode", `{"store":"unmounted","recording_id":1,"profile":"h264"}`, "encode_unmounted", nil, `store "unmounted" is not among this worker's mounted stores [nagoya swapped tokyo]`},
		{"encode_reconcile", `{}`, "cleanup", nil, "encode reconcile: job enqueuer is nil"},
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
