package streamer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/fetburner/rokuban/internal/db/dbtest"
	"github.com/fetburner/rokuban/internal/objstore"
	"github.com/fetburner/rokuban/internal/objstore/fsstore"
	"github.com/fetburner/rokuban/internal/store"
	"github.com/fetburner/rokuban/internal/store/pgstore/pgstoretest"
	"github.com/fetburner/rokuban/internal/testutil"
)

type rangeOnlyStore struct {
	objstore.Store
	getFrom func(context.Context, string, int64) (io.ReadCloser, error)
}

func (s *rangeOnlyStore) GetFrom(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	return s.getFrom(ctx, key, offset)
}

type countedRangeReader struct {
	store objstore.Store
	calls atomic.Int32
}

func (r *countedRangeReader) GetFrom(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	r.calls.Add(1)
	body, err := r.store.(objstore.Reader).Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(io.Discard, body, offset); err != nil {
		_ = body.Close()
		return nil, err
	}
	return body, nil
}

func newRangeDeliveryFixture(t *testing.T, payload []byte, getFrom func(context.Context, string, int64) (io.ReadCloser, error)) (*httptest.Server, int64) {
	t.Helper()
	pool := testutil.SetupDB(t)
	const key = "derived/proxy.mp4"
	base, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Put(t.Context(), key, bytes.NewReader(payload), int64(len(payload)), sha256Sum(payload), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	recordingID := dbtest.Recording(t, pool, func(p *dbtest.RecordingParams) { p.Status = store.RecordingStatusFinished })
	profile := "proxy"
	assetID := dbtest.MediaAsset(t, pool, recordingID, func(p *dbtest.MediaAssetParams) {
		p.Kind, p.Profile, p.RelPath, p.SizeBytes = store.AssetKindEncoded, &profile, key, int64(len(payload))
	})
	if _, err := pool.Exec(t.Context(), `UPDATE media_asset_copies SET rel_path = $2 WHERE media_asset_id = $1 AND store = $3`, assetID, key, dbtest.StoreName); err != nil {
		t.Fatal(err)
	}
	objects := &rangeOnlyStore{Store: base, getFrom: getFrom}
	router := chi.NewRouter()
	New(pgstoretest.Media(pool), Config{ObjectStores: map[string]objstore.Store{dbtest.StoreName: objects}}).Mount(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, recordingID
}

func sha256Sum(payload []byte) [32]byte { return sha256.Sum256(payload) }

func TestRangeReaderDeliveryHTTPContract(t *testing.T) {
	payload := bytes.Repeat([]byte("proxy-delivery-"), 8192)
	base, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Put(t.Context(), "derived/proxy.mp4", bytes.NewReader(payload), int64(len(payload)), sha256.Sum256(payload), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	reader := &countedRangeReader{store: base}
	server, id := newRangeDeliveryFixture(t, payload, reader.GetFrom)
	url := fmt.Sprintf("%s/api/media/recordings/%d/file?profile=proxy", server.URL, id)
	request := func(method string, headers map[string]string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		_ = resp.Body.Close()
		return resp, body
	}

	before := reader.calls.Load()
	response, body := request(http.MethodGet, nil)
	if response.StatusCode != 200 || !bytes.Equal(body, payload) {
		t.Fatalf("GET got status %d and %d bytes, want 200 and %d bytes", response.StatusCode, len(body), len(payload))
	}
	if got := reader.calls.Load() - before; got != 1 {
		t.Fatalf("one GET opened GetFrom %d times, want 1", got)
	}
	if response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", response.Header.Get("X-Accel-Buffering"))
	}

	response, body = request(http.MethodGet, map[string]string{"Range": "bytes=100-199"})
	if response.StatusCode != 206 || response.Header.Get("Content-Range") != fmt.Sprintf("bytes 100-199/%d", len(payload)) || !bytes.Equal(body, payload[100:200]) {
		t.Fatalf("206 response status=%d Content-Range=%q body=%d bytes", response.StatusCode, response.Header.Get("Content-Range"), len(body))
	}
	response, _ = request(http.MethodGet, map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(payload)+1)})
	if response.StatusCode != 416 {
		t.Fatalf("unsatisfiable Range status = %d, want 416", response.StatusCode)
	}

	before = reader.calls.Load()
	response, body = request(http.MethodHead, nil)
	if response.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("HEAD got status %d and body %d bytes, want 200 and empty", response.StatusCode, len(body))
	}
	if got := reader.calls.Load() - before; got != 0 {
		t.Fatalf("HEAD opened GetFrom %d times, want 0", got)
	}

	response, _ = request(http.MethodHead, nil)
	lastModified := response.Header.Get("Last-Modified")
	response, body = request(http.MethodGet, map[string]string{"Range": "bytes=100-199", "If-Range": lastModified})
	if response.StatusCode != 206 || !bytes.Equal(body, payload[100:200]) {
		t.Fatalf("matching If-Range status = %d body = %d bytes, want 206 and 100 bytes", response.StatusCode, len(body))
	}
	response, body = request(http.MethodGet, map[string]string{"Range": "bytes=100-199", "If-Range": "Wed, 21 Oct 2015 07:28:00 GMT"})
	if response.StatusCode != 200 || !bytes.Equal(body, payload) {
		t.Fatalf("stale If-Range status = %d body = %d bytes, want 200 and complete body", response.StatusCode, len(body))
	}
}

type failingBody struct{ sent bool }

func (b *failingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, bytes.Repeat([]byte("x"), 32768)), nil
	}
	return 0, errors.New("injected upstream failure")
}
func (*failingBody) Close() error { return nil }

func TestRangeReaderDeliveryReportsTruncatedResponse(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 65536)
	server, id := newRangeDeliveryFixture(t, payload, func(context.Context, string, int64) (io.ReadCloser, error) { return &failingBody{}, nil })
	url := fmt.Sprintf("%s/api/media/recordings/%d/file?profile=proxy", server.URL, id)
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 before upstream read", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err == nil {
		t.Fatalf("client read %d of %d bytes without detecting truncated response", len(body), len(payload))
	}
}

type cancellableBody struct {
	ctx     context.Context
	first   bool
	stopped chan<- struct{}
	release <-chan struct{}
}

func (b *cancellableBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		return copy(p, bytes.Repeat([]byte("x"), 32768)), nil
	}
	select {
	case <-b.ctx.Done():
		close(b.stopped)
		return 0, b.ctx.Err()
	case <-b.release:
		close(b.stopped)
		return 0, errors.New("test released blocked body")
	}
}
func (*cancellableBody) Close() error { return nil }

func TestRangeReaderDeliveryStopsAfterClientDisconnect(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 1<<20)
	stopped := make(chan struct{})
	release := make(chan struct{})
	server, id := newRangeDeliveryFixture(t, payload, func(ctx context.Context, _ string, _ int64) (io.ReadCloser, error) {
		return &cancellableBody{ctx: ctx, stopped: stopped, release: release}, nil
	})
	// server.Close の前に詰まった置き場読みを解放し、壊れた実装でもテストを有限時間で終える。
	t.Cleanup(func() { close(release) })
	url := fmt.Sprintf("%s/api/media/recordings/%d/file?profile=proxy", server.URL, id)
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	if _, err := response.Body.Read(buf); err != nil {
		t.Fatalf("first response read: %v", err)
	}
	_ = response.Body.Close()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream body continued reading after the client disconnected")
	}
}
