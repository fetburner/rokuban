package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/encode"
	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/testexec"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

const derivedTestOriginal = "sites/default/derived-publish/original.ts"

func derivedPublishConfig(t *testing.T, root string) (*config.Config, string) {
	t.Helper()
	testutil.RequireNoError(t, mediapath.WriteStoreMarker(root, dbtest.StoreName), "writing source store marker")
	original := filepath.Join(root, derivedTestOriginal)
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(original), 0o755), "creating original directory")
	testutil.RequireNoError(t, os.WriteFile(original, []byte("original"), 0o600), "writing original")

	binDir := t.TempDir()
	ffmpeg, ffprobe := filepath.Join(binDir, "ffmpeg"), filepath.Join(binDir, "ffprobe")
	calls := filepath.Join(binDir, "encode-runs")
	t.Setenv("ROKUBAN_TEST_ENCODE_RUNS", calls)
	testexec.WriteExecutable(t, ffmpeg, []byte(`#!/bin/sh
set -eu
if [ "$#" -eq 2 ] && [ "$1" = "-hide_banner" ] && [ "$2" = "-decoders" ]; then
    printf ' S..... libaribcaption        ARIB caption decoder\n'
    exit 0
fi
for output do
    case "$output" in
        */out.mp4)
            printf 'run\n' >> "$ROKUBAN_TEST_ENCODE_RUNS"
            printf 'encoded' > "$output"
            ;;
        */out.vtt) printf 'WEBVTT\n' > "$output" ;;
    esac
done
case "$output" in
    *.jpg) printf 'jpeg' > "$output" ;;
esac
`))
	testexec.WriteExecutable(t, ffprobe, []byte("#!/bin/sh\nprintf '1\n'\n"))
	return &config.Config{
		Mirakcs: []config.MirakcSite{{Site: "default"}},
		Storage: config.StorageConfig{
			Stores: map[string]config.StoreConfig{
				dbtest.StoreName: {Kind: "fs"},
				"cloud":          {Kind: "s3", Bucket: "rokuban-test", Region: "us-east-1"},
			},
			Originals:  []config.OriginalStoreRule{{Store: dbtest.StoreName}},
			Derived:    "cloud",
			ScratchDir: t.TempDir(),
		},
		Encode: config.EncodeConfig{FFmpeg: ffmpeg, FFprobe: ffprobe, Profiles: []config.EncodeProfile{
			{Name: "web", Container: "mp4", VideoCodec: "libx264", Subtitles: "webvtt"},
		}},
	}, calls
}

// TestGeneratedMediaPublishesToFilesystem は、実際のジョブルートから FS への Put を検査する。
// 遠隔への Put は、世代予約や ffmpeg が終わり Put に達した箇所で失敗する。
func TestGeneratedMediaPublishesToFilesystem(t *testing.T) {
	for _, derivedKind := range []string{"s3", "fs"} {
		t.Run(derivedKind, func(t *testing.T) {
			root := t.TempDir()
			cfg, _ := derivedPublishConfig(t, root)
			testutil.RequireNoError(t, encode.ValidateDecoders(t.Context(), cfg.Encode), "checking fixture ffmpeg decoders")
			local, err := fsstore.New(root)
			testutil.RequireNoError(t, err, "opening source FS")
			other, err := fsstore.New(t.TempDir())
			testutil.RequireNoError(t, err, "opening second test store")
			objects := map[string]objstore.Store{dbtest.StoreName: local}
			var wantObjects objstore.Store = local
			wantStore := dbtest.StoreName
			if derivedKind == "s3" {
				objects["cloud"] = &rejectDerivedPut{Store: other, t: t}
			} else {
				cfg.Storage.Derived = "archive"
				cfg.Storage.Stores["archive"] = config.StoreConfig{Kind: "fs"}
				objects["archive"] = other
				wantStore, wantObjects = "archive", other
			}
			policy, err := cfg.Storage.BuildStorePolicy()
			testutil.RequireNoError(t, err, "building storage policy")
			scoped, err := newStoreScopedDeps(nil, cfg, policy, dbtest.StoreName, root, objects)
			testutil.RequireNoError(t, err, "building store-scoped dependencies")
			publisher := &derivedPublishStub{}
			scoped.MediaReconcile = publisher
			scoped.DerivedMediaPublish = publisher
			scoped.Encode.Attempts = publisher
			scoped.Encode.Progress = nil
			registry := worker.NewWorkers(&worker.Deps{
				Encode: cfg.Encode, ScratchDir: cfg.Storage.ScratchDir,
				StorageObjects: objects, DerivedObjectStoreName: cfg.Storage.Derived,
				PerStore: map[string]*worker.StoreDeps{dbtest.StoreName: scoped},
			})
			for _, args := range []jobq.Args{
				jobs.EncodeJobArgs{RecordingID: 42, Profile: "web", Store: dbtest.StoreName},
				jobs.ThumbnailJobArgs{RecordingID: 42, Store: dbtest.StoreName},
				jobs.SeekTilesJobArgs{RecordingID: 42, Store: dbtest.StoreName},
			} {
				t.Run(args.Kind(), func(t *testing.T) {
					publisher.published = ""
					testutil.RequireNoError(t, registry.Work(t.Context(), args), "running generated media job")
					if publisher.published == "" {
						t.Fatal("generated output was not committed")
					}
					_, err := wantObjects.Head(t.Context(), publisher.published)
					testutil.RequireNoError(t, err, "reading committed output in the publishing store")
					if args.Kind() == "encode" {
						key, err := mediapath.SubtitleSibling(publisher.published)
						testutil.RequireNoError(t, err, "deriving subtitle key")
						_, err = wantObjects.Head(t.Context(), key)
						testutil.RequireNoError(t, err, "reading encoded subtitle in the publishing store")
					}
				})
			}
			if scoped.DerivedPublishStoreName != wantStore {
				t.Errorf("published store name = %q, want %q", scoped.DerivedPublishStoreName, wantStore)
			}
		})
	}
}

