package streamer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/testutil"
)

const testSite = "default"

// fixture は録画 1 本と原本ファイルを用意した配信サーバーを返す。
type fixture struct {
	srv         *httptest.Server
	mediaDir    string
	recordingID int64
	content     []byte
}

// makeTSData は 188 バイト境界に揃ったダミー TS データを返す。
func makeTSData(packets int) []byte {
	data := make([]byte, packets*188)
	for i := range packets {
		off := i * 188
		data[off] = 0x47
		data[off+3] = 0x10 | byte(i%16)
		// 各パケットに通し番号を入れて Range の位置ずれを検出できるようにする
		data[off+4] = byte(i)
		data[off+5] = byte(i >> 8)
	}
	return data
}

func newFixture(t *testing.T, relPath string, writeFile bool) *fixture {
	t.Helper()
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	content := makeTSData(500)

	if writeFile {
		full := filepath.Join(mediaDir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating dir: %v", err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatalf("writing fixture file: %v", err)
		}
	}

	recordingID := seedRecording(t, pool)
	seedAsset(t, pool, recordingID, relPath, int64(len(content)))

	r := chi.NewRouter()
	New(pool, Config{MediaDir: mediaDir}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &fixture{srv: srv, mediaDir: mediaDir, recordingID: recordingID, content: content}
}

func (f *fixture) url() string {
	return fmt.Sprintf("%s/api/recordings/%d/file", f.srv.URL, f.recordingID)
}

func seedRecording(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateRecording(context.Background(), sqlcgen.CreateRecordingParams{
		Source:            "manual",
		Site:              testSite,
		NetworkID:         32678,
		ServiceID:         5168,
		EventID:           1,
		ServiceName:       "テストチャンネル",
		ChannelType:       "GR",
		Channel:           "27",
		Title:             "テスト番組",
		ProgramStartAt:    time.Now().Truncate(time.Second),
		ProgramDurationMs: 1800000,
		Status:            "finished",
	})
	if err != nil {
		t.Fatalf("seeding recording: %v", err)
	}
	return id
}

func seedAsset(t *testing.T, pool *pgxpool.Pool, recordingID int64, relPath string, size int64) int64 {
	t.Helper()
	id, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindOriginal,
		RelPath:     relPath,
		SizeBytes:   size,
	})
	if err != nil {
		t.Fatalf("seeding media_asset: %v", err)
	}
	return id
}

func get(t *testing.T, url string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp, body
}

