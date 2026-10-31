package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/fsink"
	"github.com/fetburner/rokuban/internal/jobs"
	"github.com/fetburner/rokuban/internal/mediapath"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore"
	"github.com/fetburner/rokuban/internal/testutil"
)

// transferTestRemoteStreamer は、転送元の置き場の streamer を httptest で代わりに立てる。
// route は /api/media/stores/test-store/recordings/{id}/file。既定の応答は http.ServeContent で、
// Range に 206 と Content-Range で答える（実 streamer の配信と同じ）。
type transferTestRemoteStreamer struct {
	mu     sync.Mutex
	ranges []string
}

// seenRanges は route が受けた各要求の Range ヘッダを順に返す。Range が無ければ空文字。
func (s *transferTestRemoteStreamer) seenRanges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ranges)
}

func newTransferTestRemoteStreamer(t *testing.T, recordingID int64, content []byte, serve func(http.ResponseWriter, *http.Request, []byte)) (*httptest.Server, *transferTestRemoteStreamer) {
	t.Helper()
	streamer := &transferTestRemoteStreamer{}
	routePath := fmt.Sprintf("/api/media/stores/test-store/recordings/%d/file", recordingID)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != routePath {
			http.NotFound(w, r)
			return
		}
		streamer.mu.Lock()
		streamer.ranges = append(streamer.ranges, r.Header.Get("Range"))
		streamer.mu.Unlock()
		if serve != nil {
			serve(w, r, content)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	return srv, streamer
}

// transferTestRemoteFixture は、test-store を worker にマウントさせず、internal_url の streamer だけで
// 原本を持たせた転送の土台。archive だけがマウントされている。
type transferTestRemoteFixture struct {
	pool         *pgxpool.Pool
	roots        map[string]string
	cfg          config.StorageConfig
	fingerprint  string
	assetID      int64
	recordingID  int64
	content      []byte
	streamer     *transferTestRemoteStreamer
	relPath      string
	storageMedia *pgstore.Media
}

func newTransferTestRemoteFixture(t *testing.T, content []byte, knownSHA bool, serve func(http.ResponseWriter, *http.Request, []byte)) transferTestRemoteFixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	sourceRoot := transferTestRoots(t, "test-store")["test-store"]
	recordingID := dbtest.Recording(t, pool)
	assetID := seedOriginalAsset(t, pool, sourceRoot, recordingID, "sites/default/transfer/source.ts", content)
	if knownSHA {
		sum := sha256.Sum256(content)
		if _, err := pool.Exec(context.Background(), `UPDATE media_assets SET sha256 = $1 WHERE id = $2`, sum[:], assetID); err != nil {
			t.Fatal(err)
		}
	}
	srv, streamer := newTransferTestRemoteStreamer(t, recordingID, content, serve)
	cfg := transferTestConfig("archive", "archive", "test-store")
	source := cfg.Stores["test-store"]
	source.InternalURL = srv.URL
	cfg.Stores["test-store"] = source
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return transferTestRemoteFixture{
		pool: pool, roots: transferTestRoots(t, "archive"), cfg: cfg, fingerprint: fingerprint,
		assetID: assetID, recordingID: recordingID, content: content, streamer: streamer,
		relPath:      "sites/default/transfer/source.ts",
		storageMedia: pgstore.NewMedia(pool, "test-store", ""),
	}
}

func (f transferTestRemoteFixture) worker(t *testing.T) StorageTransferWorker {
	t.Helper()
	return StorageTransferWorker{
		Storage: f.storageMedia, Config: f.cfg, Roots: f.roots,
		ObjectStores: transferTestObjectStores(t, f.roots), ScratchDir: t.TempDir(),
	}
}

func (f transferTestRemoteFixture) args() jobs.StorageTransferArgs {
	return jobs.StorageTransferArgs{MediaAssetID: f.assetID, Store: "archive", PolicyFingerprint: f.fingerprint}
}

// copyState は archive の copy の状態を返す。行が無ければ空文字。
func (f transferTestRemoteFixture) copyState(t *testing.T) string {
	t.Helper()
	var state string
	err := f.pool.QueryRow(context.Background(),
		`SELECT state FROM media_asset_copies WHERE media_asset_id = $1 AND store = 'archive' AND state <> 'deleted'`, f.assetID).Scan(&state)
	if err != nil {
		return ""
	}
	return state
}

