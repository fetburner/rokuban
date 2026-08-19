package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

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
func TestNewHTTPServer_SlowHeaderConnectionIsClosed(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := newHTTPServer(l.Addr().String(), handler)
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

	// httpReadHeaderTimeout（10 秒）を過ぎても接続が残るのは実装の欠落なので、
	// その 2 倍強を待って失敗させる。ReadHeaderTimeout が効いていれば 10 秒
	// 程度で切られ、このデッドラインには達しない。
	waitDeadline := httpReadHeaderTimeout * 3
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

	if elapsed < httpReadHeaderTimeout/2 {
		t.Fatalf("connection closed too fast (%v); expected it to be held open for close to httpReadHeaderTimeout (%v) before being cut", elapsed, httpReadHeaderTimeout)
	}
}
