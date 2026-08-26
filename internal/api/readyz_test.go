package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/testutil"
)

// readyzStatus は /readyz を 1 回叩いて (ステータスコード, body の status) を返す。
func readyzStatus(t *testing.T, router http.Handler) (int, string) {
	t.Helper()
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return resp.StatusCode, body.Status
}

// DB に繋がっていれば 200。
func TestReadyz_DBReachable(t *testing.T) {
	pool := testutil.SetupDB(t)

	code, status := readyzStatus(t, NewRouter(RouterConfig{Pool: pool}))
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
	if status != "ok" {
		t.Errorf("body status = %q, want %q", status, "ok")
	}
}

// DB が落ちている（= ping が失敗する）と 503。readiness の本題そのもの。
//
// 実バイナリで postgres を止めての確認は docs/runbook/k8s.md の
// 「/readyz が DB 断で 503 になる」に手順を置いてある。ここは同じ判定を
// 到達不能なアドレスで再現する（CI に postgres の停止権限が無くても回るように）。
func TestReadyz_DBUnreachable(t *testing.T) {
	// **確実に閉じているアドレスを取る。** 固定ポート（`127.0.0.1:1` 等）だと、
	// そこで何かが listen している環境では「接続拒否」ではなく「応答しない」を
	// 測ることになり、このテストが別のものを見てしまう。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 直後に閉じる = このアドレスは connection refused を返す

	pool, err := pgxpool.New(context.Background(),
		"postgres://rokuban:rokuban@"+addr+"/rokuban?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	code, status := readyzStatus(t, NewRouter(RouterConfig{Pool: pool}))
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if status != "database unavailable" {
		t.Errorf("body status = %q, want %q", status, "database unavailable")
	}
}

// DB が「落ちている」ではなく「応答しない」ときも、有限時間で 503 を返すこと。
//
// **この判定が無いと readyzTimeout は何も主張していない。** 到達不能
// （connection refused）は即座にエラーが返るので、タイムアウトの経路を通らない。
// ここでは accept はするが 1 バイトも返さないリスナーを立てて、pgx の
// ハンドシェイクを止める。readyzTimeout を伸ばすとこのテストは落ちる。
func TestReadyz_HangingDBTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	// accept して何も書かない（Close もしない）。閉じるとエラーが即返ってしまい、
	// 「応答しない」ではなく「切断された」を測ることになる。
	//
	// **掴んだ conn の参照を保持する。** 捨てると到達不能になり、`net` が netFD に
	// 張る finalizer（`SetFinalizer(fd, (*netFD).Close)`）で GC が socket を
	// 閉じうる。閉じられると Ping は即エラーで返るので、測る対象が
	// 「応答しない」から「切断された」に変わってしまう（GOGC=1 で再現する）。
	//
	// この goroutine から t.Cleanup を呼んではいけない --- テスト終了後に accept が
	// 1 本通ると「Cleanup called after test finished」で panic する。
	// 相手側は pool.Close() / ln.Close() で閉じる。
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	pool, err := pgxpool.New(context.Background(),
		"postgres://rokuban:rokuban@"+ln.Addr().String()+"/rokuban?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	srv := httptest.NewServer(NewRouter(RouterConfig{Pool: pool}))
	defer srv.Close()

	// クライアント側の上限はハンドラ側の上限（readyzTimeout = 2s）より十分長く
	// 取る。ここで先にタイムアウトすると、測っているものがクライアントの上限に
	// なってしまう。
	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	elapsed := time.Since(start)

	var body HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if body.Status != "database unavailable" {
		t.Errorf("body status = %q, want %q（ping を通らずに 503 を返していないか）", body.Status, "database unavailable")
	}
	// readyzTimeout（2s）+ 余裕。probe の timeoutSeconds（4s）を超えたら、
	// kubelet が先に諦めるのでこの経路の意味が無くなる。
	if elapsed > 4*time.Second {
		t.Errorf("took %v, want <= 4s (readyzTimeout が効いていない)", elapsed)
	}
	// **下限も主張する。** これが無いと「Ping を一度も通らずに即 503 を返す実装」
	// でも通ってしまう（実測: nil 判定を `Stat().TotalConns() == 0` に変える変異が
	// 0.00s で PASS した）。ハンドシェイクが止まっているのだから、
	// readyzTimeout 近くまで掛かるのが正しい。
	if elapsed < readyzTimeout*4/5 {
		t.Errorf("took %v, want >= %v (タイムアウト経路を通っていない)", elapsed, readyzTimeout*4/5)
	}
}

// プール未設定は fail-closed（503）。DB を持たないプロセスを Service の後ろに
// 入れないため。
func TestReadyz_NoPool(t *testing.T) {
	code, status := readyzStatus(t, NewRouter(RouterConfig{}))
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if status != "no database pool" {
		t.Errorf("body status = %q, want %q", status, "no database pool")
	}
}

// /readyz は Host allowlist を免除する。免除し忘れると probe が 400 で落ち、
// Pod は永久に Service の後ろに入らない。
func TestReadyz_HostAllowlistExempt(t *testing.T) {
	router := NewRouter(RouterConfig{AllowedHosts: []string{"rokuban.local"}})
	srv := httptest.NewServer(router)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	// allowlist に無い、かつ alwaysAllowedHosts にも無い Host。
	req.Host = "pod-ip.cluster.local"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Pool を渡していないので 503 が正しい応答。ここで見たいのは
	// 「Host 検証で 400 にならないこと」なので、503 であることまで主張する
	// （400 と 503 を取り違えないため）。
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (400 なら Host allowlist の免除が効いていない)", resp.StatusCode)
	}
}