type rejectDerivedPut struct {
	objstore.Store
	t *testing.T
}

func (s *rejectDerivedPut) Put(_ context.Context, key string, _ io.Reader, _ int64, _ [32]byte, _ string) error {
	s.t.Errorf("generation called remote Put for %q", key)
	return errors.New("generation called remote Put")
}

type derivedPublishStub struct {
	store.MediaReconcile
	store.DerivedMediaPublisher
	store.EncodeAttempts
	published string
}

func (*derivedPublishStub) GetEncodedPublishState(context.Context, int64, string, bool, bool) (store.EncodedPublishState, error) {
	return store.EncodedPublishState{Desired: true}, nil
}

func (*derivedPublishStub) GetActiveOriginalMediaAsset(context.Context, int64) (store.MediaAsset, error) {
	return store.MediaAsset{ID: 1, RelPath: derivedTestOriginal, Store: dbtest.StoreName}, nil
}

func (*derivedPublishStub) HasActiveThumbnailMediaAsset(context.Context, int64) (bool, error) {
	return false, nil
}

func (*derivedPublishStub) HasActiveSeekTilesMediaAsset(context.Context, int64) (bool, error) {
	return false, nil
}

func (*derivedPublishStub) GetDerivedMediaAssetGeneration(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}

func (*derivedPublishStub) Begin(context.Context, int64, string, int32) (store.EncodeAttemptStart, error) {
	return store.EncodeAttemptStart{Count: 1}, nil
}

func (*derivedPublishStub) Clear(context.Context, int64, string, int32) error { return nil }

func (*derivedPublishStub) Fail(context.Context, int64, string, int32, string) (bool, error) {
	return true, nil
}

func (*derivedPublishStub) PreparePendingMediaAssetCopy(context.Context, store.PendingMediaAssetCopy) error {
	return nil
}

func (s *derivedPublishStub) PublishEncoded(_ context.Context, in store.EncodedPublishInput, publish func() error) (bool, error) {
	s.published = in.RelPath
	return true, publish()
}

func (s *derivedPublishStub) PublishThumbnail(_ context.Context, in store.ThumbnailPublishInput, publish func() error) (bool, error) {
	s.published = in.RelPath
	return true, publish()
}

func (s *derivedPublishStub) PublishSeekTiles(_ context.Context, in store.SeekTilesPublishInput, publish func() error) (bool, error) {
	s.published = in.RelPath
	return true, publish()
}