// failureText は archive の copy に記録された最後の転送失敗と試行回数を返す。
func (f transferTestRemoteFixture) failureText(t *testing.T) (string, int) {
	t.Helper()
	var lastError string
	var attempts int
	if err := f.pool.QueryRow(context.Background(), `
SELECT attempt.last_error, attempt.attempt_count FROM media_asset_transfer_attempts attempt
JOIN media_asset_copies copy ON copy.id = attempt.copy_id
WHERE copy.media_asset_id = $1 AND copy.store = 'archive'`, f.assetID).Scan(&lastError, &attempts); err != nil {
		t.Fatalf("reading recorded transfer failure: %v", err)
	}
	return lastError, attempts
}

func (f transferTestRemoteFixture) destinationExists() bool {
	return fileExists(filepath.Join(f.roots["archive"], filepath.FromSlash(f.relPath)))
}

func TestStorageTransferWorkerCopiesOriginalFromInternalURL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		knownSHA bool
	}{
		{name: "unknown SHA-256 is computed and written", knownSHA: false},
		{name: "known SHA-256 is verified", knownSHA: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := []byte("original bytes read from the source store's streamer over internal_url")
			f := newTransferTestRemoteFixture(t, content, tc.knownSHA, nil)
			worker := f.worker(t)
			if err := worker.Work(context.Background(), f.args()); err != nil {
				t.Fatalf("Work from internal_url: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(f.roots["archive"], filepath.FromSlash(f.relPath)))
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("destination = %q, %v; want %q", got, err, content)
			}
			if state := f.copyState(t); state != "active" {
				t.Fatalf("archive copy state = %q, want active", state)
			}
			var stored []byte
			if err := f.pool.QueryRow(context.Background(), `SELECT sha256 FROM media_assets WHERE id = $1`, f.assetID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			want := sha256.Sum256(content)
			if !bytes.Equal(stored, want[:]) {
				t.Fatalf("stored SHA-256 = %x, want %x", stored, want[:])
			}
			if ranges := f.streamer.seenRanges(); !slices.Equal(ranges, []string{""}) {
				t.Fatalf("streamer saw Range headers %q, want one request without Range", ranges)
			}
		})
	}
}

func TestStorageTransferWorkerResumesRemoteOriginalWithRange(t *testing.T) {
	content := []byte("resumed remote original: the checkpointed prefix is kept and only the suffix is requested")
	f := newTransferTestRemoteFixture(t, content, true, nil)
	transfer, ok, err := f.storageMedia.PrepareMediaAssetTransfer(context.Background(), f.assetID, "archive",
		mustOriginalRules(t, f.cfg), f.cfg.FSStoreNames(), transferSourceStores(f.cfg, []string{"archive"}), f.cfg.FSStoreNames(), f.cfg.Derived, storageTransferAttemptLimit)
	if err != nil || !ok {
		t.Fatalf("preparing checkpointed remote transfer = (%+v, %t, %v)", transfer, ok, err)
	}
	tempPath := filepath.Join(f.roots["archive"], filepath.Dir(filepath.FromSlash(transfer.RelPath)),
		fmt.Sprintf("%s%d-%d", mediapath.StorageTransferTempFilePrefix, f.assetID, transfer.CopyID))
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		t.Fatal(err)
	}
	prefixSize := len(content) / 3
	prefix := content[:prefixSize]
	if err := os.WriteFile(tempPath, prefix, 0o644); err != nil {
		t.Fatal(err)
	}
	hasher := sha256.New()
	if _, err := hasher.Write(prefix); err != nil {
		t.Fatal(err)
	}
	if err := fsink.WriteCheckpoint(tempPath, int64(prefixSize), hasher, nil); err != nil {
		t.Fatal(err)
	}

	worker := f.worker(t)
	if err := worker.Work(context.Background(), f.args()); err != nil {
		t.Fatalf("resuming remote transfer: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(f.roots["archive"], filepath.FromSlash(f.relPath)))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("resumed destination = %q, %v; want %q", got, err, content)
	}
	if want := fmt.Sprintf("bytes=%d-", prefixSize); !slices.Equal(f.streamer.seenRanges(), []string{want}) {
		t.Fatalf("streamer Range headers = %q, want [%q]", f.streamer.seenRanges(), want)
	}
	if state := f.copyState(t); state != "active" {
		t.Fatalf("archive copy state = %q, want active", state)
	}
}

