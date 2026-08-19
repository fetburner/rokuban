package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// listenLocal は各テスト用にループバック上のポート未指定で新しい TCP リスナーを
// 1 本立てて返す。アドレスは呼び出し側が l.Addr().String() で取る。
func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	return l
}

// TestNewProductionHTTPServer_UsesReviewedTimeouts は、本番の入口
// newProductionHTTPServer が返す *http.Server に載るタイムアウトを固定する。
//
// 期待値は httpReadHeaderTimeout / httpIdleTimeout ではなくリテラルで書く
// （CLAUDE.md「実装の定数と比較するテストは何も主張していない」）。これで
// 定数の値の劣化と、newProductionHTTPServer 内で 2 つの定数を入れ替える変異の
// 両方が落ちる。
func TestNewProductionHTTPServer_UsesReviewedTimeouts(t *testing.T) {
	srv := newProductionHTTPServer("127.0.0.1:0", http.NotFoundHandler())

	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 2m", srv.IdleTimeout)
	}
	// WriteTimeout を設定しない判断（SSE / HLS を切らない）も本番の入口で固定する。
	// 動作側の裏付けは TestNewHTTPServer_LongRunningHandlerNotCutByIdleTimeout。
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0（SSE・HLS の長寿命レスポンスを切らないため設定しない）", srv.WriteTimeout)
	}
}

// TestServerSlowHeaderConnectionIsClosed は、`rokuban server --roles api` を実
// プロセスと同じ経路で起動し、その listen ソケットに slow-header の生 TCP
// プローブを当てて、本番の配線が ReadHeaderTimeout を実際に持っていることを
// 確認する。
//
// **newProductionHTTPServer を直接呼ぶのではなく、newServerCmd の中にある
// 「srv := newProductionHTTPServer(cfg.Server.Listen, ...)」の 1 行の上を通すのが
// 要点。** newHTTPServer を直接呼ぶ 3 本のユニットテストはこの行を通らないので、
// この行が生の &http.Server{} に戻る変異を検出できない（allowed_hosts_test.go の
// startServerForAllowedHosts の doc コメントにある #209 / #216 と同型 —— 配線 1 行
// が CI 全緑のまま壊れる）。
//
// 上限は「本番の 10 秒より十分大きく、劣化・入れ替えより十分小さい」20 秒に取る。
// 正確な値は TestNewProductionHTTPServer_UsesReviewedTimeouts が固定するので、
// ここは配線が生きていることだけを見る。
func TestServerSlowHeaderConnectionIsClosed(t *testing.T) {
	// この harness は allowed_hosts のテストと同じもの（実プロセス起動の経路が
	// 1 本しかないので使い回す）。allowed_hosts の設定はこのテストの経路には
	// 影響しない —— ヘッダーを送り切らないので Host の検証まで到達しない。
	base, _ := startServerForAllowedHosts(t, []string{"rokuban.local"}, "")

	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial(%s): %v", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("GET /api/version HTTP/1.1\r\nHost: rokuban.local\r\n")); err != nil {
		t.Fatalf("writing partial request: %v", err)
	}

	const waitDeadline = 20 * time.Second
	if err := conn.SetReadDeadline(time.Now().Add(waitDeadline)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	buf := make([]byte, 1)
	start := time.Now()
	_, readErr := conn.Read(buf)
	elapsed := time.Since(start)

	if readErr == nil {
		t.Fatalf("expected the server to close the slow-header connection, got a successful read after %v", elapsed)
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("the real server did not close the slow-header connection within %v: %v "+
			"(cmd/rokuban/server.go の srv := newProductionHTTPServer(...) の行が ReadHeaderTimeout を"+
			"載せていないか、10 秒より大きい値になっている)", waitDeadline, readErr)
	}

	// 本番の ReadHeaderTimeout は 10 秒なので、数秒で切られるのは「別の理由で
	// 落ちた」か「値が劇的に縮んだ」のどちらか。どちらもこのテストの主張が
	// 空虚になるので落とす。
	if elapsed < 5*time.Second {
		t.Fatalf("connection closed after only %v; expected it to be held open for close to the production "+
			"ReadHeaderTimeout (10s) before being cut (read error: %v)", elapsed, readErr)
	}
}

