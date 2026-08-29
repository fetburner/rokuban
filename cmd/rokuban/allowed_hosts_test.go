package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// syncLogBuffer は captureStderr が集めるログのための、mutex で保護した
// io.Writer。
//
// 書き手と読み手が別 goroutine にある構造なので guard する: 書くのは
// captureStderr が os.Stderr の代わりに差し込むパイプを読む goroutine、
// 読むのはテスト goroutine（String）で、両者の順序を保証する同期は harness の
// 中に無い。
//
// ただし現状の `--roles api` 経路では race を観測できていない（未検証）。実測:
// Write / String の Lock を外して `go test -race ./cmd/rokuban/ -run AllowedHosts
// -count=3` を回しても報告は 0 件だった。この経路で出るログが起動 2 行と shutdown
// だけで、前者は /healthz の応答を、後者は Cleanup の LIFO 順（slog 復元より先に
// 停止待ち）を挟むためと思われるが、そこは測っていない。ログ行が増える・別ロールを
// 足すと成立しうるので guard は残す。internal/worker と internal/reconciler の同型
// ログキャプチャはいずれも同期呼び出しのログしか見ておらず、無 guard の前例には
// ならない。
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServerForAllowedHosts は `rokuban server --roles api` を実プロセスと同じ経路
// （root コマンド → newServerCmd の RunE）で起動し、疎通した base URL と、起動中に
// os.Stderr へ書かれたログを返す。hosts は server.allowed_hosts に書く値
// （nil / 空なら allowed_hosts のキーごと書かない = 空の既定構成）。serverExtra は
// server: ブロックへそのまま挿入する追加行（`  trust_forwarded_host: true` のように
// インデント込み・改行なしで渡す。不要なら空文字列）。
//
// **`api.NewRouter` を直接叩かず、config ファイルからコマンドを起動するのが要点。**
// `server.AllowedHosts` / `server.TrustForwardedHost` を `api.RouterConfig` に渡す
// 配線は `cmd/rokuban/server.go` の `newServerCmd` の中にあり、
// `internal/api` のユニットテスト（`api.NewRouter(RouterConfig{...})` を直接呼ぶ）や
// `internal/config` のユニットテスト（YAML → 構造体までしか見ない）はどちらもこの
// 配線行の上を通らない。`cfg.Server.TrustForwardedHost` を `true` に決め打ちする
// 変異や、`warnIfAllowedHostsEmpty` への引数を決め打ちする変異を入れても、
// config → 配線の間の 1 行が検証されていなければ CI は全緑のまま #216 の脆弱性や
// allowed_hosts の警告漏れ・過剰発火が復活しうる（issue #209 の `LiveEnabled`
// 配線ミスと同型。`capabilities_test.go` の `runServerForCapabilities` のコメント
// 参照）。
func startServerForAllowedHosts(t *testing.T, hosts []string, serverExtra string) (string, *syncLogBuffer) {
	t.Helper()

	pool := testutil.SetupDB(t)
	connCfg := pool.Config().ConnConfig
	port := freePort(t)

	// config の検証は db.password を必須にするが、テスト DB は trust 認証で
	// パスワード無しのことがある（ローカル）。空なら任意の値を置く。
	password := connCfg.Password
	if password == "" {
		password = "unused-under-trust-auth"
	}

	// server: ブロックの追加行はここで 1 箇所だけ組む。呼び出し側に YAML の
	// 断片（インデントと末尾改行の有無）を持たせると、間違えたときの症状が
	// 「config のパースエラー」になって allowed_hosts の検証から遠くなる。
	var serverLines []string
	if len(hosts) > 0 {
		serverLines = append(serverLines, fmt.Sprintf("  allowed_hosts: [%s]", strings.Join(hosts, ", ")))
	}
	if serverExtra != "" {
		serverLines = append(serverLines, serverExtra)
	}

	path := writeServerTestConfig(t, fmt.Sprintf(`
server:
  listen: "127.0.0.1:%d"
%s
db:
  host: %s
  port: %d
  user: %s
  password: %s
  database: %s
  sslmode: disable
mirakc:
  url: http://mirakc.invalid:40772
storage:
  media_dir: /tmp/rokuban-allowed-hosts-test-media
`, port, strings.Join(serverLines, "\n"), connCfg.Host, connCfg.Port, connCfg.User, password, connCfg.Database))

	logs := &syncLogBuffer{}
	captureStderr(t, logs)

	ctx, cancel := context.WithCancel(context.Background())
	// exited は「コマンドが返った」ことを表す。起動待ちの側と Cleanup の側の 2 箇所
	// から見るため close する（capabilities_test.go の runServerForCapabilities と同じ形）。
	exited := make(chan struct{})
	var exitErr error
	go func() {
		root := newRootCmd()
		root.SetArgs([]string{"server", "--roles", "api", "--config", path})
		exitErr = root.ExecuteContext(ctx)
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			t.Error("server did not shut down within 30s")
		}
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// /healthz は Host allowlist を免除しているので、疎通確認に使える
	// （allowed_hosts の設定に関わらず 200 になるはず）。
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, logs
			}
			t.Fatalf("GET /healthz = %d, want 200", resp.StatusCode)
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered on %s: %v", base, err)
		}
		select {
		case <-exited:
			t.Fatalf("server exited before answering: %v", exitErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// requestWithHostHeaders は指定した Host / X-Forwarded-Host で GET /api/version し、
// ステータスコードを返す。/api/version は infraPaths の免除対象ではないので、
// Host allowlist の検証をそのまま受ける。
func requestWithHostHeaders(t *testing.T, baseURL, host, forwardedHost string) int {
	t.Helper()
	req, err := http.NewRequest("GET", baseURL+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if forwardedHost != "" {
		req.Header.Set("X-Forwarded-Host", forwardedHost)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// server.trust_forwarded_host を config に書かない（既定 false）直接露出構成では、
// X-Forwarded-Host の自己申告で Host allowlist を素通りできてはいけない
// （issue #216）。r.Host は DNS rebinding された攻撃者ドメインを想定。
//
// これは cmd/rokuban/server.go:121 の
// `TrustForwardedHost: cfg.Server.TrustForwardedHost` を `TrustForwardedHost: true`
// に決め打ちする変異を検出する唯一のテスト（internal/api・internal/config の
// ユニットテストはこの配線行の上を通らない）。
func TestServerAllowedHosts_DefaultDoesNotTrustForwardedHost(t *testing.T) {
	base, _ := startServerForAllowedHosts(t, []string{"rokuban.local"}, "")

	status := requestWithHostHeaders(t, base, "attacker-controlled.example.com", "rokuban.local")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d（trust_forwarded_host 未設定なら X-Forwarded-Host の自己申告で allowlist を素通りしてはいけない）",
			status, http.StatusBadRequest)
	}
}

// 上のテストとの両方向確認: server.trust_forwarded_host: true を明示した
// リバースプロキシ構成では、従来どおり X-Forwarded-Host を検証対象にして通す。
//
// 「allowlist 外なら 400」まで見るのは、200 側だけだと空虚に通るため ——
// allowed_hosts が config に載らなければ middleware は検証を丸ごとスキップして
// 何でも 200 にするので、startServerForAllowedHosts が allowed_hosts 行を落とす
// 変異でもこのテストは緑のままだった（実測。hosts の分岐を `if false && ...` に
// して確認）。
func TestServerAllowedHosts_OptInTrustsForwardedHost(t *testing.T) {
	base, _ := startServerForAllowedHosts(t, []string{"rokuban.local"}, "  trust_forwarded_host: true")

	status := requestWithHostHeaders(t, base, "internal-proxy.example.com", "rokuban.local")
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d（trust_forwarded_host: true なら X-Forwarded-Host が allowlist 内で通すべき）",
			status, http.StatusOK)
	}

	status = requestWithHostHeaders(t, base, "internal-proxy.example.com", "not-in-allowlist.example.com")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d（trust_forwarded_host: true でも X-Forwarded-Host が allowlist 外なら弾くべき）",
			status, http.StatusBadRequest)
	}
}
