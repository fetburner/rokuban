package streamer

import (
	"bytes"
	"context"
	"errors"
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
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/fetburner/rokuban/internal/api"
	"github.com/fetburner/rokuban/internal/ffargs"
	"github.com/fetburner/rokuban/internal/metrics"
	"github.com/fetburner/rokuban/internal/mirakc"
)

const testLiveSite = "default"

// fakeMirakcLiveState は偽 mirakc サーバーの観測結果。
type fakeMirakcLiveState struct {
	mu           sync.Mutex
	requests     int
	priorities   []string
	requestURIs  []string
	disconnected chan int64
}

func (s *fakeMirakcLiveState) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// requestURIList は mirakc が受け取った生の Request-URI（パス + クエリ）を返す。
// 「streamer が mirakc に何を送ったか」を測る唯一の観測点（issue #217）。
func (s *fakeMirakcLiveState) requestURIList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requestURIs)
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
		// r.RequestURI は net/http が受け取った生の Request-URI（デコード前）。
		// r.URL.Path を見ると %2F が '/' に戻ってしまい、「別エンドポイントへの
		// 要求に化けていないか」の観測にならない。
		state.requestURIs = append(state.requestURIs, r.RequestURI)
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

// newFastFakeMirakcLiveServer は newFakeMirakcLiveServer と同じ GET
// /api/services/{id}/stream を実装するが、10ms ごとに 188 byte という
// スロットリングを入れない（tight loop で書く）。captions 経路は起動時に
// upstream の先頭 liveCaptionProbeBytes（512 KiB）を同期に読み切る
// （readLiveCaptionPrefix）ため、newFakeMirakcLiveServer のレート
// （188 byte / 10ms ≈ 18.8 KB/s）だと 512 KiB に達するまで 25 秒以上かかり、
// playlistStartupTimeout（15s）を超えてテストが確実にタイムアウトする。
func newFastFakeMirakcLiveServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/MP2T")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		buf := bytes188Packet()
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := w.Write(buf); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
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