// 応答が offset からの原本として使えないとき、転送は失敗として記録され、宛先は公開されない。
func TestStorageTransferWorkerRejectsUnusableRemoteRangeAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request, []byte)
		// want は記録される失敗に含まれる文言。応答を受け入れる変異では別の文言になり、このテストは落ちる。
		want string
	}{
		{
			name: "200 to a Range request",
			serve: func(w http.ResponseWriter, _ *http.Request, content []byte) {
				_, _ = w.Write(content)
			},
			want: "returned status 200, want 206 Partial Content",
		},
		{
			name: "206 whose Content-Range starts elsewhere",
			serve: func(w http.ResponseWriter, _ *http.Request, content []byte) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(content)-1, len(content)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(content)
			},
			want: "range starts at byte 0, want",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := []byte("range answers must start exactly where the checkpointed prefix ends")
			f := newTransferTestRemoteFixture(t, content, true, tc.serve)
			transfer, ok, err := f.storageMedia.PrepareMediaAssetTransfer(context.Background(), f.assetID, "archive",
				mustOriginalRules(t, f.cfg), f.cfg.FSStoreNames(), transferSourceStores(f.cfg, []string{"archive"}), f.cfg.FSStoreNames(), f.cfg.Derived, storageTransferAttemptLimit)
			if err != nil || !ok {
				t.Fatalf("preparing transfer = (%+v, %t, %v)", transfer, ok, err)
			}
			tempPath := filepath.Join(f.roots["archive"], filepath.Dir(filepath.FromSlash(transfer.RelPath)),
				fmt.Sprintf("%s%d-%d", mediapath.StorageTransferTempFilePrefix, f.assetID, transfer.CopyID))
			if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
				t.Fatal(err)
			}
			prefix := content[:len(content)/2]
			if err := os.WriteFile(tempPath, prefix, 0o644); err != nil {
				t.Fatal(err)
			}
			hasher := sha256.New()
			_, _ = hasher.Write(prefix)
			if err := fsink.WriteCheckpoint(tempPath, int64(len(prefix)), hasher, nil); err != nil {
				t.Fatal(err)
			}

			worker := f.worker(t)
			if err := worker.Work(context.Background(), f.args()); err != nil {
				t.Fatalf("Work with unusable range answer returned %v; want failure recorded, not returned", err)
			}
			lastError, attempts := f.failureText(t)
			if !strings.Contains(lastError, tc.want) {
				t.Fatalf("recorded failure = %q, want it to contain %q", lastError, tc.want)
			}
			if attempts != 1 {
				t.Fatalf("attempt count = %d, want 1", attempts)
			}
			if f.destinationExists() {
				t.Fatal("destination was published from an unusable range answer")
			}
			if state := f.copyState(t); state == "active" {
				t.Fatal("archive copy became active from an unusable range answer")
			}
		})
	}
}

// internal_url の streamer が X-Accel-Redirect で返すと、本体が無いので転送は失敗になる。
// 原因が分かるように、文言でその理由を示す。
func TestStorageTransferWorkerRejectsAccelRedirectAnswer(t *testing.T) {
	content := []byte("bytes behind an X-Accel-Redirect answer never reach the transfer")
	f := newTransferTestRemoteFixture(t, content, true, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("X-Accel-Redirect", "/internal/stores/test-store/sites/default/transfer/source.ts")
		w.WriteHeader(http.StatusOK)
	})
	worker := f.worker(t)
	if err := worker.Work(context.Background(), f.args()); err != nil {
		t.Fatalf("Work with X-Accel-Redirect answer returned %v; want failure recorded, not returned", err)
	}
	lastError, attempts := f.failureText(t)
	if !strings.Contains(lastError, "X-Accel-Redirect") {
		t.Fatalf("recorded failure = %q, want it to name X-Accel-Redirect", lastError)
	}
	if attempts != 1 {
		t.Fatalf("attempt count = %d, want 1", attempts)
	}
	if f.destinationExists() {
		t.Fatal("destination was published from an X-Accel-Redirect answer")
	}
}

