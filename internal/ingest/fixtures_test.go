package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/chapters"
	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/encode"
	"github.com/fetburner/rokuban/internal/jobq"
	jobqfaketest "github.com/fetburner/rokuban/internal/jobq/jobqfake/testutil"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// singleSiteClients は 1 site だけを束縛した MirakcClients を組み立てる。site が空なら store.DefaultSite を使う。
func singleSiteClients(site string, client *mirakc.Client) map[string]*mirakc.Client {
	if site == "" {
		site = store.DefaultSite
	}
	return map[string]*mirakc.Client{site: client}
}

type mirakcRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f mirakcRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// enqueuedArgs は kind の投入のうち、型 T に一致する Args を返す。
func enqueuedArgs[T jobq.Args](enqueuer *jobqfaketest.RecordingEnqueuer, kind string) []T {
	var found []T
	for _, call := range enqueuer.CallsOfKind(kind) {
		if typed, ok := call.Args.(T); ok {
			found = append(found, typed)
		}
	}
	return found
}

// countEnqueuedForRecording は recording_id が一致する kind の投入数を数える。
func countEnqueuedForRecording(t *testing.T, enqueuer *jobqfaketest.RecordingEnqueuer, kind string, recordingID int64) int {
	t.Helper()
	count := 0
	for _, call := range enqueuer.CallsOfKind(kind) {
		encoded, err := json.Marshal(call.Args)
		if err != nil {
			t.Fatalf("encoding %s args: %v", kind, err)
		}
		var keyed struct {
			RecordingID int64 `json:"recording_id"`
		}
		if err := json.Unmarshal(encoded, &keyed); err != nil {
			t.Fatalf("decoding %s args: %v", kind, err)
		}
		if keyed.RecordingID == recordingID {
			count++
		}
	}
	return count
}

// testStorageObjects は FS の置き場 name を dir に置いた StorageObjects を返す。
func testStorageObjects(t *testing.T, name, dir string) map[string]objstore.Store {
	t.Helper()
	return map[string]objstore.Store{name: testFilesystemObjectStore(t, dir)}
}

func testFilesystemObjectStore(t *testing.T, root string) objstore.Store {
	t.Helper()
	objects, err := fsstore.New(root)
	testutil.RequireNoError(t, err, "creating filesystem object store")
	return objects
}