// TestFailedDerivedTransferDoesNotReencode は FS 公開の前後と遠隔送信の失敗後に、同じ encode reconcile を回す。
func TestFailedDerivedTransferDoesNotReencode(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := t.Context()
	root := t.TempDir()
	cfg, calls := derivedPublishConfig(t, root)
	policy, err := cfg.Storage.BuildStorePolicy()
	testutil.RequireNoError(t, err, "building storage policy")
	deps, _, err := newWorkerDeps(ctx, cfg, policy, nil, []string{dbtest.StoreName},
		map[string]string{dbtest.StoreName: root}, riveradapter.Config{Queues: []string{"encode"}}, pool, nil, false)
	testutil.RequireNoError(t, err, "building worker dependencies")
	remoteRoot := t.TempDir()
	remote, err := fsstore.New(remoteRoot)
	testutil.RequireNoError(t, err, "opening remote stand-in")
	deps.StorageObjects["cloud"] = unavailableDerivedUpload{Store: remote}
	enqueuer := &enqueueRecorder{}
	deps.Enqueuer = enqueuer
	registry := worker.NewWorkers(deps)
	recordingID := dbtest.Recording(t, pool)
	dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.RelPath, p.SizeBytes = derivedTestOriginal, int64(len("original"))
	})
	dbtest.ProcessingPolicy(t, pool, recordingID, func(p *dbtest.ProcessingPolicyParams) {
		p.EncodeProfiles = []string{"web"}
	})

	reconcile := jobs.EncodeReconcileArgs{RecordingID: recordingID}
	testutil.RequireNoError(t, registry.Work(ctx, reconcile), "reconciling before encode")
	if len(enqueuer.args) != 1 {
		t.Fatalf("jobs before publication = %v, want one encode", enqueuer.args)
	}
	encodeJob, ok := enqueuer.args[0].(jobs.EncodeJobArgs)
	if !ok || encodeJob.Store != dbtest.StoreName {
		t.Fatalf("job before publication = %#v, want encode for %s", enqueuer.args[0], dbtest.StoreName)
	}
	enqueuer.args = nil
	testutil.RequireNoError(t, registry.Work(ctx, encodeJob), "publishing encoded to FS")
	assertEncodeState := func() {
		t.Helper()
		runs, err := os.ReadFile(calls)
		testutil.RequireNoError(t, err, "reading ffmpeg invocation log")
		if string(runs) != "run\n" {
			t.Fatalf("ffmpeg invocations = %q, want exactly one", runs)
		}
		var attempts int
		testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM recording_encode_attempts WHERE recording_id = $1`, recordingID).Scan(&attempts), "reading encode attempts")
		if attempts != 0 {
			t.Fatalf("encode attempt rows = %d, want 0 after successful FS publication", attempts)
		}
	}
	assertEncodeState()
	testutil.RequireNoError(t, registry.Work(ctx, reconcile), "reconciling after FS publication")
	if len(enqueuer.args) != 0 {
		t.Fatalf("jobs after FS publication = %v, want none", enqueuer.args)
	}

	backlog, err := deps.UnscopedStorageTransfer.CountMediaAssetTransferBacklogBySourceStore(ctx, []string{dbtest.StoreName}, store.MediaAssetTransferAttemptLimit)
	testutil.RequireNoError(t, err, "counting transfer backlog")
	if backlog[dbtest.StoreName] != 1 {
		t.Fatalf("transfer backlog = %v, want one staged encoded asset", backlog)
	}
	testutil.RequireNoError(t, registry.Work(ctx, jobs.StorageTransferReconcileArgs{}), "reconciling transfer")
	if len(enqueuer.args) != 1 {
		t.Fatalf("jobs after transfer reconcile = %v, want one derived transfer", enqueuer.args)
	}
	transfer, ok := enqueuer.args[0].(jobs.StorageTransferArgs)
	if !ok || transfer.Store != "cloud" || transfer.SourceStore != dbtest.StoreName {
		t.Fatalf("transfer job = %#v, want FS to cloud", enqueuer.args[0])
	}
	enqueuer.args = nil
	testutil.RequireNoError(t, registry.Work(ctx, transfer), "recording remote upload failure")
	var attempts int
	var lastError, reservedKey string
	testutil.RequireNoError(t, pool.QueryRow(ctx, `
SELECT a.attempt_count, a.last_error, c.rel_path
FROM media_asset_transfer_attempts a
JOIN media_asset_copies c ON c.id = a.copy_id
WHERE c.media_asset_id = $1 AND c.store = 'cloud' AND c.state = 'pending'
`, transfer.MediaAssetID).Scan(&attempts, &lastError, &reservedKey), "reading pending cloud transfer failure")
	if attempts != 1 || lastError != "starting derived upload: upload unavailable" {
		t.Fatalf("transfer attempt = (%d, %q), want one failed remote upload", attempts, lastError)
	}
	testutil.RequireNoError(t, registry.Work(ctx, reconcile), "reconciling after transfer failure")
	assertEncodeState()
	if len(enqueuer.args) != 0 {
		t.Fatalf("jobs after transfer failure = %v, want none", enqueuer.args)
	}
	testutil.RequireNoError(t, registry.Work(ctx, encodeJob), "handling duplicate encode delivery after transfer failure")
	assertEncodeState()

	// Transfer failure does not remove FS output. A subsequent transfer can use those bytes.
	deps.StorageObjects["cloud"] = remote
	testutil.RequireNoError(t, registry.Work(ctx, transfer), "retrying derived transfer")
	var state, key string
	testutil.RequireNoError(t, pool.QueryRow(ctx, `SELECT state, rel_path FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'cloud'`, transfer.MediaAssetID).Scan(&state, &key), "reading cloud copy state")
	if state != "active" || key != reservedKey {
		t.Fatalf("cloud copy = (%q, %q), want (%q, active)", key, state, reservedKey)
	}
	for objectKey, want := range map[string]string{key: "encoded", strings.TrimSuffix(key, ".mp4") + ".vtt": "WEBVTT\n"} {
		got, err := os.ReadFile(filepath.Join(remoteRoot, filepath.FromSlash(objectKey)))
		testutil.RequireNoError(t, err, "reading transferred object")
		if string(got) != want {
			t.Errorf("transferred object %q = %q, want %q", objectKey, got, want)
		}
	}
}

type unavailableDerivedUpload struct{ *fsstore.Store }

func (unavailableDerivedUpload) StartUpload(context.Context, string, int64, [32]byte, string) ([]byte, error) {
	return nil, errors.New("upload unavailable")
}