// 論理資産の SHA-256 と違うバイトが internal_url から来たら、既存の不一致の扱い（原本の不一致として記録し、
// 転送元を残す）になる。
func TestStorageTransferWorkerRemoteBytesDifferingFromLogicalSHAAreMismatch(t *testing.T) {
	content := []byte("logical original recorded with this SHA-256")
	served := bytes.Clone(content)
	served[0] ^= 0xff
	f := newTransferTestRemoteFixture(t, content, true, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(served))
	})
	worker := f.worker(t)
	if err := worker.Work(context.Background(), f.args()); err != nil {
		t.Fatalf("Work with differing remote bytes: %v", err)
	}
	lastError, attempts := f.failureText(t)
	if !strings.Contains(lastError, "does not match the logical media asset") {
		t.Fatalf("recorded failure = %q, want the original SHA-256 mismatch", lastError)
	}
	if attempts != 1 {
		t.Fatalf("attempt count = %d, want 1", attempts)
	}
	if f.destinationExists() {
		t.Fatal("mismatched remote bytes were published")
	}
	if !hasCurrentCopyInStore(t, f.pool, f.assetID, "test-store") {
		t.Fatal("current original copy in the source store is missing after mismatch")
	}
}

// internal_url の無い、マウントしていない置き場は転送元にならない。試行を数えないことは
// TestStorageTransferWorkerChecksMountsBeforePrepare の "missing source does not reserve or count an attempt" が見る。
// ここでは、同じ録画で internal_url の有無だけを変えて、予約されるかを対で確かめる。
func TestPrepareMediaAssetTransferReservesUnmountedSourceOnlyWithInternalURL(t *testing.T) {
	content := []byte("reservation depends on internal_url, not on the mount")
	f := newTransferTestRemoteFixture(t, content, true, nil)
	reserve := func(cfg config.StorageConfig) (bool, error) {
		_, ok, err := f.storageMedia.PrepareMediaAssetTransfer(context.Background(), f.assetID, "archive",
			mustOriginalRules(t, cfg), []string{"archive"}, transferSourceStores(cfg, []string{"archive"}), []string{"archive"}, cfg.Derived, storageTransferAttemptLimit)
		return ok, err
	}

	cfgNoURL := f.cfg
	cfgNoURL.Stores = map[string]config.StoreConfig{"archive": f.cfg.Stores["archive"], "test-store": {Kind: "fs"}}
	if ok, err := reserve(cfgNoURL); err != nil || ok {
		t.Fatalf("reserving without internal_url = (%t, %v), want no reservation", ok, err)
	}
	if ok, err := reserve(f.cfg); err != nil || !ok {
		t.Fatalf("reserving with internal_url = (%t, %v), want reserved", ok, err)
	}
}

// 派生物は HTTP で読む経路が無いので、転送元はマウントした置き場に限る。internal_url の置き場にしか無い
// 派生物は予約されず、マウントされると予約される。原本の経路（上の内容）とは別の SQL の枝を見る。
func TestPrepareMediaAssetTransferKeepsDerivedSourcesMounted(t *testing.T) {
	pool := testutil.SetupDB(t)
	sourceRoot := transferTestRoots(t, "test-store")["test-store"]
	recordingID := dbtest.Recording(t, pool)
	content := []byte("derived bytes exist only on the unmounted source store")
	assetID := seedEncodedOrThumbnailAsset(t, pool, sourceRoot, recordingID, store.AssetKindThumbnail, nil, "sites/default/transfer/thumbnail.jpg", content)
	cfg := transferTestConfig("archive", "archive", "test-store")
	source := cfg.Stores["test-store"]
	source.InternalURL = "http://127.0.0.1:1"
	cfg.Stores["test-store"] = source
	media := pgstore.NewMedia(pool, "test-store", "")
	reserve := func(fsStores, sourceStores []string) bool {
		t.Helper()
		_, ok, err := media.PrepareMediaAssetTransfer(context.Background(), assetID, "archive",
			mustOriginalRules(t, cfg), fsStores, sourceStores, fsStores, cfg.Derived, storageTransferAttemptLimit)
		if err != nil {
			t.Fatalf("preparing derived transfer: %v", err)
		}
		return ok
	}
	if reserve([]string{"archive"}, []string{"archive", "test-store"}) {
		t.Fatal("derived copy was reserved from an unmounted store")
	}
	if !reserve([]string{"archive", "test-store"}, []string{"archive", "test-store"}) {
		t.Fatal("derived copy was not reserved when its source store is mounted")
	}
}