// seedOriginalAsset は原本の active media_assets 行 + ファイルを 1 件用意する。
func seedOriginalAsset(t *testing.T, pool *pgxpool.Pool, mediaDir string, recordingID int64, relPath string, content []byte) {
	t.Helper()
	full := filepath.Join(mediaDir, filepath.FromSlash(relPath))
	testutil.RequireNoError(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir")
	testutil.RequireNoError(t, os.WriteFile(full, content, 0o644), "write")
	dbtest.MediaAsset(t, pool, recordingID, func(fixtureParams *dbtest.MediaAssetParams) {
		fixtureParams.RelPath = relPath
		fixtureParams.SizeBytes = int64(len(content))
	})
}

func prepareEncodeInput(t *testing.T, pool *pgxpool.Pool, recordingID int64, mediaDir, relPath string, profiles []string, content []byte) int64 {
	t.Helper()
	if len(profiles) > 0 {
		dbtest.ProcessingPolicy(t, pool, recordingID, func(p *dbtest.ProcessingPolicyParams) { p.EncodeProfiles = profiles })
	}
	seedOriginalAsset(t, pool, mediaDir, recordingID, relPath, content)
	return recordingID
}

// encodeConfigWith は名前だけが意味を持つ encode 設定を作る（このパスは
// プロファイル名しか見ない --- ffmpeg は起動しない）。
func encodeConfigWith(names ...string) config.EncodeConfig {
	profiles := make([]config.EncodeProfile, 0, len(names))
	for _, n := range names {
		profiles = append(profiles, config.EncodeProfile{
			Name: n, Container: "mp4", VideoCodec: "libx264", AudioCodec: "aac",
		})
	}
	return config.EncodeConfig{Profiles: profiles}
}

// runEncodeReconcilePassArgs は encode の reconcile を 1 回走らせ、投入を記録した enqueuer を返す。
func runEncodeReconcilePassArgs(t *testing.T, w *encode.EncodeReconcileWorker, args jobs.EncodeReconcileArgs) *jobqfaketest.RecordingEnqueuer {
	t.Helper()
	if w.Media == nil {
		t.Fatal("EncodeReconcileWorker.Media is nil")
	}
	enqueuer, ok := w.Enqueuer.(*jobqfaketest.RecordingEnqueuer)
	if !ok {
		enqueuer = jobqfaketest.NewRecordingEnqueuer()
		w.Enqueuer = enqueuer
	}
	testutil.RequireNoError(t, jobqfaketest.RunWorkerOnce(context.Background(), w, args), "EncodeReconcileWorker.Work")
	return enqueuer
}

// cutFFmpegProfile は cut プロファイルを 1 つ返す。Scaler と Height を 0 にして、
// filtergraph が trim と concat だけになるようにする。
func cutFFmpegProfile() config.EncodeProfile {
	return config.EncodeProfile{
		Name:       "cut",
		Container:  "mp4",
		VideoCodec: "libx264",
		AudioCodec: "aac",
		Cut:        true,
	}
}

// seedEncodedAsset は active な encoded media_assets 行を 1 つ作って id を返す。
// 世代は relPath の stem から読む。
func seedEncodedAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, profile, relPath string) int64 {
	t.Helper()
	ctx := context.Background()
	profileName := profile
	stem := strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
	var generation int64
	if _, g, ok := config.SplitCutGeneration(stem); ok {
		generation = int64(g)
	} else if _, g, ok := config.SplitUncutGeneration(stem); ok {
		generation = int64(g)
	}
	assetID, err := sqlcgen.New(pool).UpsertDerivedMediaAsset(ctx, sqlcgen.UpsertDerivedMediaAssetParams{Kind: "encoded",
		RecordingID:       recordingID,
		Profile:           &profileName,
		SizeBytes:         1,
		DerivedGeneration: generation,
	})
	testutil.RequireNoError(t, err, "seeding encoded media_asset")
	_, err = sqlcgen.New(pool).UpsertMediaAssetCopy(ctx, sqlcgen.UpsertMediaAssetCopyParams{
		MediaAssetID: assetID, Store: dbtest.StoreName, RelPath: relPath,
		Generation: generation, State: "active",
	})
	testutil.RequireNoError(t, err, "seeding encoded media_asset copy")
	return assetID
}

// setFrozenCuts は media_asset_cuts を直接書く（テストが前状態を作るために使う）。
func setFrozenCuts(t *testing.T, pool *pgxpool.Pool, assetID int64, keep []chapters.Range) error {
	t.Helper()
	ranges, err := pgstore.KeepRangesParam(keep)
	if err != nil {
		return err
	}
	q := sqlcgen.New(pool)
	if err := q.DeleteMediaAssetCuts(context.Background(), assetID); err != nil {
		return err
	}
	return q.InsertMediaAssetCuts(context.Background(), sqlcgen.InsertMediaAssetCutsParams{
		MediaAssetID: assetID,
		KeepRanges:   ranges,
	})
}

// runEncodeReconcilePass は引数なしの reconcile を 1 回走らせる。
func runEncodeReconcilePass(t *testing.T, w *encode.EncodeReconcileWorker) *jobqfaketest.RecordingEnqueuer {
	t.Helper()
	return runEncodeReconcilePassArgs(t, w, jobs.EncodeReconcileArgs{})
}

func assertOriginalAssetSHA256(t *testing.T, pool *pgxpool.Pool, recordingID int64, content []byte) {
	t.Helper()
	const query = `SELECT sha256 FROM media_assets WHERE recording_id = $1 AND kind = $2 AND EXISTS (SELECT 1 FROM media_asset_copies c WHERE c.media_asset_id = media_assets.id AND c.state = 'active') AND profile IS NULL`
	var got []byte
	testutil.RequireNoError(t, pool.QueryRow(context.Background(), query, recordingID, store.AssetKindOriginal).Scan(&got), "reading active original media asset SHA-256")
	want := sha256.Sum256(content)
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("active original SHA-256 = %x, want %x", got, want)
	}
}
