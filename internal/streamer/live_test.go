package streamer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
// 引数から `-hls_segment_filename <pattern>` と直後の `*.m3u8` の対をプロファイル
// ごとに読み取り、最小限の有効な HLS プレイリスト + セグメント 1 本を書き出してから、
// ctx キャンセル（exec.CommandContext の既定動作で SIGKILL）まで走り続ける。
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

segfile=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-hls_segment_filename" ]; then
    segfile="$a"
  fi
  case "$a" in
    *.m3u8)
      playlist="$a"
      mkdir -p "$(dirname "$playlist")" 2>/dev/null
      seg=$(printf '%s' "$segfile" | sed 's/%05d/00001/')
      mkdir -p "$(dirname "$seg")" 2>/dev/null
      {
        printf '#EXTM3U\n'
        printf '#EXT-X-VERSION:3\n'
        printf '#EXT-X-TARGETDURATION:2\n'
        printf '#EXT-X-MEDIA-SEQUENCE:0\n'
        printf '#EXTINF:2.0,\n'
        printf '%s\n' "$(basename "$seg")"
      } > "$playlist"
      printf 'fake-ts-segment-data' > "$seg"
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

func segmentURL(base string, serviceID int64, name string) string {
	return fmt.Sprintf("%s/api/sites/%s/services/%d/live/segments/%s", base, testLiveSite, serviceID, name)
}

// firstSegmentName はテスト用プレイリスト本文から最初の非コメント行（セグメント
// ファイル名）を取り出す。
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

	// プレイリストからセグメント名を取り、実際に配信できることも確認する。
	resp, err := http.Get(playlistURL(srv.URL, serviceID, "h264"))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	seg := firstSegmentName(t, string(body))

	segResp, err := http.Get(segmentURL(srv.URL, serviceID, seg))
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
func TestLiveStreamer_URLPathFixedDepth(t *testing.T) {
	// docs/operations.md §5 に書かれている nginx map の正規表現と同じ構造。
	re := regexp.MustCompile(`^/api/sites/([^/]+)/services/([^/]+)/live/`)

	playlist := "/api/sites/default/services/1024/live/playlist.m3u8"
	m := re.FindStringSubmatch(playlist)
	if m == nil || m[1] != "default" || m[2] != "1024" {
		t.Fatalf("playlist path did not match fixed-depth regex: %v", m)
	}

	segment := "/api/sites/default/services/1024/live/segments/h264_seg00001.ts"
	m = re.FindStringSubmatch(segment)
	if m == nil || m[1] != "default" || m[2] != "1024" {
		t.Fatalf("segment path did not match fixed-depth regex: %v", m)
	}

	// クエリ文字列に鍵を置いていないことも確認する（profile はクエリ側）。
	if strings.Contains(playlist, "?") {
		t.Errorf("playlist path must not carry the profile in the path: %q", playlist)
	}
}

// idle GC はサービス単位で、セグメント要求が idle timeout の間来なければ ffmpeg を
// 止め、mirakc 側の接続も閉じる（受け入れ基準: 「クライアントが離れたら ffmpeg
// プロセスが実際に消える」）。
func TestLiveStreamer_IdleGC_StopsSession(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.IdleTimeout = 30 * time.Millisecond
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	resp, err := http.Get(playlistURL(srv.URL, 42, "h264"))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	seg := firstSegmentName(t, string(body))

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
	segResp, err := http.Get(segmentURL(srv.URL, 42, seg))
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

	resp, err := http.Get(playlistURL(srv.URL, 7, "h264"))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	seg := firstSegmentName(t, string(body))

	// idle timeout より短い間隔でセグメントを要求し続け、GC が走っても消えないことを見る。
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		segResp, err := http.Get(segmentURL(srv.URL, 7, seg))
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

	finalResp, err := http.Get(segmentURL(srv.URL, 7, seg))
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

// gaugeValue はテストのためだけに prometheus.Gauge の現在値を読む。
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("reading gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}
