package streamer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
)

const testLiveSite = "default"

// fakeMirakcLiveState は偽 mirakc サーバーの観測結果。
type fakeMirakcLiveState struct {
	mu           sync.Mutex
	requests     int
	priorities   []string
	disconnected chan int64
}

func (s *fakeMirakcLiveState) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// newFakeMirakcLiveServer は GET /api/services/{id}/stream を実装する偽 mirakc。
// クライアント（streamer）が接続を切ると r.Context().Done() で検出し、
// disconnected チャネルに serviceId を送る --- 「止めたら mirakc 側の接続が
// 残らない」ことを検証するための観測点。
func newFakeMirakcLiveServer(t *testing.T) (*httptest.Server, *fakeMirakcLiveState) {
	t.Helper()
	state := &fakeMirakcLiveState{disconnected: make(chan int64, 16)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sid int64
		_, _ = fmt.Sscanf(r.URL.Path, "/api/services/%d/stream", &sid)

		state.mu.Lock()
		state.requests++
		state.priorities = append(state.priorities, r.Header.Get("X-Mirakurun-Priority"))
		state.mu.Unlock()

		w.Header().Set("Content-Type", "video/MP2T")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		buf := bytes188Packet()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				state.disconnected <- sid
				return
			case <-ticker.C:
				if _, err := w.Write(buf); err != nil {
					state.disconnected <- sid
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, state
}

func bytes188Packet() []byte {
	b := make([]byte, 188)
	b[0] = 0x47
	return b
}

// installFakeLiveFFmpeg は実際の ffmpeg に依存しない偽 ffmpeg を置く（CI に ffmpeg
// が無い可能性があるため。encode_test.go の installFakeFFmpeg と同じ方針）。
//
// 引数から `-hls_segment_filename <pattern>` / `-hls_base_url <prefix>` と直後の
// `*.m3u8` の組をプロファイルごとに読み取り、最小限の有効な HLS プレイリスト +
// セグメント 1 本を書き出してから、ctx キャンセル（exec.CommandContext の既定動作で
// SIGKILL）まで走り続ける。
//
// **`-hls_base_url` を実際に反映する。** BuildLiveFFmpegArgs が渡す値をそのまま
// プレイリストの行に付けることで、本物の ffmpeg（8.1.2 で確認済み）と同じ
// 「セグメント URI はプレイリスト URL 基準の相対パスであり、`-hls_base_url` の値が
// 前に付く」という振る舞いを模す。ここを basename だけにすると、実装側の
// `-hls_base_url` 欠落バグを偽 ffmpeg が覆い隠してしまう（レビューで発見）。
//
// **書き込みはすべて一時ファイル + rename でアトミックにする。** `-hls_flags
// temp_file` を渡した本物の ffmpeg は一時ファイルに書いてからリネームするため、
// 配信側が書き込み途中の内容を読むことはない。偽 ffmpeg が `>` で直接上書きすると
// この保証が失われ、`waitForPlaylist` が「存在する」だけを見て途中の内容を配って
// しまう（実際に flaky の原因になった。#EXTINF より前で読まれると
// `firstSegmentName` が空を返す）。
func installFakeLiveFFmpeg(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffmpeg-live")
	script := `#!/bin/sh
# 標準入力（mirakc からのライブ TS）を消費してパイプを埋めない。
cat >/dev/null &

baseurl=""
segfile=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-hls_segment_filename" ]; then
    segfile="$a"
  fi
  if [ "$prev" = "-hls_base_url" ]; then
    baseurl="$a"
  fi
  case "$a" in
    *.m3u8)
      playlist="$a"
      mkdir -p "$(dirname "$playlist")" 2>/dev/null
      seg=$(printf '%s' "$segfile" | sed 's/%05d/00001/')
      mkdir -p "$(dirname "$seg")" 2>/dev/null
      segname="${baseurl}$(basename "$seg")"

      # セグメントを一時ファイル→rename で先に確定させる（プレイリストが
      # 指す前に実体が存在すること）。
      printf 'fake-ts-segment-data' > "$seg.tmp"
      mv "$seg.tmp" "$seg"

      {
        printf '#EXTM3U\n'
        printf '#EXT-X-VERSION:3\n'
        printf '#EXT-X-TARGETDURATION:2\n'
        printf '#EXT-X-MEDIA-SEQUENCE:0\n'
        printf '#EXTINF:2.0,\n'
        printf '%s\n' "$segname"
      } > "$playlist.tmp"
      mv "$playlist.tmp" "$playlist"
      ;;
  esac
  prev="$a"
done

while true; do
  sleep 1
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestLiveStreamer(t *testing.T, mirakcURL string, cfg LiveConfig) (*LiveStreamer, *httptest.Server) {
	t.Helper()
	client := mirakc.NewClient(mirakcURL, nil)
	ls := NewLive(client, testLiveSite, cfg)
	r := chi.NewRouter()
	ls.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	// セッションを張ったまま test 関数を抜けると、mirakc への持続接続が残って
	// 偽 mirakc サーバーの httptest.Close() がハングする（実際に踏んだ）。
	// shutdown は複数回呼んでも安全（2 回目は空の sessions を見るだけ）。
	t.Cleanup(ls.shutdown)
	return ls, srv
}

func baseLiveConfig(t *testing.T) LiveConfig {
	return LiveConfig{
		Enabled:       true,
		FFmpeg:        installFakeLiveFFmpeg(t),
		SegmentDir:    t.TempDir(),
		MaxSessions:   2,
		IdleTimeout:   10 * time.Second,
		TunerPriority: 3,
		Profiles: []LiveProfile{
			{Name: "h264", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
		},
	}
}

func playlistURL(base string, serviceID int64, profile string) string {
	u := fmt.Sprintf("%s/api/sites/%s/services/%d/live/playlist.m3u8", base, testLiveSite, serviceID)
	if profile != "" {
		u += "?profile=" + profile
	}
	return u
}

// firstSegmentName はテスト用プレイリスト本文から最初の非コメント行（セグメント
// URI）を取り出す。
func firstSegmentName(t *testing.T, playlist string) string {
	t.Helper()
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	t.Fatalf("no segment line in playlist: %q", playlist)
	return ""
}

// resolveRelative はプレイリスト URL を基準に相対 URI を解決する。実 HLS クライアント
// （hls.js 等）がプレイリスト本文の行をどう解釈するかを模す ---
// テストが自前で `/segments/{name}` パスを組み立てるのではなく、プレイリストの
// URL そのものを基準にする（レビューで指摘された「オラクルの空虚さ」を潰す）。
func resolveRelative(t *testing.T, baseURL, ref string) string {
	t.Helper()
	base, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parsing base URL %q: %v", baseURL, err)
	}
	rel, err := url.Parse(ref)
	if err != nil {
		t.Fatalf("parsing reference %q: %v", ref, err)
	}
	return base.ResolveReference(rel).String()
}

// firstSegmentURL は plURL からプレイリストを取得し、最初のセグメント URI を
// **プレイリスト自身の URL を基準に相対解決した絶対 URL** として返す。
//
// 以前のテストは `/live/segments/{name}` という文字列を自前で組み立てて GET して
// いたため、実装側の `-hls_base_url` 欠落（プレイリストが basename しか書かない
// バグ）を検出できなかった（レビュー指摘の必須 1）。実 HLS クライアントは
// プレイリスト本文の URI をプレイリストの URL 基準で相対解決するので、テストも
// 同じ経路を通す。
func firstSegmentURL(t *testing.T, plURL string) string {
	t.Helper()
	resp, err := http.Get(plURL)
	if err != nil {
		t.Fatalf("GET playlist %q: %v", plURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("playlist %q status = %d, want 200", plURL, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	uri := firstSegmentName(t, string(body))
	return resolveRelative(t, plURL, uri)
}

// 同じサービスへの同時リクエストは 1 本の ffmpeg（= mirakc への 1 リクエスト）に
// 収束する（受け入れ基準「同じサービスを 2 クライアントが見たとき、ffmpeg が
// 1 本しか起きない」）。
func TestLiveStreamer_SharedSession(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))
	_ = ls

	const serviceID = int64(1024)
	var wg sync.WaitGroup
	results := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(playlistURL(srv.URL, serviceID, "h264"))
			if err != nil {
				t.Errorf("GET playlist: %v", err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)
			results[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for i, code := range results {
		if code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, code)
		}
	}
	if got := state.requestCount(); got != 1 {
		t.Fatalf("mirakc stream requests = %d, want 1 (single ffmpeg for a shared service)", got)
	}

	// プレイリストが指すセグメント URI を、プレイリスト URL 基準で相対解決した先
	// から実際に配信できることも確認する（hls.js と同じ経路）。
	segURL := firstSegmentURL(t, playlistURL(srv.URL, serviceID, "h264"))

	segResp, err := http.Get(segURL)
	if err != nil {
		t.Fatalf("GET segment: %v", err)
	}
	defer func() { _ = segResp.Body.Close() }()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("segment status = %d, want 200", segResp.StatusCode)
	}
	segBody, _ := io.ReadAll(segResp.Body)
	if string(segBody) != "fake-ts-segment-data" {
		t.Errorf("segment body = %q, want %q", segBody, "fake-ts-segment-data")
	}
}

// X-Mirakurun-Priority に live.tuner_priority がそのまま載ること
// （チューナー調停の実装、issue #91 の決定 4）。
func TestLiveStreamer_UsesConfiguredTunerPriority(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.TunerPriority = 5
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	resp, err := http.Get(playlistURL(srv.URL, 1, "h264"))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	_ = resp.Body.Close()

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.priorities) != 1 || state.priorities[0] != "5" {
		t.Errorf("priorities = %v, want [5]", state.priorities)
	}
}

// 同時セッション上限はプロセスローカル。超えた要求は 503 になり、既存セッションは
// 壊れない（受け入れ基準）。
func TestLiveStreamer_SessionLimit(t *testing.T) {
	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.MaxSessions = 1
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	resp1, err := http.Get(playlistURL(srv.URL, 1, "h264"))
	if err != nil {
		t.Fatalf("GET playlist (1st service): %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("1st service status = %d, want 200", resp1.StatusCode)
	}

	resp2, err := http.Get(playlistURL(srv.URL, 2, "h264"))
	if err != nil {
		t.Fatalf("GET playlist (2nd service): %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("2nd service status = %d, want 503 (process-local session limit)", resp2.StatusCode)
	}

	// 既存セッション（1st service）は壊れていない。
	resp1b, err := http.Get(playlistURL(srv.URL, 1, "h264"))
	if err != nil {
		t.Fatalf("GET playlist (1st service again): %v", err)
	}
	defer func() { _ = resp1b.Body.Close() }()
	if resp1b.StatusCode != http.StatusOK {
		t.Fatalf("1st service status after limit hit = %d, want 200", resp1b.StatusCode)
	}
	body1b, _ := io.ReadAll(resp1b.Body)
	if string(body1) != string(body1b) {
		t.Errorf("1st service playlist changed after limit hit: %q vs %q", body1, body1b)
	}
}

// 未知のプロファイルは 400 で、mirakc へは一切リクエストしない
// （セッションを起こす前に検証する）。
func TestLiveStreamer_UnknownProfile(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	resp, err := http.Get(playlistURL(srv.URL, 1, "does-not-exist"))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if got := state.requestCount(); got != 0 {
		t.Errorf("mirakc requests = %d, want 0 (rejected before starting a session)", got)
	}
}

// site が config.mirakc.site と一致しない要求は 404（DB を引かずパスだけで判定する）。
func TestLiveStreamer_SiteMismatch(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	resp, err := http.Get(fmt.Sprintf("%s/api/sites/other-site/services/1/live/playlist.m3u8", srv.URL))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if got := state.requestCount(); got != 0 {
		t.Errorf("mirakc requests = %d, want 0", got)
	}
}

// URL にセッション ID が現れず、正規表現 1 本で (site, serviceId) が取り出せること
// （前段の consistent hash 鍵になるための固定深さ制約、issue #56 / #91）。
//
// レビュー指摘（必須 3）: 以前のこのテストはテスト自身が書いたリテラル文字列に
// 正規表現を当てるだけで、Mount が実際に登録するルートを一切見ていなかった。
// 実装の base を `/api/sites/{site}/services/{serviceId}/live/sessions/{sessionId}`
// に変えても素通しした（レビュアが実証）。ここでは `chi.Walk` で**実際にマウント
// されたルートパターンの集合**を取り出し、期待する 2 本と厳密に一致することを見る
// --- ルートの形が 1 文字でも変われば必ず落ちる（余分な変数セグメントを許す
// 正規表現より、期待値との完全一致の方が「セッション ID を意味する変数名か」を
// 当てずっぽうで判定せずに済む）。
func TestLiveStreamer_URLPathFixedDepth(t *testing.T) {
	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	client := mirakc.NewClient(mirakcSrv.URL, nil)
	ls := NewLive(client, testLiveSite, baseLiveConfig(t))
	r := chi.NewRouter()
	ls.Mount(r)
	// firstSegmentURL がセッションを張るので、他のテストと同じく
	// mirakc への持続接続を閉じてから httptest サーバーを閉じる。
	t.Cleanup(ls.shutdown)

	routes := walkLiveRoutes(t, r)
	want := []string{
		"/api/sites/{site}/services/{serviceId}/live/playlist.m3u8",
		"/api/sites/{site}/services/{serviceId}/live/segments/{name}",
	}
	slices.Sort(routes)
	slices.Sort(want)
	if !slices.Equal(routes, want) {
		t.Fatalf("mounted live routes = %v, want exactly %v", routes, want)
	}

	// 実際に 200 が返る要求の URL にも、docs/operations.md §5 の nginx map と同じ
	// 正規表現で (site, serviceId) が取り出せることを確認する（ルートの形だけでなく、
	// 実在の URL でも成立する）。
	re := regexp.MustCompile(`^/api/sites/([^/]+)/services/([^/]+)/live/`)
	plURL := playlistURL(newLiveTestServerURL(t, r), 1024, "h264")
	segURL := firstSegmentURL(t, plURL)

	for _, raw := range []string{plURL, segURL} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing %q: %v", raw, err)
		}
		m := re.FindStringSubmatch(u.Path)
		if m == nil || m[1] != "default" || m[2] != "1024" {
			t.Errorf("request path %q did not match the fixed-depth regex: %v", u.Path, m)
		}
		// クエリ文字列に鍵（site/serviceId）を置いていないことも確認する
		// （profile はクエリ側に許すが、鍵はパス側のみ）。
		if u.RawQuery != "" && (strings.Contains(u.RawQuery, "site") || strings.Contains(u.RawQuery, "service")) {
			t.Errorf("query %q must not carry the hash key", u.RawQuery)
		}
	}
}

// walkLiveRoutes は r にマウントされている全ルートパターンを返す。
func walkLiveRoutes(t *testing.T, r chi.Router) []string {
	t.Helper()
	var routes []string
	err := chi.Walk(r, func(method, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		routes = append(routes, route)
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	return routes
}

// newLiveTestServerURL は r を httptest でリスンさせ、その URL を返す（このテスト
// だけの使い捨てサーバーなので t.Cleanup で閉じる）。
func newLiveTestServerURL(t *testing.T, r chi.Router) string {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

// idle GC はサービス単位で、セグメント要求が idle timeout の間来なければ ffmpeg を
// 止め、mirakc 側の接続も閉じる（受け入れ基準: 「クライアントが離れたら ffmpeg
// プロセスが実際に消える」）。
func TestLiveStreamer_IdleGC_StopsSession(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.IdleTimeout = 30 * time.Millisecond
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	segURL := firstSegmentURL(t, playlistURL(srv.URL, 42, "h264"))

	if got := state.requestCount(); got != 1 {
		t.Fatalf("mirakc requests = %d, want 1", got)
	}

	dir := filepath.Join(cfg.SegmentDir, testLiveSite, "42")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("session dir should exist while active: %v", err)
	}

	// idle timeout を過ぎるまで待ってから GC を回す。
	time.Sleep(60 * time.Millisecond)
	ls.reapIdle()

	select {
	case sid := <-state.disconnected:
		if sid != 42 {
			t.Errorf("disconnected service id = %d, want 42", sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mirakc connection was not closed within 2s of idle GC (tuner not released)")
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("session dir should be removed after idle GC, stat err = %v", err)
	}

	// セッションが無くなっているので、同じセグメントの再要求は 404
	// （hls.js はここでプレイリストを再取得し、新しいセッションを起こす）。
	segResp, err := http.Get(segURL)
	if err != nil {
		t.Fatalf("GET segment after GC: %v", err)
	}
	defer func() { _ = segResp.Body.Close() }()
	if segResp.StatusCode != http.StatusNotFound {
		t.Errorf("segment status after GC = %d, want 404", segResp.StatusCode)
	}
}

// idle GC の逆方向: セグメント要求（touch）が続いていれば idle timeout を過ぎても
// 止めない。片方向だけの確認では「常に止める」に反転していても気付けない
// （テスト規律「分岐を直したら両方向で確認する」）。
func TestLiveStreamer_IdleGC_TouchKeepsSessionAlive(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.IdleTimeout = 200 * time.Millisecond
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	segURL := firstSegmentURL(t, playlistURL(srv.URL, 7, "h264"))

	// idle timeout より短い間隔でセグメントを要求し続け、GC が走っても消えないことを見る。
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		segResp, err := http.Get(segURL)
		if err != nil {
			t.Fatalf("GET segment: %v", err)
		}
		_ = segResp.Body.Close()
		ls.reapIdle()
		time.Sleep(50 * time.Millisecond)
	}

	select {
	case sid := <-state.disconnected:
		t.Fatalf("session for service %d was reclaimed despite being touched", sid)
	default:
	}

	finalResp, err := http.Get(segURL)
	if err != nil {
		t.Fatalf("GET segment (final): %v", err)
	}
	defer func() { _ = finalResp.Body.Close() }()
	if finalResp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (session must still be alive)", finalResp.StatusCode)
	}
}

// プロセス停止（Run の ctx キャンセル）で全セッションが止まり、mirakc 側の接続も
// 残らない（受け入れ基準: 「streamer プロセスを止めるとチューナーが解放される」）。
func TestLiveStreamer_Run_StopsAllSessionsOnShutdown(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.IdleTimeout = time.Hour // GC では止まらない距離にしておく
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	resp, err := http.Get(playlistURL(srv.URL, 99, "h264"))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	_ = resp.Body.Close()
	if got := state.requestCount(); got != 1 {
		t.Fatalf("mirakc requests = %d, want 1", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- ls.Run(ctx) }()
	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after ctx cancellation (session not stopped)")
	}

	select {
	case sid := <-state.disconnected:
		if sid != 99 {
			t.Errorf("disconnected service id = %d, want 99", sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mirakc connection was not closed on shutdown (tuner not released)")
	}

	// shutdown 後は新規セッションを受け付けない。
	resp2, err := http.Get(playlistURL(srv.URL, 100, "h264"))
	if err != nil {
		t.Fatalf("GET playlist after shutdown: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status after shutdown = %d, want 503", resp2.StatusCode)
	}
}

// レビュー指摘（必須 5）: プロセスが落ちても（SIGTERM が効かない SIGKILL 等）
// セグメント残骸が溜まらないこと。tmpfs はノード再起動でしか消えないため、
// 「crash したら tmpfs 自体が消える」には頼れない --- 起動時（NewLive、HTTP
// リスナーが立つ前）に live.segment_dir を掃くことで対処する。
func TestLiveStreamer_NewLive_SweepsStaleSegmentDirAtStartup(t *testing.T) {
	cfg := baseLiveConfig(t)

	// 前回プロセスの残骸を模す: 生きているセッションの形をした、しかし
	// 誰も管理していないディレクトリ。
	staleDir := filepath.Join(cfg.SegmentDir, testLiveSite, "123456")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatalf("seeding stale dir: %v", err)
	}
	stalePlaylist := filepath.Join(staleDir, "h264.m3u8")
	if err := os.WriteFile(stalePlaylist, []byte("#EXTM3U\nstale\n"), 0o644); err != nil {
		t.Fatalf("seeding stale playlist: %v", err)
	}

	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	client := mirakc.NewClient(mirakcSrv.URL, nil)
	_ = NewLive(client, testLiveSite, cfg)

	if _, err := os.Stat(stalePlaylist); !os.IsNotExist(err) {
		t.Errorf("stale playlist should be swept at startup, stat err = %v", err)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("stale session dir should be swept at startup, stat err = %v", err)
	}
}

// 受け入れ基準そのもの: 「同じサービスを 2 クライアントが見たとき ... 片方が
// 離れてももう片方が切れない」を、離脱するクライアント A と居続けるクライアント B の
// 2 者で明示的に確認する（`TouchKeepsSessionAlive` は 1 クライアントの持続だけを
// 見ており、「複数クライアントの共有セッションで一方だけの離脱」を踏んでいない。
// レビューで指摘された必須 6）。
func TestLiveStreamer_OneClientLeavingDoesNotStopTheOther(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.IdleTimeout = 120 * time.Millisecond
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	const serviceID = int64(55)

	// クライアント A: 最初にプレイリストを取得してセッションを起こすが、その後は
	// 何も要求せず離脱する。ffmpeg の起動（偽物でもプロセス起動コストがある）が
	// idle timeout に対して遅いことがあるため、この 1 回目の取得時間そのものを
	// idle 判定の基準にしない --- 直後に 1 回セグメントを取り直して lastAccess を
	// 「今」に揃えてから idle 判定ループへ入る。
	segURL := firstSegmentURL(t, playlistURL(srv.URL, serviceID, "h264"))
	if resp, err := http.Get(segURL); err == nil {
		_ = resp.Body.Close()
	}

	// クライアント B: 同じサービスを見続ける（idle timeout より短い間隔でセグメントを
	// 取り続ける）。
	stopB := make(chan struct{})
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		for {
			select {
			case <-stopB:
				return
			default:
			}
			resp, err := http.Get(segURL)
			if err == nil {
				_ = resp.Body.Close()
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		select {
		case <-stopB:
		default:
			close(stopB)
		}
		<-bDone
	})

	// A は離脱済み（何もしない）。B だけが持続する間、idle GC を何度回しても
	// セッションは生き続けなければならない。
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		ls.reapIdle()
		time.Sleep(40 * time.Millisecond)
	}

	select {
	case sid := <-state.disconnected:
		t.Fatalf("session for service %d was reclaimed while client B was still active (client A merely left, it did not ask to stop the session)", sid)
	default:
	}

	// 明示的に確認: A が最初に見た URL がまだ有効（B のおかげ）。
	finalResp, err := http.Get(segURL)
	if err != nil {
		t.Fatalf("GET segment (final): %v", err)
	}
	defer func() { _ = finalResp.Body.Close() }()
	if finalResp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (B keeps the shared session alive after A left)", finalResp.StatusCode)
	}

	// B も離脱すれば最終的に GC される（逆方向の確認。片方向だけでは
	// 「常に生き続ける」に壊れていても気付けない）。
	close(stopB)
	<-bDone
	time.Sleep(150 * time.Millisecond) // B 離脱後、idle timeout を超えさせる
	ls.reapIdle()
	select {
	case sid := <-state.disconnected:
		if sid != serviceID {
			t.Errorf("disconnected service id = %d, want %d", sid, serviceID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session was not reclaimed after both clients left")
	}
}

func TestBuildLiveFFmpegArgs(t *testing.T) {
	profiles := []LiveProfile{
		{Name: "h264", VideoCodec: "libx264", AudioCodec: "aac", Height: 720, Preset: "veryfast", SegmentSeconds: 2, PlaylistSize: 6, ExtraArgs: []string{"-b:v", "2M"}},
		{Name: "h264low", VideoCodec: "libx264", AudioCodec: "aac", Height: 360, SegmentSeconds: 4, PlaylistSize: 3},
	}
	args := BuildLiveFFmpegArgs(profiles, "/tmp/live/1")

	if !slices.Contains(args, "-i") {
		t.Fatal("missing -i")
	}
	if got := args[slices.Index(args, "-i")+1]; got != "pipe:0" {
		t.Errorf("input = %q, want pipe:0 (streamer fetches from mirakc itself)", got)
	}
	// ARIB caption を map すると Debian ffmpeg が exit 1 する（実 mirakc で観測）。
	// 映像・音声だけを明示 map する契約を固定する。
	if !slices.Contains(args, "0:v:0") || !slices.Contains(args, "0:a:0") {
		t.Errorf("missing -map 0:v:0 / 0:a:0 (would pick up arib_caption and die): %v", args)
	}
	if slices.Contains(args, "0:s:0") || slices.Contains(args, "0:d:0") {
		t.Errorf("must not map subtitle/data streams: %v", args)
	}

	// 2 プロファイルぶんの出力（.m3u8）が両方含まれる = 1 回の起動で両方出す。
	m3u8Count := 0
	for _, a := range args {
		if strings.HasSuffix(a, ".m3u8") {
			m3u8Count++
		}
	}
	if m3u8Count != 2 {
		t.Errorf("m3u8 outputs = %d, want 2 (one ffmpeg, all profiles)", m3u8Count)
	}
	if !slices.Contains(args, "/tmp/live/1/h264.m3u8") {
		t.Errorf("missing h264 playlist path: %v", args)
	}
	if !slices.Contains(args, "/tmp/live/1/h264low.m3u8") {
		t.Errorf("missing h264low playlist path: %v", args)
	}
	if !slices.Contains(args, "/tmp/live/1/segments/h264_seg%05d.ts") {
		t.Errorf("missing h264 segment pattern: %v", args)
	}
	// -hls_base_url が無いと ffmpeg はプレイリストに basename しか書かず、実 HLS
	// クライアントの相対解決が `-hls_segment_filename` の物理パス（segments/ 配下）
	// と食い違って 404 になる（レビューで発見した回帰。実 ffmpeg 8.1.2 で確認済み）。
	if !slices.Contains(args, "-hls_base_url") {
		t.Fatal("missing -hls_base_url (playlist URIs would be bare basenames, mismatching the segments/ route)")
	}
	if got := args[slices.Index(args, "-hls_base_url")+1]; got != "segments/" {
		t.Errorf("-hls_base_url = %q, want %q", got, "segments/")
	}
	if !slices.Contains(args, "scale=-2:720") {
		t.Errorf("missing scale filter for h264: %v", args)
	}
	if !slices.Contains(args, "veryfast") {
		t.Errorf("missing preset: %v", args)
	}
	if !slices.Contains(args, "-b:v") || !slices.Contains(args, "2M") {
		t.Errorf("missing extra_args: %v", args)
	}

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "&&") || strings.Contains(joined, "|") {
		t.Errorf("args look like a shell pipeline: %v", args)
	}
}

// Enabled=false のときは Mount がライブのルートを一切登録しない
// （ffmpeg 無し環境で streamer を壊さないための、ffmpeg を一切 exec しない側の保証）。
func TestLiveStreamer_Disabled_DoesNotMountRoutes(t *testing.T) {
	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.Enabled = false
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	resp, err := http.Get(playlistURL(srv.URL, 1, "h264"))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (route must not exist)", resp.StatusCode)
	}
}

// LiveActiveSessions ゲージが実際のセッション数を反映すること
// （per-process gauge。docs/operations.md §5）。
func TestLiveStreamer_ActiveSessionsGauge(t *testing.T) {
	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.IdleTimeout = time.Hour
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	resp, err := http.Get(playlistURL(srv.URL, 1, "h264"))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	_ = resp.Body.Close()

	if got := ls.sessionCount(); got != 1 {
		t.Fatalf("sessionCount = %d, want 1", got)
	}
	if got := gaugeValue(t, metrics.LiveActiveSessions); got != 1 {
		t.Errorf("rokuban_live_active_sessions = %v, want 1", got)
	}

	ls.shutdown()
	if got := gaugeValue(t, metrics.LiveActiveSessions); got != 0 {
		t.Errorf("rokuban_live_active_sessions after shutdown = %v, want 0", got)
	}
}

// レビュー指摘（必須 1）: プレイリスト本文が書くセグメント URI は、プレイリスト
// **自身の URL を基準に相対解決した先が実際に配信されているルート**でなければ
// ならない。旧テストは `/segments/{name}` という文字列を自前で組み立てて GET して
// おり、hls.js のような実クライアントの相対解決を模していなかったため、
// `-hls_base_url` が抜けていて basename しか書かれない実装のバグ
// （実 ffmpeg 8.1.2 で確認済み）を検出できなかった。`firstSegmentURL` を経由する
// ことで、この経路を通るテストすべてが同じ回帰を検出できる。
func TestLiveStreamer_PlaylistSegmentURIsResolveToServingRoute(t *testing.T) {
	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	segURL := firstSegmentURL(t, playlistURL(srv.URL, 1, "h264"))

	segResp, err := http.Get(segURL)
	if err != nil {
		t.Fatalf("GET resolved segment URL %q: %v", segURL, err)
	}
	defer func() { _ = segResp.Body.Close() }()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("resolved segment URL %q status = %d, want 200 (the playlist URI must resolve to a route this server actually serves)",
			segURL, segResp.StatusCode)
	}
	segBody, _ := io.ReadAll(segResp.Body)
	if string(segBody) != "fake-ts-segment-data" {
		t.Errorf("segment body = %q, want %q", segBody, "fake-ts-segment-data")
	}
}

// gaugeValue はテストのためだけに prometheus.Gauge の現在値を読む。
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("reading gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}