// TestNewHTTPServer_SlowHeaderConnectionIsClosed は、リクエストヘッダーを送り
// 切らないクライアントの接続が有限時間でサーバー側から切られることを確認する
// （issue #368 の受け入れ基準「不完全な HTTP header を送る接続が有限時間で
// server 側から切られる」）。
//
// **`api.NewRouter` を直接 `httptest.NewServer` に渡すのではなく、`newHTTPServer`
// が返す `*http.Server` を実際の TCP リスナーに `Serve` させる**（issue #368
// 「罠」: テストは http.Server の配線を通す必要がある）。`httptest.Server` は
// 既定でこのタイムアウトを設定しないため、ルーター単体のテストでは
// ReadHeaderTimeout の有無を検出できない。
//
// readHeaderTimeout はテスト専用の短い値をリテラルで渡す（本番の
// httpReadHeaderTimeout=10秒 と比較すると定数を変えても通るテストになるため。
// CLAUDE.md「実装の定数と比較するテストは何も主張していない」）。idleTimeout は
// この経路に影響しないだけの大きな値にして固定する。
func TestNewHTTPServer_SlowHeaderConnectionIsClosed(t *testing.T) {
	const readHeaderTimeout = 200 * time.Millisecond

	l := listenLocal(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := newHTTPServer(l.Addr().String(), handler, readHeaderTimeout, time.Hour)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// リクエストラインだけ送り、ヘッダーの終端（空行）を送らない --- ヘッダーを
	// 送り切っていないクライアントを模す。
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n")); err != nil {
		t.Fatalf("writing partial request: %v", err)
	}

	// readHeaderTimeout を過ぎても接続が残るのは実装の欠落なので、その 10 倍を
	// 待って失敗させる。ReadHeaderTimeout が効いていれば readHeaderTimeout
	// 程度で切られ、このデッドラインには達しない。
	waitDeadline := readHeaderTimeout * 10
	if err := conn.SetReadDeadline(time.Now().Add(waitDeadline)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	buf := make([]byte, 1)
	start := time.Now()
	_, readErr := conn.Read(buf)
	elapsed := time.Since(start)

	if readErr == nil {
		t.Fatalf("expected the server to close the slow-header connection, got a successful read after %v", elapsed)
	}
	if !errors.Is(readErr, io.EOF) {
		var netErr net.Error
		if errors.As(readErr, &netErr) && netErr.Timeout() {
			t.Fatalf("connection was not closed within %v (ReadHeaderTimeout is not taking effect): %v", waitDeadline, readErr)
		}
		// EOF 以外の切断（connection reset 等）も「切られた」とみなす。
	}

	if elapsed < readHeaderTimeout/2 {
		t.Fatalf("connection closed too fast (%v); expected it to be held open for close to readHeaderTimeout (%v) before being cut", elapsed, readHeaderTimeout)
	}
}

// TestNewHTTPServer_IdleConnectionIsClosedByIdleTimeout は、1 リクエスト分の
// レスポンスを受け取り終えたあと次のリクエストを送らずに keep-alive のまま
// 待っている接続が、idleTimeout で有限時間内にサーバー側から切られることを
// 確認する（server.go の httpIdleTimeout の doc コメントが主張する「次の
// リクエストの到着待ちの間だけ働く」の前半）。
//
// readHeaderTimeout はこの経路に影響しない大きな値に固定し、idleTimeout だけを
// テスト専用の短い値にする。
func TestNewHTTPServer_IdleConnectionIsClosedByIdleTimeout(t *testing.T) {
	const idleTimeout = 200 * time.Millisecond

	l := listenLocal(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := newHTTPServer(l.Addr().String(), handler, time.Hour, idleTimeout)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")); err != nil {
		t.Fatalf("writing request: %v", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// ここから次のリクエストを送らずに待つ --- keep-alive のアイドル接続を模す。
	waitDeadline := idleTimeout * 10
	if err := conn.SetReadDeadline(time.Now().Add(waitDeadline)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	buf := make([]byte, 1)
	start := time.Now()
	_, readErr := conn.Read(buf)
	elapsed := time.Since(start)

	if readErr == nil {
		t.Fatalf("expected the server to close the idle keep-alive connection, got a successful read after %v", elapsed)
	}
	if !errors.Is(readErr, io.EOF) {
		var netErr net.Error
		if errors.As(readErr, &netErr) && netErr.Timeout() {
			t.Fatalf("connection was not closed within %v (IdleTimeout is not taking effect): %v", waitDeadline, readErr)
		}
	}

	if elapsed < idleTimeout/2 {
		t.Fatalf("connection closed too fast (%v); expected it to be held open for close to idleTimeout (%v) before being cut", elapsed, idleTimeout)
	}
}

// TestNewHTTPServer_LongRunningHandlerNotCutByIdleTimeout は、ハンドラが応答を
// 書き続けている間（SSE の hub.Run や HLS のセグメント配信を模す）は
// idleTimeout を超えても接続が切られないことを確認する。server.go の
// httpIdleTimeout の doc コメントが主張する「ハンドラが応答を書き続けている間は
// 対象にならない」を実測で裏付ける唯一のテスト。
func TestNewHTTPServer_LongRunningHandlerNotCutByIdleTimeout(t *testing.T) {
	const idleTimeout = 80 * time.Millisecond
	const chunkInterval = 60 * time.Millisecond
	const chunkCount = 6 // 合計 360ms > idleTimeout の 4.5 倍

	l := listenLocal(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "ResponseWriter does not implement http.Flusher", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for i := 0; i < chunkCount; i++ {
			time.Sleep(chunkInterval)
			_, _ = fmt.Fprintf(w, "chunk-%d\n", i)
			flusher.Flush()
		}
	})
	srv := newHTTPServer(l.Addr().String(), handler, time.Hour, idleTimeout)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetReadDeadline(time.Now().Add(chunkInterval*time.Duration(chunkCount) + 5*time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	start := time.Now()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")); err != nil {
		t.Fatalf("writing request: %v", err)
	}

	// http.ReadResponse は Transfer-Encoding: chunked を自動で解いて Body から
	// 読めるようにする。ステータス行や chunk のサイズプレフィックスを手で解釈
	// せずに、ハンドラが書いた本文だけを読み切れるようにするため使う。
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: %d", resp.StatusCode)
	}

	// 残りのチャンクを読み切るまで、途中で読み取りエラー（切断）が起きないことを
	// 確認する。全体の所要時間は idleTimeout の 4.5 倍を超えるので、IdleTimeout が
	// 応答書き込み中の接続にも効いていればここで読み取りエラーになる。
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("unexpected read error while handler was still writing: %v", err)
	}
	elapsed := time.Since(start)

	if len(body) == 0 {
		t.Fatalf("expected to read chunks written by the handler, got none")
	}
	if elapsed < idleTimeout*2 {
		t.Fatalf("handler finished too fast (%v); the test doesn't actually exercise writing past idleTimeout (%v)", elapsed, idleTimeout)
	}
}