// SHA-256 が未知の原本で、宛先が既に在り、転送元がリモートなら、採用せず失敗を記録する。文言は運用者の対処を示す。
func TestStorageTransferWorkerDoesNotAdoptUnverifiableRemoteDestination(t *testing.T) {
	content := []byte("unverified destination with an old row that has no SHA-256")
	f := newTransferTestRemoteFixture(t, content, false, nil)
	destination := filepath.Join(f.roots["archive"], filepath.FromSlash(f.relPath))
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	other := bytes.Repeat([]byte("x"), len(content))
	if err := os.WriteFile(destination, other, 0o644); err != nil {
		t.Fatal(err)
	}
	worker := f.worker(t)
	if err := worker.Work(context.Background(), f.args()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	lastError, attempts := f.failureText(t)
	for _, want := range []string{"no SHA-256", "mount a store", "remove"} {
		if !strings.Contains(lastError, want) {
			t.Fatalf("recorded failure = %q, want it to contain %q", lastError, want)
		}
	}
	if attempts != 1 {
		t.Fatalf("attempt count = %d, want 1", attempts)
	}
	if got, err := os.ReadFile(destination); err != nil || !bytes.Equal(got, other) {
		t.Fatalf("existing destination was changed: %q, %v", got, err)
	}
}

// 新しい copy が internal_url の置き場にしか無く、古い copy がマウントした置き場にあるとき、
// 転送はマウントした copy から読み、internal_url へ要求を送らない。
func TestStorageTransferWorkerPrefersMountedCopyOverNewerRemoteCopy(t *testing.T) {
	pool := testutil.SetupDB(t)
	roots := transferTestRoots(t, "local-src", "archive")
	content := []byte("older mounted copy is read locally while a newer remote-only copy exists")
	recordingID := dbtest.Recording(t, pool)
	// seedOriginalAsset の copy 行は dbtest.StoreName（test-store）に載る。それが新しい方（remote のみ）になる。
	// マウントした local-src の古い copy 行を、1 時間前の created_at で足す。
	assetID := seedOriginalAsset(t, pool, roots["local-src"], recordingID, "sites/default/transfer/source.ts", content)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO media_asset_copies (media_asset_id, store, rel_path, generation, state, created_at)
VALUES ($1, 'local-src', 'sites/default/transfer/source.ts', 0, 'active', now() - interval '1 hour')`, assetID); err != nil {
		t.Fatalf("adding older mounted copy: %v", err)
	}
	srv, streamer := newTransferTestRemoteStreamer(t, recordingID, content, nil)

	cfg := transferTestConfig("archive", "archive", "local-src", "test-store")
	source := cfg.Stores["test-store"]
	source.InternalURL = srv.URL
	cfg.Stores["test-store"] = source
	fingerprint, err := cfg.PolicyFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	worker := StorageTransferWorker{
		Storage: pgstore.NewMedia(pool, "archive", ""), Config: cfg, Roots: roots,
		ObjectStores: transferTestObjectStores(t, roots), ScratchDir: t.TempDir(),
	}
	if err := worker.Work(context.Background(), jobs.StorageTransferArgs{MediaAssetID: assetID, Store: "archive", PolicyFingerprint: fingerprint}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(roots["archive"], "sites/default/transfer/source.ts"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("destination = %q, %v; want %q", got, err, content)
	}
	if n := len(streamer.seenRanges()); n != 0 {
		t.Fatalf("internal_url received %d requests, want 0: the newer remote-only copy was chosen over the mounted copy", n)
	}
}
