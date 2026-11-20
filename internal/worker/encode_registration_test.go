package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/encode"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testutil"
)

type encodeRouteAttempts struct {
	store.EncodeAttempts
	start store.EncodeAttemptStart
	err   error
}

func (a encodeRouteAttempts) Begin(context.Context, int64, string, int32) (store.EncodeAttemptStart, error) {
	return a.start, a.err
}

func (encodeRouteAttempts) Fail(context.Context, int64, string, int32, string) (bool, error) {
	return false, nil
}

type encodeRouteMedia struct {
	store.MediaReconcile
	err error
}

func (m encodeRouteMedia) GetEncodedPublishState(context.Context, int64, string, bool, bool) (store.EncodedPublishState, error) {
	return store.EncodedPublishState{}, m.err
}

func TestNewWorkersRegistersEncodeJobs(t *testing.T) {
	t.Parallel()

	tokyoErr := errors.New("Tokyo attempts queried")
	nagoyaErr := errors.New("Nagoya attempts queried")
	tokyoRoot, nagoyaRoot := t.TempDir(), t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(tokyoRoot, "tokyo"), "marking Tokyo root")
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(nagoyaRoot, "nagoya"), "marking Nagoya root")
	registry := NewWorkers(&Deps{PerStore: map[string]*StoreDeps{
		"tokyo":   {MediaDir: tokyoRoot, MediaStoreName: "tokyo", Encode: encode.Stores{Attempts: encodeRouteAttempts{err: tokyoErr}}},
		"nagoya":  {MediaDir: nagoyaRoot, MediaStoreName: "nagoya", Encode: encode.Stores{Attempts: encodeRouteAttempts{err: nagoyaErr}}},
		"swapped": {MediaDir: tokyoRoot, MediaStoreName: "swapped", Encode: encode.Stores{Attempts: encodeRouteAttempts{err: tokyoErr}}},
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

func TestNewWorkersEncodeUsesStoreScopedMedia(t *testing.T) {
	t.Parallel()

	unscopedErr := errors.New("unscoped media queried")
	scopedErr := errors.New("store-scoped media queried")
	root := t.TempDir()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(root, "tokyo"), "marking Tokyo root")
	registry := NewWorkers(&Deps{
		UnscopedMediaReconcile: encodeRouteMedia{err: unscopedErr},
		Encode:                 config.EncodeConfig{Profiles: []config.EncodeProfile{{Name: "h264"}}},
		PerStore: map[string]*StoreDeps{
			"tokyo": {
				MediaDir:       root,
				MediaStoreName: "tokyo",
				MediaReconcile: encodeRouteMedia{err: scopedErr},
				Encode:         encode.Stores{Attempts: encodeRouteAttempts{start: store.EncodeAttemptStart{Count: 1}}},
			},
		},
	})

	err := registry.Work(t.Context(), jobs.EncodeJobArgs{Store: "tokyo", RecordingID: 1, Profile: "h264"})
	if !errors.Is(err, scopedErr) {
		t.Fatalf("encode media error = %v, want store-scoped error %v", err, scopedErr)
	}
}