// playlistURL は SI の (networkId, serviceId) からプレイリスト URL を組み立てる。
// パスに載るのは SI の値そのもの（合成 id ではない。issue #217）。
func playlistURL(base string, networkID, serviceID int, profile string) string {
	u := fmt.Sprintf("%s/api/sites/%s/networks/%d/services/%d/live/playlist.m3u8",
		base, testLiveSite, networkID, serviceID)
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

	const serviceID = 1024
	var wg sync.WaitGroup
	results := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(playlistURL(srv.URL, 0, serviceID, "h264"))
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
	segURL := firstSegmentURL(t, playlistURL(srv.URL, 0, serviceID, "h264"))

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

	resp, err := http.Get(playlistURL(srv.URL, 0, 1, "h264"))
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

	resp1, err := http.Get(playlistURL(srv.URL, 0, 1, "h264"))
	if err != nil {
		t.Fatalf("GET playlist (1st service): %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("1st service status = %d, want 200", resp1.StatusCode)
	}

	resp2, err := http.Get(playlistURL(srv.URL, 0, 2, "h264"))
	if err != nil {
		t.Fatalf("GET playlist (2nd service): %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("2nd service status = %d, want 503 (process-local session limit)", resp2.StatusCode)
	}

	// 既存セッション（1st service）は壊れていない。
	resp1b, err := http.Get(playlistURL(srv.URL, 0, 1, "h264"))
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
// installFakeLiveFFmpegCaptions は captions 経路（buildLiveCaptionFFmpegArgs）の
// argv を読んで、master / variant / 字幕 playlist と .ts / .vtt セグメントを
// 一式書き出す偽 ffmpeg。installFakeLiveFFmpeg（非 captions 用）と分けているのは、
// captions の argv には `-master_pl_name playlist.m3u8`（相対名。ディレクトリを
// 含まない値）のようにそのままでは書き込み先にならないトークンが混じり、
// 汎用スクリプトの「`*.m3u8` に一致したら即書き込む」処理では捕まえられない
// （このバグが安全側 --- カレントディレクトリを汚染する形の失敗にはならず、
// 静かに `dir` を取り違えて確実にテストが失敗する）ため。
//
// 末尾の位置引数（`dir/playlist_%v.m3u8`）から dir を確定させ、1 プロファイル
// （variant "0"）ぶんの
//   - dir/segments/0_seg00001.ts（映像セグメント）
//   - dir/playlist_0.m3u8（variant playlist、#EXTINF を含む）
//   - dir/playlist.m3u8（master、#EXT-X-STREAM-INF を含む）
//   - `-hls_subtitle_path` が渡っていれば dir/subtitles_0.m3u8（字幕
//     playlist、#EXTINF を含む）と dir/seg0.vtt（VTT セグメント）
//
// を書く。Content-Type 判定は名前の拡張子だけを見るので、実データの中身は
// 空でも配信側の検証には十分（waitForPlaylist が見るのは #EXTINF の有無だけ）。
func installFakeLiveFFmpegCaptions(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffmpeg-live-captions")
	script := `#!/bin/sh
cat >/dev/null &

segfile=""
subpath=""
prev=""
lastarg=""
for a in "$@"; do
  if [ "$prev" = "-hls_segment_filename" ]; then
    segfile="$a"
  fi
  if [ "$prev" = "-hls_subtitle_path" ]; then
    subpath="$a"
  fi
  prev="$a"
  lastarg="$a"
done

outdir=$(dirname "$lastarg")
mkdir -p "$outdir/segments"

seg=$(printf '%s' "$segfile" | sed 's/%v/0/; s/%05d/00001/')
mkdir -p "$(dirname "$seg")" 2>/dev/null
printf 'fake-ts-segment-data' > "$seg.tmp"
mv "$seg.tmp" "$seg"

variant="$outdir/playlist_0.m3u8"
{
  printf '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.0,\n'
  printf 'segments/%s\n' "$(basename "$seg")"
} > "$variant.tmp"
mv "$variant.tmp" "$variant"

# 字幕まわり（vtt / 字幕 playlist）を master より先に書く。**master の
# readiness（#EXT-X-STREAM-INF の出現）を待てば、それより後に書く全ファイルの
# 存在も保証される**というテスト側の前提（順序に依存した readiness）を成立
# させるための順序。逆にすると、master を待った直後に字幕ファイルへ直接
# アクセスするテストが競合しうる。
if [ -n "$subpath" ]; then
  vttseg="$outdir/seg0.vtt"
  printf 'WEBVTT\n\n' > "$vttseg.tmp"
  mv "$vttseg.tmp" "$vttseg"

  subplaylist="$outdir/subtitles_0.m3u8"
  {
    printf '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.0,\n'
    printf 'segments/seg0.vtt\n'
  } > "$subplaylist.tmp"
  mv "$subplaylist.tmp" "$subplaylist"
fi

master="$outdir/playlist.m3u8"
{
  printf '#EXTM3U\n#EXT-X-VERSION:3\n'
  if [ -n "$subpath" ]; then
    printf '#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="subtitle_0",DEFAULT=YES,URI="subtitles_0.m3u8"\n'
  fi
  printf '#EXT-X-STREAM-INF:BANDWIDTH=1000\nplaylist_0.m3u8\n'
} > "$master.tmp"
mv "$master.tmp" "$master"

while true; do
  sleep 1
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// installFakeFFprobeAlwaysReportsSubtitle は probeLiveCaptionStream が呼ぶ
// ffprobe（`-select_streams s -show_entries stream=index -of csv=p=0`）を偽装し、
// 常に字幕ストリーム有りと報告する。実 ffprobe を fake の生パケット列
// （PAT/PMT を持たない）に向けると失敗する（実測: exit 1）ため、captions の
// 一連の書き出しを試験するにはこれで置き換える必要がある。
func installFakeFFprobeAlwaysReportsSubtitle(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffprobe script assumes a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-ffprobe-subtitle")
	script := "#!/bin/sh\necho 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLiveStreamer_Segment_CaptionsEnabled_ServesVTTAndM3U8AndTS は issue #430
// の受け入れ基準を Segment ハンドラのレベルで固定する（着手前は Segment に
// 専用のテストが 1 本も無かった）: captions 有効時は .ts / .vtt / .m3u8 の
// いずれも 200 で、Content-Type がそれぞれ video/mp2t・text/vtt・
// application/vnd.apple.mpegurl になる。
func TestLiveStreamer_Segment_CaptionsEnabled_ServesVTTAndM3U8AndTS(t *testing.T) {
	mirakcSrv := newFastFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.Captions = true
	cfg.FFmpeg = installFakeLiveFFmpegCaptions(t)
	cfg.FFprobe = installFakeFFprobeAlwaysReportsSubtitle(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	// master playlist を先に取得し、readiness（#EXT-X-STREAM-INF の出現）を
	// 待つ。偽 ffmpeg は master を全ファイルの最後に書くので、これが返れば
	// 以下の個別ファイルは全て存在が保証される（.ts/.vtt は waitForPlaylist の
	// ような待ち合わせを持たない生の http.ServeFile 経路なので、先に readiness
	// を確認しないと書き込みとの競合で 404 になりうる --- 実際に最初の実装は
	// これを端折って ts/vtt が 404 になっていた）。
	plResp, err := http.Get(playlistURL(srv.URL, 0, 1, ""))
	if err != nil {
		t.Fatalf("GET master playlist: %v", err)
	}
	_ = plResp.Body.Close()
	if plResp.StatusCode != http.StatusOK {
		t.Fatalf("master playlist status = %d, want 200", plResp.StatusCode)
	}

	cases := []struct {
		name        string
		path        string
		wantType    string
		description string
	}{
		{"ts", "0_seg00001.ts", "video/mp2t", "video segment"},
		{"vtt", "seg0.vtt", "text/vtt; charset=utf-8", "subtitle segment"},
		{"variant m3u8", "playlist_0.m3u8", "application/vnd.apple.mpegurl", "variant playlist"},
		{"subtitle m3u8", "subtitles_0.m3u8", "application/vnd.apple.mpegurl", "subtitle playlist"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			url := fmt.Sprintf("%s/api/sites/%s/networks/0/services/1/live/segments/%s", srv.URL, testLiveSite, tt.path)
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET %s (%s): %v", url, tt.description, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200 (%s)", resp.StatusCode, tt.description)
			}
			if got := resp.Header.Get("Content-Type"); got != tt.wantType {
				t.Errorf("Content-Type = %q, want %q (%s)", got, tt.wantType, tt.description)
			}
		})
	}
}

// TestLiveStreamer_Segment_WaitsForVariantPlaylistContent は D の決定
// （variant / 字幕 playlist にも waitForPlaylist の readiness 待ちを通す）を、
// ファイルの出現をテスト側で意図的に遅らせて直接検証する。
// TestLiveStreamer_Segment_CaptionsEnabled_ServesVTTAndM3U8AndTS は偽 ffmpeg が
// 全ファイルを完全に同期的に（1 つのシェルスクリプトの中で順番に）書くため、
// D が守ろうとしている「書き込み途中に読まれる窓」をそもそも再現できない ---
// このテストはセッションを直接注入し、ファイル書き込みを 200ms 遅らせることで
// その窓を確実に作る。
//
// 壊し方: Segment の `if ls.cfg.Captions && strings.HasSuffix(name, ".m3u8")`
// 分岐（readiness 待ち）を削除して素の http.ServeFile に戻す --- まだ存在
// しないファイルに対して即座に 404 が返り、elapsed が遅延（200ms）よりずっと
// 短くなることで検出する。
func TestLiveStreamer_Segment_WaitsForVariantPlaylistContent(t *testing.T) {
	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.Captions = true
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	dir := t.TempDir()
	serviceID := mirakc.ServiceID(0, 1)
	s := &liveSession{
		serviceID:  serviceID,
		dir:        dir,
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
		lastAccess: time.Now(),
		cancel:     func() {},
	}
	close(s.ready)
	ls.mu.Lock()
	ls.sessions[serviceID] = s
	ls.mu.Unlock()
	t.Cleanup(func() { close(s.done) })

	playlistPath := filepath.Join(dir, "playlist_0.m3u8")
	const delay = 200 * time.Millisecond
	go func() {
		time.Sleep(delay)
		// waitForPlaylist が期待する atomic write（temp file + rename）。
		content := []byte("#EXTM3U\n#EXTINF:2.0,\nsegments/seg.ts\n")
		tmp := playlistPath + ".tmp"
		if err := os.WriteFile(tmp, content, 0o644); err != nil {
			t.Errorf("writing delayed playlist: %v", err)
			return
		}
		if err := os.Rename(tmp, playlistPath); err != nil {
			t.Errorf("renaming delayed playlist: %v", err)
		}
	}()

	start := time.Now()
	url := fmt.Sprintf("%s/api/sites/%s/networks/0/services/1/live/segments/playlist_0.m3u8", srv.URL, testLiveSite)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must wait for the delayed write instead of 404ing immediately)", resp.StatusCode)
	}
	if elapsed < delay-50*time.Millisecond {
		t.Errorf("responded after %v, want to have waited close to %v for the delayed write "+
			"(too fast: did it actually poll, or did something else make this pass by luck?)", elapsed, delay)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !bytes.Contains(body, []byte("#EXTINF")) {
		t.Errorf("response body = %q, want it to contain #EXTINF", body)
	}
}

// TestLiveStreamer_Segment_CaptionsDisabled_RejectsVTTAndM3U8 は captions 無効
// （既定）のとき、Segment が .vtt / .m3u8 を 400 で拒否し、.ts は従来どおり
// 200 を返すことを固定する。壊し方: Segment の
// `!ls.cfg.Captions && !strings.HasSuffix(name, ".ts")` ガードを消す。
func TestLiveStreamer_Segment_CaptionsDisabled_RejectsVTTAndM3U8(t *testing.T) {
	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	for _, name := range []string{"foo.vtt", "foo.m3u8"} {
		t.Run(name, func(t *testing.T) {
			url := fmt.Sprintf("%s/api/sites/%s/networks/0/services/1/live/segments/%s", srv.URL, testLiveSite, name)
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (captions disabled must reject non-.ts segment names)", resp.StatusCode)
			}
		})
	}

	// .ts は captions の有無に関わらず従来どおり 200。
	segURL := firstSegmentURL(t, playlistURL(srv.URL, 0, 1, "h264"))
	resp, err := http.Get(segURL)
	if err != nil {
		t.Fatalf("GET %s: %v", segURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf(".ts status = %d, want 200", resp.StatusCode)
	}
}

func TestLiveStreamer_UnknownProfile(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	resp, err := http.Get(playlistURL(srv.URL, 0, 1, "does-not-exist"))
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

// injectNeverReadySession は runSession を経ずに、起動待ちのまま（s.ready を
// 閉じない）セッションを ls.sessions に直接注入する。getOrCreateSession 経由で
// 作ると呼び出し側もろとも ready を待ってしまい、Segment / Playlist 単体の
// タイムアウト挙動を検証できない。
//
// newTestLiveStreamer が登録した t.Cleanup(ls.shutdown) は全セッションに
// stop()（cancel を呼んで <-s.done を待つ）を呼ぶ。この注入セッションは
// runSession を経ていない（s.done を閉じる者がいない）ため、何もしないと
// shutdown が無期限にハングする。t.Cleanup は LIFO なので、ここで（呼び出し元が
// newTestLiveStreamer の後で呼ぶ前提で）登録すれば shutdown より先に走り、
// s.done を閉じて詰まりを防げる。
func injectNeverReadySession(t *testing.T, ls *LiveStreamer, serviceID int64) *liveSession {
	t.Helper()
	s := &liveSession{
		serviceID:  serviceID,
		ready:      make(chan struct{}), // 意図的に閉じない = 起動待ちのまま
		done:       make(chan struct{}),
		lastAccess: time.Now(),
		cancel:     func() {},
	}
	ls.mu.Lock()
	ls.sessions[serviceID] = s
	ls.mu.Unlock()
	t.Cleanup(func() { close(s.done) })
	return s
}

// assertBoundedByStartupTimeout は elapsed が「その時点の playlistStartupTimeout
// ちょうどで打ち切られた」ことを確認する。**上限だけでなく下限も見る。** 上限
// だけだと、検証対象の経路が playlistStartupTimeout を読まず、別の（たまたま
// 短い）定数に差し替えられていても素通りしてしまう（レビュー指摘、issue #286
// の任意項目）。下限を「その時点の値」そのものと比較することで、テストが
// 上書きした値を経路が実際に読んでいることまで固定する。
func assertBoundedByStartupTimeout(t *testing.T, elapsed time.Duration) {
	t.Helper()
	if elapsed < playlistStartupTimeout {
		t.Errorf("elapsed %v is shorter than the current playlistStartupTimeout (%v) --- "+
			"the code path under test must be reading this same variable, not a different literal",
			elapsed, playlistStartupTimeout)
	}
	if elapsed > 1*time.Second {
		t.Errorf("elapsed %v took too long, want bounded by playlistStartupTimeout (%v)", elapsed, playlistStartupTimeout)
	}
}

// Segment はセッションの起動待ちで無期限に滞留しない（issue #189 の項目 1）。
// Playlist 側の waitForPlaylist と同じ playlistStartupTimeout で打ち切ることを
// 固定する --- ここを直す前は、起動処理（runSession）がまだ s.ready を閉じて
// いないセッションに対する Segment 要求は、リクエストしたクライアント自身が
// 切断するまで戻らなかった（mirakc への接続がハングした場合の実害。docs/api.md
// §ライブ視聴の HLS が前提とする idle GC はセッションが動き出してから効くもので、
// 起動待ち自体には効かない）。
func TestLiveStreamer_Segment_TimesOutWhenSessionNeverBecomesReady(t *testing.T) {
	// 15 秒の実待ちはテストを不必要に遅くするので、この 1 本だけ短くする
	// （罠: 他の待ちと定数を分けるとここが揃っているか気付けなくなるので、
	// 必ず playlistStartupTimeout そのものを書き換える）。
	prevTimeout := playlistStartupTimeout
	playlistStartupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { playlistStartupTimeout = prevTimeout })

	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	const serviceID = 999
	s := injectNeverReadySession(t, ls, serviceID)

	// テストのクライアント自身には十分大きいタイムアウトを設定する。直す前の
	// 実装のまま実行しても、ここでテストプロセスがハングせずアサーション失敗
	// （t.Fatalf）で終わるようにするための保険。
	client := &http.Client{Timeout: 2 * time.Second}
	segURL := fmt.Sprintf("%s/api/sites/%s/networks/0/services/%d/live/segments/h264_seg00001.ts",
		srv.URL, testLiveSite, serviceID)

	start := time.Now()
	resp, err := client.Get(segURL)
	if err != nil {
		t.Fatalf("GET segment: %v (segment request should time out with a response, not hang until the client gives up)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusGatewayTimeout)
	}
	assertBoundedByStartupTimeout(t, elapsed)

	// 起動待ちを諦めても close(s.ready) の性質は変えない --- セッション自体は
	// 「起動中」のまま残る（このハンドラの都合でセッションを壊してはいけない。
	// issue #189 の罠）。
	select {
	case <-s.ready:
		t.Errorf("s.ready must not be closed by the handler giving up on waiting")
	default:
	}
}

// Playlist もセッションの起動待ち（getOrCreateSession の <-s.ready）で無期限に
// 滞留しない（issue #286。#189 は Segment だけを直しており、Playlist 側の
// getOrCreateSession は当時直っていなかった --- レビューで指摘され、#286 の
// probe で実測された）。
//
// **Segment より実害が大きい。** hls.js はプレイリストを数秒間隔で再取得する
// のに対し、セグメント要求はプレイリストが 1 度成功した後にしか発生しない。
// mirakc がハングしている間、実際に滞留するのは主に Playlist ハンドラの側になる。
func TestLiveStreamer_Playlist_TimesOutWhenSessionNeverBecomesReady(t *testing.T) {
	prevTimeout := playlistStartupTimeout
	playlistStartupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { playlistStartupTimeout = prevTimeout })

	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	const serviceID = 998
	injectNeverReadySession(t, ls, serviceID)

	client := &http.Client{Timeout: 2 * time.Second}
	start := time.Now()
	resp, err := client.Get(playlistURL(srv.URL, 0, serviceID, ""))
	if err != nil {
		t.Fatalf("GET playlist: %v (playlist request should time out with a response, not hang until the client gives up)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusGatewayTimeout)
	}
	assertBoundedByStartupTimeout(t, elapsed)
}

// newHangingMirakcServer はヘッダを一切返さない偽 mirakc（接続は受け付けるが、
// クライアントが諦める・呼び出し元がこのサーバーを閉じるまで応答しない）。
// mirakc が完全にハングしている状況を、注入ではなく `StreamService` の
// 本物の HTTP 往復を通して再現するためのもの。
func newHangingMirakcServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// getOrCreateSession の**新規作成経路**（`ls.sessions` にまだ無い serviceID への
// 最初の要求。マップ挿入直後に `go ls.runSession` し、その `<-s.ready` を待つ側）
// も playlistStartupTimeout で打ち切られることを、実際に `runSession` →
// `StreamService` を通す経路で固定する（issue #286 の再指摘）。
//
// `TestLiveStreamer_Playlist_TimesOutWhenSessionNeverBecomesReady` は
// `injectNeverReadySession` で `ls.sessions` に直接セッションを注入するため、
// **既存セッション経路**（`live.go` の 1 つ目の select）しか通らない。
// `getOrCreateSession` には `<-s.ready` を待つ select が既存セッション経路・
// 新規作成経路の 2 か所にあり、これは互いに独立したコードパスなので、
// 片方だけに期限を入れて片方を忘れても上記のテストは検知できない。
//
// 実際にレビューで、新規作成経路側の `case <-time.After(playlistStartupTimeout):
// return nil, errStartupTimeout` を丸ごと削除して `go test ./internal/streamer/`
// を回しても全テスト green のままであることが指摘された。しかも新規作成経路は
// 「mirakc がハングしたときに最初のプレイリスト要求が通る側」（2 本目以降の
// 要求だけが既存セッション経路に入る）なので、本番で先に効くのはこちらである。
func TestLiveStreamer_Playlist_NewSessionTimesOutWhenMirakcNeverResponds(t *testing.T) {
	prevTimeout := playlistStartupTimeout
	playlistStartupTimeout = 100 * time.Millisecond
	t.Cleanup(func() { playlistStartupTimeout = prevTimeout })

	mirakcSrv := newHangingMirakcServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	// ls.sessions にまだ存在しない serviceID --- 最初の要求は必ず
	// getOrCreateSession の新規作成経路（マップ挿入 + go ls.runSession）を通る。
	const serviceID = 777

	client := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	resp, err := client.Get(playlistURL(srv.URL, 0, serviceID, ""))
	if err != nil {
		t.Fatalf("GET playlist: %v (new-session path should time out with a response, not hang until the client gives up)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusGatewayTimeout)
	}
	assertBoundedByStartupTimeout(t, elapsed)
}

// パスの id セグメントが SI の 16 bit 整数として読めない要求は、mirakc に
// 一切触れずに弾かれる（issue #217）。
//
// **これは「不明な id は mirakc が拒否する」という未測定の断言の置き換えである。**
// 以前の実装はパスの値をそのまま mirakc へ渡す前提だったので、「細工した値が
// mirakc の別エンドポイントへの要求に化けないこと」を Rokuban 側で示す手段が
// 無かった。ここでは弾く側（400/404）を、
// TestLiveStreamer_MirakcPathIsComposedFromPathSegments が通す側（mirakc が
// 実際に受け取る Request-URI）を固定するので、mirakc の挙動に依存しない。
func TestLiveStreamer_RejectsHostileIDSegments(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	tests := []struct {
		name string
		path string
		want int
	}{
		// パス区切りの注入。%2F はデコードされずセグメントの一部として届くため、
		// 400 で止まる（デコードされて別階層に化けるなら 404 になり、いずれに
		// せよ mirakc には届かない）。
		{"encoded path traversal", "/api/sites/default/networks/0/services/1%2F..%2Ftuners/live/playlist.m3u8", http.StatusBadRequest},
		// クエリの注入（`?decode=0` を service id 側にねじ込む）。
		{"encoded query injection", "/api/sites/default/networks/0/services/1%3Fdecode%3D0/live/playlist.m3u8", http.StatusBadRequest},
		{"non numeric", "/api/sites/default/networks/0/services/abc/live/playlist.m3u8", http.StatusBadRequest},
		// 空セグメントは chi のルートに一致してしまう（実測: 404 ではなく
		// ハンドラまで届く）ので、空文字を弾くのは parseSIID 側の責務になる。
		{"empty service id", "/api/sites/default/networks/0/services//live/playlist.m3u8", http.StatusBadRequest},
		{"negative", "/api/sites/default/networks/0/services/-1/live/playlist.m3u8", http.StatusBadRequest},
		{"signed plus", "/api/sites/default/networks/0/services/+1/live/playlist.m3u8", http.StatusBadRequest},
		{"hex", "/api/sites/default/networks/0/services/0x400/live/playlist.m3u8", http.StatusBadRequest},
		// SI の service_id は 16 bit。65536 以上は合成 id の桁を侵食するので弾く。
		{"beyond 16 bit", "/api/sites/default/networks/0/services/65536/live/playlist.m3u8", http.StatusBadRequest},
		{"overflow", "/api/sites/default/networks/0/services/99999999999999999999/live/playlist.m3u8", http.StatusBadRequest},
		{"fullwidth digits", "/api/sites/default/networks/0/services/１０２４/live/playlist.m3u8", http.StatusBadRequest},
		// 先頭ゼロは「同じチャンネルの別名 URL」になり、前段の consistent hash
		// （鍵は URL 文字列）が同じサービスを 2 Pod に割る。正準形だけ受ける。
		{"leading zero service id", "/api/sites/default/networks/0/services/01024/live/playlist.m3u8", http.StatusBadRequest},
		{"leading zero network id", "/api/sites/default/networks/00/services/1024/live/playlist.m3u8", http.StatusBadRequest},
		{"network id non numeric", "/api/sites/default/networks/abc/services/1024/live/playlist.m3u8", http.StatusBadRequest},
		{"network id beyond 16 bit", "/api/sites/default/networks/65536/services/1024/live/playlist.m3u8", http.StatusBadRequest},
		// 生の '/' は階層が変わるのでルート自体に一致しない。
		{"literal extra segment", "/api/sites/default/networks/0/services/1/stream/live/playlist.m3u8", http.StatusNotFound},
		// セグメント側も同じ入口（resolveRequest）を通る。
		{"segment route non numeric", "/api/sites/default/networks/0/services/abc/live/segments/h264_seg00001.ts", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.want {
				t.Errorf("GET %s status = %d, want %d", tt.path, resp.StatusCode, tt.want)
			}
		})
	}

	if got := state.requestURIList(); len(got) != 0 {
		t.Errorf("mirakc received %d request(s) %v, want none", len(got), got)
	}
}

// 通る側: mirakc が実際に受け取る Request-URI は、パスの SI 値から合成した
// 整数 1 つだけで組み立てられている（issue #217 / #208）。
//
// パスに載るのは SI の (networkId, serviceId) で、Mirakurun 合成 id
// （networkId*100_000 + serviceId）への変換は streamer が mirakc.ServiceID で行う。
// フロントが合成していた形（issue #208）だと、この変換規則が Go と TypeScript に
// 二重化し、URL の id 空間が一覧 API と食い違ったままになる。
func TestLiveStreamer_MirakcPathIsComposedFromPathSegments(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	// 実機の BS（network_id=4, service_id=101）ではなく、network_id が 0 でない
	// ことがはっきり効く値を使う（0 だと合成の有無を区別できない）。
	resp, err := http.Get(playlistURL(srv.URL, 31920, 53248, "h264"))
	if err != nil {
		t.Fatalf("GET playlist: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got := state.requestURIList()
	want := []string{"/api/services/3192053248/stream?decode=1"}
	if !slices.Equal(got, want) {
		t.Errorf("mirakc request URIs = %v, want %v", got, want)
	}
}

// mirakc が要求を拒否した場合（実在しない id・チューナー枯渇など）は 503 に
// まとまる（issue #217）。
//
// **docs/api/media.md が「実在しない id での起動失敗は他の失敗と同じく 503 に
// まとまる」と書いている根拠がこれ。** Rokuban は不明な id を検出しない
// （そのために DB を引かない）ので、mirakc がどのステータスで拒否しても
// writeSessionError の default 節に落ちる --- 分類の細かさは mirakc の応答に
// 依存させない。
func TestLiveStreamer_UpstreamRejectionBecomes503(t *testing.T) {
	// 実在しない id に対する mirakc の応答を模す（404）。**mirakc が実際に
	// 404 を返すことは測っていない**ので、ここで固定しているのは「上流が
	// 拒否したときの Rokuban 側の振る舞い」だけである。
	mirakcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service not found", http.StatusNotFound)
	}))
	t.Cleanup(mirakcSrv.Close)

	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	resp, body := get(t, playlistURL(srv.URL, 31920, 53248, "h264"), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	// 本文は上流のものを漏らさず、他の起動失敗（チューナー枯渇・ffmpeg 起動
	// 失敗）と同じ文言にまとまる（プレーンテキスト。OpenAPI 対象外）。
	if got := strings.TrimSpace(string(body)); got != "live stream unavailable" {
		t.Errorf("body = %q, want %q", got, "live stream unavailable")
	}
}

// site がこのプロセスの束縛サイトと一致しない要求は 404（DB を引かずパスだけで判定する）。
func TestLiveStreamer_SiteMismatch(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	resp, err := http.Get(fmt.Sprintf("%s/api/sites/other-site/networks/0/services/1/live/playlist.m3u8", srv.URL))
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

// URL にセッション ID が現れず、正規表現 1 本で (site, networkId, serviceId) が
// 取り出せること（前段の consistent hash 鍵になるための固定深さ制約、
// issue #56 / #91。鍵が 3 項になったのは issue #217）。
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
		"/api/sites/{site}/networks/{networkId}/services/{serviceId}/live/playlist.m3u8",
		"/api/sites/{site}/networks/{networkId}/services/{serviceId}/live/segments/{name}",
		// 離脱ヒント（issue #191）。セッション ID を持たない = 宛先はプレイリスト /
		// セグメントと同じ (site, networkId, serviceId) のまま、固定深さも保つ。
		"/api/sites/{site}/networks/{networkId}/services/{serviceId}/live/leave",
	}
	slices.Sort(routes)
	slices.Sort(want)
	if !slices.Equal(routes, want) {
		t.Fatalf("mounted live routes = %v, want exactly %v", routes, want)
	}

	// 実際に 200 が返る要求の URL にも、docs/operations.md §5 の nginx map と同じ
	// 正規表現で (site, networkId, serviceId) が取り出せることを確認する
	// （ルートの形だけでなく、実在の URL でも成立する）。
	re := regexp.MustCompile(`^/api/sites/([^/]+)/networks/([^/]+)/services/([^/]+)/live/`)
	baseURL := newLiveTestServerURL(t, r)
	plURL := playlistURL(baseURL, 4, 1024, "h264")
	segURL := firstSegmentURL(t, plURL)

	for _, raw := range []string{plURL, segURL, leaveURL(baseURL, 4, 1024)} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing %q: %v", raw, err)
		}
		m := re.FindStringSubmatch(u.Path)
		if m == nil || m[1] != "default" || m[2] != "4" || m[3] != "1024" {
			t.Errorf("request path %q did not match the fixed-depth regex: %v", u.Path, m)
		}
		// クエリ文字列に鍵（site/networkId/serviceId）を置いていないことも確認する
		// （profile はクエリ側に許すが、鍵はパス側のみ）。
		if u.RawQuery != "" && (strings.Contains(u.RawQuery, "site") ||
			strings.Contains(u.RawQuery, "service") || strings.Contains(u.RawQuery, "network")) {
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

	segURL := firstSegmentURL(t, playlistURL(srv.URL, 0, 42, "h264"))

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

	segURL := firstSegmentURL(t, playlistURL(srv.URL, 0, 7, "h264"))

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

	resp, err := http.Get(playlistURL(srv.URL, 0, 99, "h264"))
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
	resp2, err := http.Get(playlistURL(srv.URL, 0, 100, "h264"))
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
	// **cfg.SegmentDir 自体は消さない（issue #189 の項目 2）。** SegmentDir に
	// k8s emptyDir を直接マウントしている構成では、Linux はマウントポイント
	// 自体への rmdir を EBUSY で拒む（Linux コンテナで実測済み。issue #189
	// コメント参照）。中身だけを掃く実装なら、この構成でも起動時 sweep が
	// SegmentDir 自体を rmdir しようとしないので EBUSY を踏まない。
	if _, err := os.Stat(cfg.SegmentDir); err != nil {
		t.Errorf("cfg.SegmentDir itself must survive the sweep (only its contents should be removed), stat err = %v", err)
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

	const serviceID = 55

	// クライアント A: 最初にプレイリストを取得してセッションを起こすが、その後は
	// 何も要求せず離脱する。ffmpeg の起動（偽物でもプロセス起動コストがある）が
	// idle timeout に対して遅いことがあるため、この 1 回目の取得時間そのものを
	// idle 判定の基準にしない --- 直後に 1 回セグメントを取り直して lastAccess を
	// 「今」に揃えてから idle 判定ループへ入る。
	segURL := firstSegmentURL(t, playlistURL(srv.URL, 0, serviceID, "h264"))
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

// leaveURL は離脱ヒントの宛先（issue #191）。セッション ID を持たず、
// プレイリスト / セグメントと同じ (site, networkId, serviceId) の固定深さ
// （id は SI の値。issue #217）。
func leaveURL(base string, networkID, serviceID int) string {
	return fmt.Sprintf("%s/api/sites/%s/networks/%d/services/%d/live/leave",
		base, testLiveSite, networkID, serviceID)
}

// postLeave は離脱ヒントを 1 回送り、ステータスコードを返す。
func postLeave(t *testing.T, base string, networkID, serviceID int) int {
	t.Helper()
	resp, err := http.Post(leaveURL(base, networkID, serviceID), "", nil)
	if err != nil {
		t.Fatalf("POST leave: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

// 猶予の値そのものを固定する（期待値はリテラル。実装の式と比較すると何も
// 主張しないため）。**「猶予 > セグメント長 + マージン」が issue #191 の罠
// （猶予を短くすると leave が「他人の視聴を切る道具」になる）の唯一の防壁**なので、
// 振る舞いのテストとは別にここで静的に固定する --- 振る舞い側は「他の視聴者が
// touch すれば生き残る」を見るが、それは猶予が 1 秒でも 8 秒でも通ってしまう
// （実クライアントの要求間隔を実時間で待つテストは書けない）。
func TestLiveConfig_LeaveGrace(t *testing.T) {
	profiles := func(segmentSeconds ...int) []LiveProfile {
		var ps []LiveProfile
		for i, s := range segmentSeconds {
			ps = append(ps, LiveProfile{Name: fmt.Sprintf("p%d", i), SegmentSeconds: s})
		}
		return ps
	}
	tests := []struct {
		name        string
		idleTimeout time.Duration
		profiles    []LiveProfile
		want        time.Duration
	}{
		// 既定の設定（config.example.yml: segment_seconds 2 / idle_timeout 30s）
		{"既定", 30 * time.Second, profiles(2), 8 * time.Second},
		// 複数プロファイルなら最長のセグメント長に合わせる（短い方に合わせると、
		// 長いプロファイルを見ている視聴者が切られる）
		{"最長のセグメント長に合わせる", 30 * time.Second, profiles(2, 6), 20 * time.Second},
		// **危険側の組（レビュー指摘）。** 初版は idle_timeout でクリップしていたため、
		// ここが 30s / 2s / 1s になり「猶予 < セグメント長」を満たしてしまっていた。
		// idle_timeout が何であろうと猶予はセグメント長から決まる ---
		// 猶予が idle_timeout 以上になる設定では、詰める先が現在の期限より後ろに
		// なるのでヒントが no-op になる（TestLiveStreamer_LeaveHint_ClippedGraceIsNoOp）。
		{"猶予 > idle_timeout でもクリップしない", 30 * time.Second, profiles(10), 32 * time.Second},
		{"idle_timeout が猶予より短い", 5 * time.Second, profiles(2), 8 * time.Second},
		// `internal/api/api_test.go` に実在する idle_timeout: 2s との組み合わせ。
		// クリップしていた初版ではここが 2s（< セグメント長 6s）になっていた
		{"idle_timeout 2s + segment 6s（実在する設定）", 2 * time.Second, profiles(6), 20 * time.Second},
		// プロファイルが無い設定（ライブは何も配れないので session も起きない）。
		// マージンだけが残る
		{"プロファイル無し", 30 * time.Second, nil, 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := LiveConfig{IdleTimeout: tt.idleTimeout, Profiles: tt.profiles}
			if got := cfg.leaveGrace(); got != tt.want {
				t.Errorf("leaveGrace() = %v, want %v", got, tt.want)
			}
			// 罠そのもの: **猶予はセグメント長より必ず長い。例外を作らない。**
			// 初版はここに「クリップされた場合を除く」という逃げ道を書いていたため、
			// 危険側の組（上記）がすり抜けた --- 逃げ道が「破れていないこと」の根拠を
			// 別の安全装置（hintLeave の clamp）に預けており、この関数単体では
			// 性質が成り立っていなかった。
			longest := 0
			for _, p := range tt.profiles {
				if p.SegmentSeconds > longest {
					longest = p.SegmentSeconds
				}
			}
			segment := time.Duration(longest) * time.Second
			if got := cfg.leaveGrace(); got <= segment {
				t.Errorf("leaveGrace() = %v <= segment length %v: a leave hint could cut another viewer off", got, segment)
			}
		})
	}
}

// gcInterval は「先に来る方の期限」に追随する（min(IdleTimeout, 猶予) / 2）。
//
// クリップを外した（レビュー指摘）ことで猶予が IdleTimeout を超えうるように
// なったため、**猶予だけを見ていると通常の idle GC の刻みまで粗くなる**
// （ヒントと無関係な回収が遅れる）。両方向をリテラルの期待値で固定する。
func TestLiveStreamer_GCIntervalTakesTheEarlierDeadline(t *testing.T) {
	tests := []struct {
		name        string
		idleTimeout time.Duration
		segment     int
		want        time.Duration
	}{
		// 既定: 猶予 8 秒の方が先に来るので 4 秒（idle_timeout/2 = 15 秒ではない）
		{"既定は猶予に追随する", 30 * time.Second, 2, 4 * time.Second},
		// 猶予 32 秒 > idle_timeout 30 秒: idle_timeout 側に追随（16 秒にしない）
		{"猶予が長い設定では idle_timeout に追随する", 30 * time.Second, 10, 15 * time.Second},
		// 下限 1 秒（busy loop 防止）
		{"下限は 1 秒", 500 * time.Millisecond, 2, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ls := &LiveStreamer{cfg: LiveConfig{
				IdleTimeout: tt.idleTimeout,
				Profiles:    []LiveProfile{{Name: "h264", SegmentSeconds: tt.segment}},
			}}
			if got := ls.gcInterval(); got != tt.want {
				t.Errorf("gcInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}

// 猶予が idle_timeout 以上になる設定（`segment_seconds: 6` + `idle_timeout: 2s`
// 等）では、**ヒントは期限を一切動かさない**（no-op）。
//
// レビュー指摘の危険側の組をそのまま踏む。ここが no-op でなければ、A が
// 見ている最中に B の leave 1 回で A のセッションが消える --- issue #191 の罠
// 「猶予をセグメント長より短くすると leave が他人の視聴を切る道具になる」の
// 実体である。**「切れない」ことを、切れるはずの間隔（A の要求間隔 = セグメント長）
// より長い時間、実時間で観測する。**
// 起動完了待ち（`<-s.ready`）の区間でも last-access が進むこと。
//
// Playlist のプレイリスト待ちは
// `TestLiveStreamer_LeaveHint_DoesNotKillASessionThatIsStillStartingUp` が
// 実経路で見ているが、**ready 待ちの区間**（getOrCreateSession の 2 経路と
// Segment。mirakc への接続が遅いと最大 playlistStartupTimeout 続く）は
// 偽 mirakc の応答を遅らせないと踏めないので、ここでは helper を直接見る ---
// 3 経路とも同じ helper を通しているので、これでその 3 つを覆える。
func TestWaitReadyTouching_KeepsTheSessionFresh(t *testing.T) {
	s := &liveSession{
		ready:      make(chan struct{}),
		lastAccess: time.Now().Add(-time.Hour), // 十分に古い
	}

	done := make(chan error, 1)
	go func() { done <- waitReadyTouching(context.Background(), s, 5*time.Second) }()

	// ポーリング 2 周ぶん待てば、待っている側が touch しているはず。
	time.Sleep(5 * playlistPollInterval)
	if idle := s.idleSince(time.Now()); idle > time.Second {
		t.Errorf("idleSince while waiting for readiness = %v, want < 1s (a client waiting for startup must count as activity)", idle)
	}

	close(s.ready)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("waitReadyTouching() = %v, want nil once ready is closed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitReadyTouching did not return after ready was closed")
	}
}

// 逆方向: 起動が終わらないまま timeout すれば errStartupTimeout を返す
// （待ちながら touch する形にしても、#189 / #286 が入れた期限は生きている）。
func TestWaitReadyTouching_TimesOut(t *testing.T) {
	s := &liveSession{ready: make(chan struct{}), lastAccess: time.Now()}
	start := time.Now()
	err := waitReadyTouching(context.Background(), s, 3*playlistPollInterval)
	if !errors.Is(err, errStartupTimeout) {
		t.Errorf("waitReadyTouching() = %v, want errStartupTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waitReadyTouching took %v, want it to give up at the timeout", elapsed)
	}
}

// installSlowStartFakeLiveFFmpeg は installFakeLiveFFmpeg と同じものを書き出すが、
// **書き出しを delay だけ遅らせる**（実 ffmpeg のトランスコード立ち上がりが遅く、
// プレイリストの 1 本目が出るまで時間がかかる状況）。
//
// `cmd.Start()` 自体は即座に返るので、**セッションは「起動済み（ready）」だが
// プレイリストはまだ無い**という状態が delay のあいだ続く --- ここが
// issue #191 のレビューで指摘された無音区間である。
func installSlowStartFakeLiveFFmpeg(t *testing.T, delay time.Duration) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg script assumes a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "slow-fake-ffmpeg-live")
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null &
sleep %.2f
baseurl=""
segfile=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-hls_segment_filename" ]; then segfile="$a"; fi
  if [ "$prev" = "-hls_base_url" ]; then baseurl="$a"; fi
  case "$a" in
    *.m3u8)
      playlist="$a"
      mkdir -p "$(dirname "$playlist")" 2>/dev/null
      seg=$(printf '%%s' "$segfile" | sed 's/%%05d/00001/')
      mkdir -p "$(dirname "$seg")" 2>/dev/null
      printf 'fake-ts-segment-data' > "$seg.tmp"
      mv "$seg.tmp" "$seg"
      {
        printf '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:2.0,\n'
        printf '%%s\n' "${baseurl}$(basename "$seg")"
      } > "$playlist.tmp"
      mv "$playlist.tmp" "$playlist"
      ;;
  esac
  prev="$a"
done
while true; do sleep 1; done
`, delay.Seconds())
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// **離脱ヒントは「起動を待っている視聴者」のセッションを殺してはならない**
// （issue #191 のレビュー指摘。この PR が新設した経路）。
//
// Playlist は `getOrCreateSession` の後に touch() を 1 回打ち、そこから
// waitForPlaylist（最大 playlistStartupTimeout）に入る。この区間で誰も touch
// しないと、そこに届いたヒント 1 発で idle 期限が猶予まで詰まり、**待っている
// 視聴者ごと**回収されてしまう。回収前は期限 30 秒 > 起動待ち 15 秒だったので
// この経路は存在せず、猶予を 8 秒に詰められるようにしたことで生まれた。
//
// **仮想時計を使わず、実時間の GC ループ（ls.Run）で見る。** reapIdleAt に
// 「いま」を渡す形だと、無音区間と GC 周期の重なりという**この不具合の本体**が
// テストから消える（レビュアーの再現条件に合わせる）。
//
// 修正前の実測: ヒント送出 → 約 2 秒後に回収 → 視聴者は playlistStartupTimeout
// 満了で 504。修正後: 200（起動待ちのあいだ last-access が更新され続ける）。
func TestLiveStreamer_LeaveHint_DoesNotKillASessionThatIsStillStartingUp(t *testing.T) {
	// 起動待ち（3.5s）> 猶予（2s）+ GC 周期（1s）になるように選ぶ。
	// **`segment_seconds: 0` は production では起こらない**（config が既定 2 を
	// 埋める）が、ここで固定したいのは「無音区間 > 猶予 + 周期」という**形**で
	// あって production の秒数ではない --- 実際の既定値（猶予 8 秒）で同じ形を
	// 作ると、起動待ちを 10 秒以上にする必要があり、テストが不必要に遅くなる。
	const startupDelay = 3500 * time.Millisecond
	prevTimeout := playlistStartupTimeout
	playlistStartupTimeout = 6 * time.Second
	t.Cleanup(func() { playlistStartupTimeout = prevTimeout })

	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.FFmpeg = installSlowStartFakeLiveFFmpeg(t, startupDelay)
	cfg.IdleTimeout = 8 * time.Second // 起動待ちより長い = ヒントが無ければ死なない
	cfg.Profiles[0].SegmentSeconds = 0
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)
	grace := cfg.leaveGrace()
	if grace+ls.gcInterval() >= startupDelay {
		t.Fatalf("test setup: grace (%v) + gc interval (%v) must be shorter than the startup delay (%v) to exercise the window",
			grace, ls.gcInterval(), startupDelay)
	}

	// 実時間の GC ループを回す（この不具合は周期と無音区間の重なりで出る）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ls.Run(ctx) }()

	// networkId 0 = mirakc の合成 id が serviceId と一致する（偽 mirakc が
	// 返す disconnected の id と直接比較できる）。
	const serviceID = 93
	type result struct {
		status int
		took   time.Duration
	}
	got := make(chan result, 1)
	go func() {
		start := time.Now()
		resp, err := http.Get(playlistURL(srv.URL, 0, serviceID, "h264"))
		if err != nil {
			got <- result{0, time.Since(start)}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)
		got <- result{resp.StatusCode, time.Since(start)}
	}()

	// セッションが起きるのを待つ（ここを待たずにヒントを送ると "no_session" に
	// なり、何も検証していないテストになる）。
	waitDeadline := time.Now().Add(2 * time.Second)
	for ls.sessionCount() == 0 && time.Now().Before(waitDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ls.sessionCount() != 1 {
		t.Fatalf("session did not start; sessionCount = %d", ls.sessionCount())
	}
	// **Playlist が touch() を打った後**にヒントを送る（それより前だと直後の
	// touch() がヒントを打ち消してしまい、この窓を踏まない）。
	time.Sleep(700 * time.Millisecond)
	if code := postLeave(t, srv.URL, 0, serviceID); code != http.StatusNoContent {
		t.Fatalf("POST leave = %d, want 204", code)
	}

	select {
	case r := <-got:
		if r.status != http.StatusOK {
			t.Fatalf("viewer waiting for the playlist got %d after %v, want 200: a leave hint must not reclaim a session that a client is still waiting on",
				r.status, r.took)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("playlist request never returned")
	}
	select {
	case sid := <-state.disconnected:
		t.Fatalf("service %d was disconnected while a client was still waiting for its playlist", sid)
	default:
	}

	// 逆方向: 起動待ちが終われば、ヒントは普通に効く（待ちの touch が
	// セッションを永久に免疫にしてしまっていない）。
	if code := postLeave(t, srv.URL, 0, serviceID); code != http.StatusNoContent {
		t.Fatalf("POST leave (after startup) = %d, want 204", code)
	}
	select {
	case sid := <-state.disconnected:
		if sid != serviceID {
			t.Errorf("disconnected service id = %d, want %d", sid, serviceID)
		}
	case <-time.After(grace + 4*time.Second):
		t.Fatalf("session was not reclaimed within %v of a leave hint sent after startup finished (the startup touching must not immunize the session forever)",
			grace+4*time.Second)
	}
}

func TestLiveStreamer_LeaveHint_ClippedGraceIsNoOp(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.IdleTimeout = 2 * time.Second
	cfg.Profiles[0].SegmentSeconds = 6 // 猶予 20 秒 >> idle_timeout 2 秒
	if cfg.leaveGrace() <= cfg.IdleTimeout {
		t.Fatalf("test setup: leaveGrace (%v) must exceed idle timeout (%v) to exercise the no-op path",
			cfg.leaveGrace(), cfg.IdleTimeout)
	}
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	const serviceID = 66
	segURL := firstSegmentURL(t, playlistURL(srv.URL, 0, serviceID, "h264"))
	if got := ls.sessionCount(); got != 1 {
		t.Fatalf("sessionCount before the hint = %d, want 1", got)
	}

	// 視聴者 A はセグメントを取り続けている（idle_timeout より短い間隔）。
	stopA := make(chan struct{})
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		for {
			select {
			case <-stopA:
				return
			default:
			}
			if resp, err := http.Get(segURL); err == nil {
				_ = resp.Body.Close()
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		select {
		case <-stopA:
		default:
			close(stopA)
		}
		<-aDone
	})

	// B が離脱ヒントを投げる。A は見続けている。
	shortenedBefore := counterValue(t, metrics.LiveLeaveHints.WithLabelValues("deadline_shortened"))
	noEffectBefore := counterValue(t, metrics.LiveLeaveHints.WithLabelValues("no_effect"))
	if code := postLeave(t, srv.URL, 0, serviceID); code != http.StatusNoContent {
		t.Fatalf("POST leave = %d, want 204", code)
	}
	// **効かなかったヒントを「詰めた」と数えない**（メトリクスは実際に起きたことを
	// 表す。issue #191 のレビュー指摘の任意 1 件）。
	if got := counterValue(t, metrics.LiveLeaveHints.WithLabelValues("no_effect")); got != noEffectBefore+1 {
		t.Errorf("rokuban_live_leave_hints_total{result=no_effect} = %v, want %v", got, noEffectBefore+1)
	}
	if got := counterValue(t, metrics.LiveLeaveHints.WithLabelValues("deadline_shortened")); got != shortenedBefore {
		t.Errorf("rokuban_live_leave_hints_total{result=deadline_shortened} = %v, want %v (a hint that changed nothing must not be counted as shortened)", got, shortenedBefore)
	}

	// A の要求間隔（0.3 秒）よりも、セグメント長（6 秒）よりも、idle_timeout
	// （2 秒）よりも長く回す。ヒントが期限を動かしていたら必ずここで死ぬ。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ls.reapIdle()
		select {
		case sid := <-state.disconnected:
			t.Fatalf("service %d was reclaimed while viewer A was still watching: a leave hint must never shorten the deadline below what the config already implies", sid)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}

	resp, err := http.Get(segURL)
	if err != nil {
		t.Fatalf("GET segment (viewer A, final): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("viewer A segment status = %d, want 200", resp.StatusCode)
	}

	// 逆方向: A も離れれば（この設定でも）回収される --- 上のループが
	// 「そもそも回収され得ない」だけで通っていないことの確認。
	close(stopA)
	<-aDone
	time.Sleep(cfg.IdleTimeout + 200*time.Millisecond)
	ls.reapIdle()
	select {
	case sid := <-state.disconnected:
		if sid != serviceID {
			t.Errorf("disconnected service id = %d, want %d", sid, serviceID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session was not reclaimed after the viewer left (the oracle above cannot actually kill a session)")
	}
}

// ヒントは idle 期限を**縮める方向にしか動かない**。
//
// 猶予が idle_timeout でクリップされる設定（segment_seconds が長い）では
// 「いま + 猶予」が現在の期限と同じかそれより後になるので、無条件に代入する実装だと
// **ヒントが延命の道具になる**（「離れた」と言うだけでチューナーを掴み続けられる）。
// 巻き戻しだけを許すことで、最悪ケースが「何も起こらない」になる。
func TestLiveSession_HintLeaveNeverExtendsTheDeadline(t *testing.T) {
	now := time.Now()
	// 9 秒前から誰も要求していないセッション（あと 1 秒で idle GC の対象になる）。
	s := &liveSession{lastAccess: now.Add(-9 * time.Second)}

	// 猶予 = idle_timeout（leaveGrace がクリップした状態）。
	s.hintLeave(now, 10*time.Second, 10*time.Second)

	if got := s.idleSince(now); got != 9*time.Second {
		t.Errorf("idleSince after a no-op hint = %v, want 9s (a leave hint must never push the deadline forward)", got)
	}
}

// 離脱ヒントを送ると、他に視聴者がいないサービスのセッションが idle_timeout を
// 待たずに回収される（受け入れ基準 2、issue #191）。
//
// **同じ瞬間に、ヒントを受けていないセッションが生き残ることを対で見る。**
// 片方だけだと「時間が経ったから回収された」と区別できない --- 回収の原因が
// ヒントであることは、同じ reapIdleAt の 1 パスで運命が分かれることでしか示せない。
//
// 実時間を待たずに済むよう、GC には仮想の「いま」を渡す（reapIdleAt）。
func TestLiveStreamer_LeaveHint_ShortensIdleDeadline(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	const leavingService = 55
	const stayingService = 56
	grace := cfg.leaveGrace()
	if grace >= cfg.IdleTimeout {
		t.Fatalf("test setup: leaveGrace (%v) must be shorter than idle timeout (%v)", grace, cfg.IdleTimeout)
	}

	// セッションが実際に起きるまで待つ（firstSegmentURL はプレイリストが
	// 配れなければ Fatal する）。「何も起きない」で通るのを防ぐため、
	// ヒントを送る前にセッション数も確認する。
	leavingSeg := firstSegmentURL(t, playlistURL(srv.URL, 0, leavingService, "h264"))
	stayingSeg := firstSegmentURL(t, playlistURL(srv.URL, 0, stayingService, "h264"))
	if got := ls.sessionCount(); got != 2 {
		t.Fatalf("sessionCount before the hint = %d, want 2", got)
	}

	// 両方の lastAccess を「いま」に揃えてから片方にだけヒントを送る
	// （偽 ffmpeg の起動コストぶんの差を判定に持ち込まない）。
	for _, u := range []string{leavingSeg, stayingSeg} {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatalf("GET segment: %v", err)
		}
		_ = resp.Body.Close()
	}
	if code := postLeave(t, srv.URL, 0, leavingService); code != http.StatusNoContent {
		t.Fatalf("POST leave = %d, want 204", code)
	}
	now := time.Now()

	// 猶予を過ぎた瞬間: ヒントを受けた方だけが回収される。
	ls.reapIdleAt(now.Add(grace + 10*time.Millisecond))

	select {
	case sid := <-state.disconnected:
		if sid != leavingService {
			t.Fatalf("disconnected service id = %d, want %d", sid, leavingService)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the session that sent a leave hint was not reclaimed %v after it (idle_timeout is %v, so this must not need the full timeout)",
			grace, cfg.IdleTimeout)
	}

	// ヒントを送っていない方は同じパスで生き残る（= 回収の原因は経過時間ではなく
	// ヒントである）。
	resp, err := http.Get(stayingSeg)
	if err != nil {
		t.Fatalf("GET segment (staying service): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("staying service segment status = %d, want 200 (only the service that sent a hint may be reclaimed)", resp.StatusCode)
	}

	if got := counterValue(t, metrics.LiveLeaveHints.WithLabelValues("deadline_shortened")); got < 1 {
		t.Errorf("rokuban_live_leave_hints_total{result=deadline_shortened} = %v, want >= 1", got)
	}
}

// ヒントは**停止命令ではない**（issue #191 の罠）。受けた直後もセッションは
// 生きていて、配信も続く --- 変わるのは idle 期限だけ。
func TestLiveStreamer_LeaveHint_DoesNotStopTheSession(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	// **networkId を 0 以外にする唯一のテスト。** ヒントの宛先はプレイリスト /
	// セグメントと同じ `(site, networkId, serviceId)` で、mirakc 合成 id への変換も
	// 同じ resolveRequest を通る（issue #217）。networkId が宛先の一部として
	// 効いていることを、下の「別 networkId のヒントは届かない」で確かめる。
	const networkID, serviceID = 4, 77
	segURL := firstSegmentURL(t, playlistURL(srv.URL, networkID, serviceID, "h264"))
	if got := ls.sessionCount(); got != 1 {
		t.Fatalf("sessionCount before the hint = %d, want 1", got)
	}

	// 同じ serviceId でも networkId が違えば別サービスなので、このセッションには
	// 届かない（`no_session` として 204 を返すだけ）。届いてしまうと、隣の
	// ネットワークの視聴者がこのセッションの期限を詰められることになる。
	noSessionBefore := counterValue(t, metrics.LiveLeaveHints.WithLabelValues("no_session"))
	if code := postLeave(t, srv.URL, networkID+1, serviceID); code != http.StatusNoContent {
		t.Fatalf("POST leave (other network) = %d, want 204", code)
	}
	if got := counterValue(t, metrics.LiveLeaveHints.WithLabelValues("no_session")); got != noSessionBefore+1 {
		t.Errorf("rokuban_live_leave_hints_total{result=no_session} = %v, want %v (a hint addressed to another networkId must not reach this session)",
			got, noSessionBefore+1)
	}

	if code := postLeave(t, srv.URL, networkID, serviceID); code != http.StatusNoContent {
		t.Fatalf("POST leave = %d, want 204", code)
	}
	now := time.Now()

	// mirakc への接続が切れていない（チューナーを即座に手放していない）。
	select {
	case sid := <-state.disconnected:
		t.Fatalf("service %d was disconnected by the leave hint itself; the hint must only shorten the idle deadline", sid)
	case <-time.After(100 * time.Millisecond):
	}
	if got := ls.sessionCount(); got != 1 {
		t.Errorf("sessionCount right after the hint = %d, want 1 (the hint is not a stop command)", got)
	}
	// 猶予の内側では GC も回収しない。
	ls.reapIdleAt(now.Add(cfg.leaveGrace() / 2))
	if got := ls.sessionCount(); got != 1 {
		t.Errorf("sessionCount within the grace = %d, want 1", got)
	}

	resp, err := http.Get(segURL)
	if err != nil {
		t.Fatalf("GET segment after the hint: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("segment status right after the hint = %d, want 200 (playback must continue during the grace)", resp.StatusCode)
	}
}

// 罠そのもの: **leave を連打しても、実際に見ている人の要求が期限を戻す**ので
// 他人の視聴を切れない（受け入れ基準 3、issue #191）。
//
// 判定の瞬間は `TestLiveStreamer_LeaveHint_ShortensIdleDeadline` と**同じ**
// 「ヒント + 猶予」の直後に取る --- そこで回収されるかどうかだけが両テストの差で、
// 差を作っているのは「他の視聴者の要求が来たか」だけである。
//
// 最後に「B も離れたら回収される」を同じ道具で確かめる（この判定手段が本当に
// セッションを殺せることの確認 --- 「何も起きない」系のテストが空虚に成功して
// いないことの証明）。
func TestLiveStreamer_LeaveHint_OtherViewerKeepsSessionAlive(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	const serviceID = 88
	grace := cfg.leaveGrace()
	segURL := firstSegmentURL(t, playlistURL(srv.URL, 0, serviceID, "h264"))
	if got := ls.sessionCount(); got != 1 {
		t.Fatalf("sessionCount before the hint = %d, want 1", got)
	}

	// 悪意のあるクライアント A が leave を連打する。そのたびに視聴者 B の
	// セグメント要求が続く（実際に見ている人がいる）。
	for i := range 5 {
		if code := postLeave(t, srv.URL, 0, serviceID); code != http.StatusNoContent {
			t.Fatalf("POST leave #%d = %d, want 204", i, code)
		}
		hintedAt := time.Now()

		// B の要求（ヒントの後に届く = 期限を戻す）。
		resp, err := http.Get(segURL)
		if err != nil {
			t.Fatalf("GET segment (viewer B, round %d): %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("viewer B segment status (round %d) = %d, want 200", i, resp.StatusCode)
		}

		// ヒントが詰めたはずの期限を跨いで GC を回す。B が見ているので回収されない。
		ls.reapIdleAt(hintedAt.Add(grace + 10*time.Millisecond))
		if got := ls.sessionCount(); got != 1 {
			t.Fatalf("sessionCount after leave spam #%d = %d, want 1 (a leave hint must not cut off a viewer who is still requesting segments)", i, got)
		}
		select {
		case sid := <-state.disconnected:
			t.Fatalf("service %d was disconnected by leave spam while viewer B was still watching", sid)
		default:
		}
	}

	if resp, err := http.Get(segURL); err != nil {
		t.Fatalf("GET segment (viewer B, final): %v", err)
	} else {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("viewer B segment status = %d, want 200 (B must still be watching)", resp.StatusCode)
		}
	}

	// 逆方向: B も離れれば（もう誰も要求しない）同じ道具で回収される。
	// これが無いと、上のループは「そもそも回収され得ない」だけで通りうる。
	if code := postLeave(t, srv.URL, 0, serviceID); code != http.StatusNoContent {
		t.Fatalf("POST leave (final) = %d, want 204", code)
	}
	ls.reapIdleAt(time.Now().Add(grace + 10*time.Millisecond))
	select {
	case sid := <-state.disconnected:
		if sid != serviceID {
			t.Errorf("disconnected service id = %d, want %d", sid, serviceID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session was not reclaimed after every viewer left (the oracle above cannot actually kill a session)")
	}
}

// ヒントはセッションを**作らない**（未開始・回収済みのどちらでも 204 のまま、
// mirakc には触らない）。宛先が (site, serviceId) である以上、この口は
// 「そのサービスを見たい」という要求と同じ形をしているので、うっかり
// getOrCreateSession を呼ぶ実装にすると**離脱がチューナーを掴む**。
func TestLiveStreamer_LeaveHint_DoesNotStartASession(t *testing.T) {
	mirakcSrv, state := newFakeMirakcLiveServer(t)
	ls, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	before := counterValue(t, metrics.LiveLeaveHints.WithLabelValues("no_session"))
	if code := postLeave(t, srv.URL, 0, 4649); code != http.StatusNoContent {
		t.Errorf("POST leave (no session) = %d, want 204", code)
	}
	if got := state.requestCount(); got != 0 {
		t.Errorf("mirakc stream requests = %d, want 0 (a leave hint must never start a session)", got)
	}
	if got := ls.sessionCount(); got != 0 {
		t.Errorf("sessionCount = %d, want 0", got)
	}
	if got := counterValue(t, metrics.LiveLeaveHints.WithLabelValues("no_session")); got != before+1 {
		t.Errorf("rokuban_live_leave_hints_total{result=no_session} = %v, want %v", got, before+1)
	}
}

// site が一致しないヒントは 404（プレイリスト / セグメントと同じ判定。DB は引かない）。
func TestLiveStreamer_LeaveHint_SiteMismatch(t *testing.T) {
	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, baseLiveConfig(t))

	resp, err := http.Post(fmt.Sprintf("%s/api/sites/other-site/networks/0/services/1/live/leave", srv.URL), "", nil)
	if err != nil {
		t.Fatalf("POST leave: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	// 対照: 同じ URL 形で site だけを正しくすると 404 にならない。これが無いと
	// 「ルート自体が存在しない」（= URL の綴り間違い）でも 404 になり、site 判定を
	// 無効化しても落ちないテストになる（レビューで実際にその状態だった）。
	ok, err := http.Post(fmt.Sprintf("%s/api/sites/%s/networks/0/services/1/live/leave", srv.URL, testLiveSite), "", nil)
	if err != nil {
		t.Fatalf("POST leave (matching site): %v", err)
	}
	defer func() { _ = ok.Body.Close() }()
	if ok.StatusCode == http.StatusNotFound {
		t.Errorf("matching site status = 404; the route must exist, otherwise the 404 above proves nothing")
	}
}

func TestBuildLiveFFmpegArgs(t *testing.T) {
	profiles := []LiveProfile{
		{Name: "h264", VideoCodec: "libx264", AudioCodec: "aac", Height: 720, Preset: "veryfast", SegmentSeconds: 2, PlaylistSize: 6, ExtraArgs: []string{"-b:v", "2M"}},
		{Name: "h264low", VideoCodec: "libx264", AudioCodec: "aac", Height: 360, SegmentSeconds: 4, PlaylistSize: 3},
	}
	args := BuildLiveFFmpegArgs(LiveConfig{Profiles: profiles}, "/tmp/live/1", false)

	if !slices.Contains(args, "-i") {
		t.Fatal("missing -i")
	}
	if got := args[slices.Index(args, "-i")+1]; got != "pipe:0" {
		t.Errorf("input = %q, want pipe:0 (streamer fetches from mirakc itself)", got)
	}
	// ARIB caption を map すると Debian ffmpeg が exit 1 する（実 mirakc で観測）。
	// 映像・音声だけを明示 map する契約を固定する。
	// -map は output 単位なので、プロファイル数ぶんの組が必要（PR #210 レビュー）。
	// ループの前に 1 組だけだと 2 本目以降は自動選択に戻り arib_caption で落ちる。
	mapV := 0
	mapA := 0
	for i := 0; i < len(args)-1; i++ {
		if args[i] != "-map" {
			continue
		}
		switch args[i+1] {
		case "0:v:0":
			mapV++
		case "0:a:0":
			mapA++
		case "0:s:0", "0:d:0":
			t.Errorf("must not map subtitle/data streams: %v", args)
		}
	}
	if mapV != len(profiles) || mapA != len(profiles) {
		t.Errorf("-map 0:v:0 / 0:a:0 counts = %d/%d, want %d each (per output): %v",
			mapV, mapA, len(profiles), args)
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

func TestBuildLiveFFmpegArgs_CaptionsUsesMasterAndWebVTT(t *testing.T) {
	args := BuildLiveFFmpegArgs(LiveConfig{
		Captions: true,
		Profiles: []LiveProfile{
			{Name: "h264", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
			{Name: "low", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 4, PlaylistSize: 3},
		},
	}, "/tmp/live/1", true)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-map 0:s:0?", "-c:s webvtt", "-var_stream_map v:0,a:0,s:0,sgroup:subs v:1,a:1",
		"-master_pl_name playlist.m3u8", "-hls_time 2", "-hls_list_size 6", "-force_key_frames:v:0",
		"-hls_subtitle_path /tmp/live/1/subtitles_%v.m3u8", "playlist_%v.m3u8", "/tmp/live/1/segments/%v_seg%05d.ts",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("caption live args missing %q: %v", want, args)
		}
	}
}

// TestBuildLiveFFmpegArgs_CaptionsPerStreamSpecifiersAreTyped は captions 経路の
// per-stream 指定子（フィルタ・preset）が型付き（`:v:N`）で、2 本目以降の
// プロファイルにも正しく別々に当たることを固定する。
//
// captions 経路の出力ストリーム順は v0, a0, s0, v1, a1。型無しの `-vf:1` /
// `-preset:1` は variant 1 の映像ではなく variant 0 の音声（出力ストリーム
// index 1）を指してしまう（レビュー指摘、実 ffmpeg 9.0.1 で測定して固定）。
//
// **フィルタは `-filter:v:N`（`-vf` の完全形）を使う。** `-vf:v:N`（`-vf` に
// 型付き specifier を重ねる書き方）は同じ実測で機能しないことを確認済み
// （specifier が分離されず、最後に指定したフィルタが両方の video ストリームに
// 適用されて ffmpeg が警告を出す）。
//
// 壊し方: `-filter:v:`+strconv.Itoa(i) を `-vf:`+strconv.Itoa(i) に、
// `-preset:v:`+strconv.Itoa(i) を `-preset:`+strconv.Itoa(i) に戻すと落ちる。
func TestBuildLiveFFmpegArgs_CaptionsPerStreamSpecifiersAreTyped(t *testing.T) {
	args := BuildLiveFFmpegArgs(LiveConfig{
		Captions: true,
		Profiles: []LiveProfile{
			{Name: "h264", VideoCodec: "libx264", AudioCodec: "aac", Height: 720, Preset: "veryfast", SegmentSeconds: 2, PlaylistSize: 6},
			{Name: "low", VideoCodec: "libx264", AudioCodec: "aac", Height: 360, Preset: "faster", SegmentSeconds: 2, PlaylistSize: 6},
		},
	}, "/tmp/live/1", false)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-filter:v:0 scale=-2:720", "-filter:v:1 scale=-2:360",
		"-preset:v:0 veryfast", "-preset:v:1 faster",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q: %v", want, args)
		}
	}
	for _, mustNotContain := range []string{
		"-vf:0", "-vf:1", "-vf:v:0", "-vf:v:1", "-preset:0", "-preset:1",
	} {
		if strings.Contains(joined, mustNotContain) {
			t.Errorf("must not contain untyped/broken specifier %q: %v", mustNotContain, args)
		}
	}
}

func TestBuildLiveFFmpegArgs_CaptionsWithoutSubtitleStream(t *testing.T) {
	args := BuildLiveFFmpegArgs(LiveConfig{
		Captions: true,
		Profiles: []LiveProfile{{Name: "h264", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 3, PlaylistSize: 7}},
	}, "/tmp/live/1", false)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "0:s:0") || strings.Contains(joined, "s:0,sgroup:subs") || strings.Contains(joined, "-c:s webvtt") {
		t.Fatalf("subtitle mapping must be omitted when input has no subtitle stream: %v", args)
	}
	for _, want := range []string{"-var_stream_map v:0,a:0", "-master_pl_name playlist.m3u8", "-hls_time 3", "-hls_list_size 7"} {
		if !strings.Contains(joined, want) {
			t.Errorf("captionless live args missing %q: %v", want, args)
		}
	}
}

// TestReadLiveCaptionPrefix_PreservesAllBytesInOrder は B の置き換え
// （liveReplayReader + goroutine 2 本 → readLiveCaptionPrefix の同期読み取り +
// io.MultiReader）が入力を 1 バイトも落とさず順序どおり ffmpeg 側へ渡すことを
// 固定する。liveCaptionProbeBytes より短い入力・長い入力の両方を見る。
//
// 単純な繰り返しパターン（例: 0x47 だけ）だとオフバイ N のずれや部分的な
// 重複を見逃すので、位置に依存する非周期パターン（`byte(i % 251)`。251 は
// liveCaptionProbeBytes と互いに素な素数）を使う --- ずれが 1 バイトでもあれば
// bytes.Equal が確実に検出する。
//
// 壊し方: readLiveCaptionPrefix 内で `prefix = prefix[:n]` を消す（読めなかった
// 分のゼロ埋めが混入する）、または `io.MultiReader(bytes.NewReader(prefix), body)`
// の引数順を `body, bytes.NewReader(prefix)` に入れ替える（prefix が後ろに
// 回り、読み取り順が入れ替わる）。
func TestReadLiveCaptionPrefix_PreservesAllBytesInOrder(t *testing.T) {
	for _, tt := range []struct {
		name  string
		total int
	}{
		{"shorter than probe prefix (upstream ends early)", liveCaptionProbeBytes / 2},
		{"longer than probe prefix (body continues after prefix)", liveCaptionProbeBytes*2 + 12345},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := make([]byte, tt.total)
			for i := range want {
				want[i] = byte(i % 251)
			}
			body := io.NopCloser(bytes.NewReader(want))

			input, prefix, err := readLiveCaptionPrefix(body)
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
				t.Fatalf("readLiveCaptionPrefix: %v", err)
			}
			wantPrefixLen := min(tt.total, liveCaptionProbeBytes)
			if len(prefix) != wantPrefixLen {
				t.Errorf("prefix length = %d, want %d", len(prefix), wantPrefixLen)
			}

			got, err := io.ReadAll(input)
			if err != nil {
				t.Fatalf("reading combined input: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("combined input length=%d, want length=%d (content differs: bytes dropped, duplicated, or reordered across the prefix/MultiReader hand-off)",
					len(got), len(want))
			}
		})
	}
}

func TestWaitForPlaylist_MasterUsesStreamInfMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "playlist.m3u8")
	content := []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nplaylist_0.m3u8\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("writing master playlist: %v", err)
	}
	s := &liveSession{lastAccess: time.Now()}
	got, ok := waitForPlaylist(context.Background(), s, path, time.Second, "#EXT-X-STREAM-INF")
	if !ok || !bytes.Equal(got, content) {
		t.Fatalf("waitForPlaylist = (%q, %v), want master playlist content", got, ok)
	}
}

// TestBuildLiveFFmpegArgs_HWAccelBeforeInput は live.hwaccel が
// `-f mpegts -i pipe:0` より前に出ることを固定する。
// 壊し方: 前置ブロックを入力の後ろへ移す。
func TestBuildLiveFFmpegArgs_HWAccelBeforeInput(t *testing.T) {
	cfg := LiveConfig{
		HWAccel: &ffargs.HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128", OutputFormat: "vaapi"},
		Profiles: []LiveProfile{
			{Name: "h264", VideoCodec: "h264_vaapi", AudioCodec: "aac", Height: 720, Scaler: ffargs.ScalerVAAPI, SegmentSeconds: 2, PlaylistSize: 6},
		},
	}
	args := BuildLiveFFmpegArgs(cfg, "/tmp/live/1", false)

	hwIdx := slices.Index(args, "-hwaccel")
	deviceIdx := slices.Index(args, "-hwaccel_device")
	outputFormatIdx := slices.Index(args, "-hwaccel_output_format")
	iIdx := slices.Index(args, "-i")
	fMpegtsIdx := -1
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-f" && args[i+1] == "mpegts" {
			fMpegtsIdx = i
			break
		}
	}
	if hwIdx < 0 || iIdx < 0 || fMpegtsIdx < 0 {
		t.Fatalf("missing -hwaccel/-i/-f mpegts: %v", args)
	}
	if hwIdx >= fMpegtsIdx || fMpegtsIdx >= iIdx {
		t.Errorf("expected -hwaccel < -f mpegts < -i, got hw=%d f_mpegts=%d i=%d: %v", hwIdx, fMpegtsIdx, iIdx, args)
	}
	if deviceIdx < 0 || args[deviceIdx+1] != "/dev/dri/renderD128" {
		t.Errorf("missing -hwaccel_device value: %v", args)
	}
	if outputFormatIdx < 0 || args[outputFormatIdx+1] != "vaapi" {
		t.Errorf("missing -hwaccel_output_format value: %v", args)
	}
	if got := args[hwIdx+1]; got != "vaapi" {
		t.Errorf("-hwaccel value = %q, want vaapi", got)
	}
}

// TestBuildLiveFFmpegArgs_PerProfileScalerAndQuality は各プロファイルの
// scaler/品質が互いに漏れないことを固定する（2 本: vaapi+qp / software+crf）。
// 壊し方: scale の append をループの外へ出す。
func TestBuildLiveFFmpegArgs_PerProfileScalerAndQuality(t *testing.T) {
	qp := 26
	crf := 23
	cfg := LiveConfig{
		Profiles: []LiveProfile{
			{Name: "hw", VideoCodec: "h264_vaapi", AudioCodec: "aac", Height: 720, Scaler: ffargs.ScalerVAAPI, QP: &qp, SegmentSeconds: 2, PlaylistSize: 6},
			{Name: "sw", VideoCodec: "libx264", AudioCodec: "aac", Height: 360, Scaler: ffargs.ScalerSoftware, CRF: &crf, SegmentSeconds: 2, PlaylistSize: 6},
		},
	}
	args := BuildLiveFFmpegArgs(cfg, "/tmp/live/1", false)

	if !slices.Contains(args, "scale_vaapi=w=-2:h=720") {
		t.Errorf("missing hw scale filter: %v", args)
	}
	if !slices.Contains(args, "scale=-2:360") {
		t.Errorf("missing sw scale filter: %v", args)
	}
	// どちらの出力も両方のスタイルの filter を持ってはいけない。
	if slices.Contains(args, "scale_vaapi=w=-2:h=360") {
		t.Errorf("sw output must not carry the hw scale filter: %v", args)
	}
	if slices.Contains(args, "scale=-2:720") {
		t.Errorf("hw output must not carry the sw scale filter: %v", args)
	}
	if !slices.Contains(args, "-qp") {
		t.Errorf("missing -qp for hw profile: %v", args)
	}
	if !slices.Contains(args, "-crf") {
		t.Errorf("missing -crf for sw profile: %v", args)
	}
	qpIdx := slices.Index(args, "-qp")
	if qpIdx < 0 || args[qpIdx+1] != "26" {
		t.Errorf("-qp value = %v, want 26", args)
	}
	crfIdx := slices.Index(args, "-crf")
	if crfIdx < 0 || args[crfIdx+1] != "23" {
		t.Errorf("-crf value = %v, want 23", args)
	}
}

// TestBuildLiveFFmpegArgs_InputAndOutputExtraArgsPositions は
// live.input_extra_args が -i より前、profile.extra_args が各出力の -f hls より
// 前に来ることを固定する。壊し方: 入れ替える。
func TestBuildLiveFFmpegArgs_InputAndOutputExtraArgsPositions(t *testing.T) {
	cfg := LiveConfig{
		InputExtraArgs: []string{"-re"},
		Profiles: []LiveProfile{
			{Name: "h264", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6, ExtraArgs: []string{"-b:v", "2M"}},
		},
	}
	args := BuildLiveFFmpegArgs(cfg, "/tmp/live/1", false)

	reIdx := slices.Index(args, "-re")
	iIdx := slices.Index(args, "-i")
	bvIdx := slices.Index(args, "-b:v")
	fHlsIdx := -1
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-f" && args[i+1] == "hls" {
			fHlsIdx = i
			break
		}
	}
	if reIdx < 0 || iIdx < 0 || bvIdx < 0 || fHlsIdx < 0 {
		t.Fatalf("missing -re/-i/-b:v/-f hls: %v", args)
	}
	if reIdx >= iIdx {
		t.Errorf("live.input_extra_args (-re at %d) must come before -i (at %d): %v", reIdx, iIdx, args)
	}
	if bvIdx >= fHlsIdx {
		t.Errorf("profile.extra_args (-b:v at %d) must come before -f hls (at %d): %v", bvIdx, fHlsIdx, args)
	}
}

// Enabled=false のときは Mount がライブのルートを一切登録しない
// （ffmpeg 無し環境で streamer を壊さないための、ffmpeg を一切 exec しない側の保証）。
func TestLiveStreamer_Disabled_DoesNotMountRoutes(t *testing.T) {
	mirakcSrv, _ := newFakeMirakcLiveServer(t)
	cfg := baseLiveConfig(t)
	cfg.Enabled = false
	_, srv := newTestLiveStreamer(t, mirakcSrv.URL, cfg)

	resp, err := http.Get(playlistURL(srv.URL, 0, 1, "h264"))
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

	resp, err := http.Get(playlistURL(srv.URL, 0, 1, "h264"))
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

	segURL := firstSegmentURL(t, playlistURL(srv.URL, 0, 1, "h264"))

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

// counterValue はテストのためだけに prometheus.Counter の現在値を読む
// （メトリクスはプロセス全体で共有されるので、テストは差分で見る）。
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("reading counter: %v", err)
	}
	return m.GetCounter().GetValue()
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

// live.enabled が false のとき、SPA を配る api ルーターに同居させても
// プレイリストのパスが HTML 200 にならない（404 になる）ことを、
// **実際の合成（api.NewRouter + LiveStreamer.Mount の early return + SPA
// フォールバック）で**確かめる（issue #209）。
//
// api 側の TestSPA_LivePlaylistNotFoundWhenLiveDisabled は Mounter を
// 一切渡さない構成なので、`Mount` の early return を通らない ---
// 「無効時にプレースホルダルートを登録する」形に将来変えると、あちらは
// 緑のままここだけが落ちる（CLAUDE.md「壊す場所を、実際に壊れる経路の上に置く」）。
func TestLiveMount_DisabledDoesNotFallBackToSPA(t *testing.T) {
	ls := NewLive(mirakc.NewClient("http://mirakc.invalid", nil), testLiveSite, LiveConfig{
		Enabled: false,
		// 無効なので使われない値（有効時に必須のものを埋めても登録されないこと）
		FFmpeg:     "ffmpeg",
		SegmentDir: t.TempDir(),
		Profiles:   []LiveProfile{{Name: "h264", VideoCodec: "libx264", AudioCodec: "aac"}},
	})
	router := api.NewRouter(api.RouterConfig{
		DistFS:  fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}},
		Mounter: ls,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	for _, path := range []string{
		playlistURL(srv.URL, 31920, 53248, ""),
		fmt.Sprintf("%s/api/sites/%s/networks/31920/services/53248/live/segments/segment_000.ts",
			srv.URL, testLiveSite),
	} {
		resp, body := get(t, path, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, http.StatusNotFound)
		}
		if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
			t.Errorf("GET %s: Content-Type = %q, want non-HTML（SPA に落ちている）", path, ct)
		}
		if strings.Contains(string(body), "<html>") {
			t.Errorf("GET %s: body = %q, want non-HTML", path, body)
		}
	}

	// 逆方向: 有効なら同じパスが登録される（この 404 が「無効だから」であって
	// 「パスの綴りが違うから」ではないことを示す）
	_, enabledSrv := newTestLiveStreamer(t, "http://mirakc.invalid", baseLiveConfig(t))
	resp, _ := get(t, playlistURL(enabledSrv.URL, 31920, 53248, ""), nil)
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("enabled: GET playlist = 404, want the route to exist")
	}
}

// TestBuildLiveFFmpegArgs_CaptionsFixSubDuration は ARIB 字幕の duration 欠如への
// 対処が argv に入ることを固定する。
//
// 実測（自前ビルドの ffmpeg n7.1.1 + libaribcaption、NHK Eテレの実 TS 30 秒）:
//   - -fix_sub_duration 無し: cue の終了時刻が `1193:03:08.900` になり字幕が消えない
//   - -fix_sub_duration のみ: 正常な終了時刻になるが cue が 5 本 → 4 本に減る
//     （次の字幕が来るまで現在の cue を出さないため）
//   - + -fix_sub_duration_heartbeat:v:0: cue 8 本、セグメント境界で分割される
//
// 壊し方: どちらかの append を消す / -fix_sub_duration を -i の後ろへ移す。
func TestBuildLiveFFmpegArgs_CaptionsFixSubDuration(t *testing.T) {
	cfg := LiveConfig{
		Captions: true,
		Profiles: []LiveProfile{
			{Name: "h264", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
			{Name: "low", VideoCodec: "libx264", AudioCodec: "aac", SegmentSeconds: 2, PlaylistSize: 6},
		},
	}

	args := BuildLiveFFmpegArgs(cfg, "/tmp/live/1", true)
	fixIdx, inputIdx, beats := -1, -1, 0
	for i, a := range args {
		switch a {
		case "-fix_sub_duration":
			fixIdx = i
		case "-i":
			if inputIdx < 0 {
				inputIdx = i
			}
		case "-fix_sub_duration_heartbeat:v:0":
			beats++
		}
	}
	if fixIdx < 0 {
		t.Fatalf("-fix_sub_duration missing: %v", args)
	}
	// 入力側オプションなので -i より前でなければ効かない。
	if fixIdx > inputIdx {
		t.Errorf("-fix_sub_duration at %d must precede -i at %d: %v", fixIdx, inputIdx, args)
	}
	// heartbeat は映像 variant 0 に 1 回だけ（プロファイル数に比例して増えない）。
	if beats != 1 {
		t.Errorf("-fix_sub_duration_heartbeat:v:0 count = %d, want 1: %v", beats, args)
	}

	// 字幕ストリームが無いと判定された経路では、どちらも付けない。
	off := strings.Join(BuildLiveFFmpegArgs(cfg, "/tmp/live/1", false), " ")
	if strings.Contains(off, "-fix_sub_duration") {
		t.Errorf("captionless args must not carry -fix_sub_duration: %s", off)
	}
}