func TestRecordingThumbnail_ServesJPEG(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9}

	recordingID := seedRecording(t, pool)
	rel := fmt.Sprintf("thumbnails/%d.jpg", recordingID)
	full := filepath.Join(mediaDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(pool).CreateMediaAsset(context.Background(), sqlcgen.CreateMediaAssetParams{
		RecordingID: recordingID,
		Kind:        db.AssetKindThumbnail,
		RelPath:     rel,
		SizeBytes:   int64(len(jpeg)),
	}); err != nil {
		t.Fatalf("seed thumbnail: %v", err)
	}

	r := chi.NewRouter()
	New(pool, Config{MediaDir: mediaDir}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, body := get(t, fmt.Sprintf("%s/api/recordings/%d/thumbnail", srv.URL, recordingID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, jpeg) {
		t.Errorf("body = %v, want jpeg", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != thumbnailContentType {
		t.Errorf("Content-Type = %q, want %q", ct, thumbnailContentType)
	}
}

func TestRecordingThumbnail_NotFoundWithoutAsset(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := seedRecording(t, pool)
	r := chi.NewRouter()
	New(pool, Config{MediaDir: t.TempDir()}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, _ := get(t, fmt.Sprintf("%s/api/recordings/%d/thumbnail", srv.URL, recordingID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRecordingFile_FullContent(t *testing.T) {
	f := newFixture(t, "2026/07/recording.m2ts", true)

	resp, body := get(t, f.url(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, f.content) {
		t.Errorf("body length = %d, want %d", len(body), len(f.content))
	}
	if ct := resp.Header.Get("Content-Type"); ct != contentType {
		t.Errorf("Content-Type = %q, want %q", ct, contentType)
	}
	// VLC がシークするために必須
	if ar := resp.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}
	if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprint(len(f.content)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(f.content))
	}
}

func TestRecordingFile_Range(t *testing.T) {
	f := newFixture(t, "recording.m2ts", true)

	tests := []struct {
		name      string
		rangeHdr  string
		wantStart int
		wantEnd   int // 含む
	}{
		{"先頭", "bytes=0-187", 0, 187},
		{"途中（188 の倍数）", "bytes=18800-19739", 18800, 19739},
		{"途中（境界外）", "bytes=1000-2000", 1000, 2000},
		{"末尾から", "bytes=-376", len(makeTSData(500)) - 376, len(makeTSData(500)) - 1},
		{"開始位置のみ", "bytes=93812-", 93812, len(makeTSData(500)) - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := get(t, f.url(), map[string]string{"Range": tt.rangeHdr})
			if resp.StatusCode != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206", resp.StatusCode)
			}
			want := f.content[tt.wantStart : tt.wantEnd+1]
			if !bytes.Equal(body, want) {
				t.Errorf("body = %d bytes, want %d (位置がずれている可能性)", len(body), len(want))
			}
			wantCR := fmt.Sprintf("bytes %d-%d/%d", tt.wantStart, tt.wantEnd, len(f.content))
			if cr := resp.Header.Get("Content-Range"); cr != wantCR {
				t.Errorf("Content-Range = %q, want %q", cr, wantCR)
			}
		})
	}
}

// 範囲外の Range は 416 になること。
func TestRecordingFile_UnsatisfiableRange(t *testing.T) {
	f := newFixture(t, "recording.m2ts", true)

	resp, _ := get(t, f.url(), map[string]string{
		"Range": fmt.Sprintf("bytes=%d-%d", len(f.content)+100, len(f.content)+200),
	})
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status = %d, want 416", resp.StatusCode)
	}
}

// 条件付きリクエストが効くこと（VLC やブラウザの再接続で使われる）。
func TestRecordingFile_ConditionalRequest(t *testing.T) {
	f := newFixture(t, "recording.m2ts", true)

	resp, _ := get(t, f.url(), nil)
	lastModified := resp.Header.Get("Last-Modified")
	if lastModified == "" {
		t.Fatal("Last-Modified is empty")
	}

	resp, body := get(t, f.url(), map[string]string{"If-Modified-Since": lastModified})
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d, want 304", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Errorf("304 should have an empty body, got %d bytes", len(body))
	}
}

func TestRecordingFile_NotFound(t *testing.T) {
	f := newFixture(t, "recording.m2ts", true)

	t.Run("存在しない録画 ID", func(t *testing.T) {
		resp, _ := get(t, fmt.Sprintf("%s/api/recordings/9999/file", f.srv.URL), nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("数値でない ID", func(t *testing.T) {
		resp, _ := get(t, fmt.Sprintf("%s/api/recordings/abc/file", f.srv.URL), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

// コミット（DB 行）はあるがファイルが無い不整合は 404 にすること。
func TestRecordingFile_MissingFile(t *testing.T) {
	f := newFixture(t, "recording.m2ts", false)

	resp, _ := get(t, f.url(), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// media_assets 行が無い録画（未 ingest）は配信しないこと。
func TestRecordingFile_NoAsset(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := seedRecording(t, pool)

	r := chi.NewRouter()
	New(pool, Config{MediaDir: t.TempDir()}).Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, _ := get(t, fmt.Sprintf("%s/api/recordings/%d/file", srv.URL, recordingID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ごみ箱に入った録画・削除済みアセットは配信しないこと。
func TestRecordingFile_DeletedIsNotServed(t *testing.T) {
	tests := []struct {
		name string
		mark func(t *testing.T, pool *pgxpool.Pool, recordingID int64)
	}{
		{
			name: "録画がごみ箱",
			mark: func(t *testing.T, pool *pgxpool.Pool, id int64) {
				if _, err := pool.Exec(context.Background(),
					"UPDATE recordings SET deleted_at = now() WHERE id = $1", id); err != nil {
					t.Fatalf("marking recording deleted: %v", err)
				}
			},
		},
		{
			name: "アセットが削除済み",
			mark: func(t *testing.T, pool *pgxpool.Pool, id int64) {
				if _, err := pool.Exec(context.Background(),
					"UPDATE media_assets SET state = 'deleted', deleted_at = now() WHERE recording_id = $1", id); err != nil {
					t.Fatalf("marking asset deleted: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			mediaDir := t.TempDir()
			content := makeTSData(10)
			if err := os.WriteFile(filepath.Join(mediaDir, "r.m2ts"), content, 0o644); err != nil {
				t.Fatalf("writing fixture: %v", err)
			}
			recordingID := seedRecording(t, pool)
			seedAsset(t, pool, recordingID, "r.m2ts", int64(len(content)))
			tt.mark(t, pool, recordingID)

			r := chi.NewRouter()
			New(pool, Config{MediaDir: mediaDir}).Mount(r)
			srv := httptest.NewServer(r)
			defer srv.Close()

			resp, _ := get(t, fmt.Sprintf("%s/api/recordings/%d/file", srv.URL, recordingID), nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}
		})
	}
}

// DB に細工された rel_path でメディアディレクトリの外を読み出せないこと。
// ingest 側でも検証しているが、配信側の独立した防御が効いていることを固定する。
func TestRecordingFile_RejectsPathTraversal(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()

	// media ディレクトリの外に「読まれてはいけない」ファイルを置く
	secret := filepath.Join(filepath.Dir(mediaDir), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatalf("writing secret: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	recordingID := seedRecording(t, pool)
	seedAsset(t, pool, recordingID, "../secret.txt", 10)

	r := chi.NewRouter()
	New(pool, Config{MediaDir: mediaDir}).Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, body := get(t, fmt.Sprintf("%s/api/recordings/%d/file", srv.URL, recordingID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if bytes.Contains(body, []byte("TOP SECRET")) {
		t.Error("メディアディレクトリ外のファイルが配信された")
	}
}

// HEAD が扱えること。VLC やブラウザはシーク前に HEAD で Content-Length と
// Accept-Ranges を取るため、405 だとシーク再生に失敗しうる。
func TestRecordingFile_Head(t *testing.T) {
	f := newFixture(t, "recording.m2ts", true)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, f.url(), nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprint(len(f.content)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(f.content))
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("HEAD should have an empty body, got %d bytes", len(body))
	}
}

// AccelLocation を設定するとバイト転送を返さず X-Accel-Redirect を返すこと
// （認可判定はアプリ、バイト転送は nginx。issue #1 の nginx コメント）。
func TestRecordingFile_AccelRedirect(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	content := makeTSData(10)
	// 番組名由来のファイル名は空白・括弧・日本語を含む
	relPath := "20260725/160000_３か月でマスターするギター（４）_53256.m2ts"
	full := filepath.Join(mediaDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	recordingID := seedRecording(t, pool)
	seedAsset(t, pool, recordingID, relPath, int64(len(content)))

	r := chi.NewRouter()
	New(pool, Config{MediaDir: mediaDir, AccelLocation: "/_media/"}).Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, body := get(t, fmt.Sprintf("%s/api/recordings/%d/file", srv.URL, recordingID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// バイトはアプリから返さない（nginx が internal location から配る）
	if len(body) != 0 {
		t.Errorf("body = %d bytes, want 0（バイト転送は nginx の担当）", len(body))
	}
	if ct := resp.Header.Get("Content-Type"); ct != contentType {
		t.Errorf("Content-Type = %q, want %q", ct, contentType)
	}
	got := resp.Header.Get("X-Accel-Redirect")
	if got == "" {
		t.Fatal("X-Accel-Redirect is empty")
	}
	// nginx は値を URI として解釈するのでパス要素はエスケープされている必要がある
	if strings.ContainsAny(got, " （）") {
		t.Errorf("X-Accel-Redirect = %q, want URL-escaped path", got)
	}
	if !strings.HasPrefix(got, "/_media/20260725/") {
		t.Errorf("X-Accel-Redirect = %q, want /_media/ prefix", got)
	}
}

// AccelLocation が有効でも、メディアディレクトリの外を指す rel_path は
// ヘッダーを返さないこと（検証前にヘッダーを返すと任意ファイルを配らせられる）。
func TestRecordingFile_AccelRedirectRejectsTraversal(t *testing.T) {
	pool := testutil.SetupDB(t)
	recordingID := seedRecording(t, pool)
	seedAsset(t, pool, recordingID, "../secret.txt", 10)

	r := chi.NewRouter()
	New(pool, Config{MediaDir: t.TempDir(), AccelLocation: "/_media/"}).Mount(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, _ := get(t, fmt.Sprintf("%s/api/recordings/%d/file", srv.URL, recordingID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Accel-Redirect"); got != "" {
		t.Errorf("X-Accel-Redirect = %q, want empty", got)
	}
}

// streamer のルートが OpenAPI 生成ルートと共存できること。
func TestMount_CoexistsWithGeneratedRoutes(t *testing.T) {
	pool := testutil.SetupDB(t)
	mediaDir := t.TempDir()
	content := makeTSData(10)
	if err := os.WriteFile(filepath.Join(mediaDir, "r.m2ts"), content, 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	recordingID := seedRecording(t, pool)
	seedAsset(t, pool, recordingID, "r.m2ts", int64(len(content)))

	router := api.NewRouter(api.RouterConfig{
		Pool:    pool,
		Mounter: New(pool, Config{MediaDir: mediaDir}),
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	for _, path := range []string{
		"/api/recordings",
		fmt.Sprintf("/api/recordings/%d/drop-stats", recordingID),
		fmt.Sprintf("/api/recordings/%d/file", recordingID),
	} {
		resp, _ := get(t, srv.URL+path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}
