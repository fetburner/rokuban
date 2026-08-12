package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// freePort は OS に空きポートを 1 つ選ばせる（実プロセスを起動するテスト用）。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("closing the reservation listener: %v", err)
	}
	return port
}

// runServerForCapabilities は `rokuban server --roles <roles>` を実プロセスと同じ
// 経路（root コマンド → newServerCmd の RunE）で起動し、GET /api/capabilities の
// 応答を返す。
//
// **ルーターの組み立てを直接呼ばずにコマンドを起動するのが要点。** 検証したいのは
// 「どのロールで起動しても同じ答えを返す」という配線であり、api.RouterConfig の
// どのフィールドをどのロールの分岐で埋めているかがまさに壊れどころだった
// （issue #209 のレビュー指摘: LiveEnabled の代入が api ロールの分岐の中にあり、
// 生成ルート自体はロールに関わらず生えるため、notifier 単独のプロセスに聞くと
// live:false が返っていた）。config → 公開面の写しを部分的に再現するテストでは
// この配線ミスを一度も通らない。
func runServerForCapabilities(t *testing.T, roles string, liveEnabled bool) map[string]any {
	t.Helper()

	// パッケージ専用のテスト DB を用意し、その接続情報で config を書く。
	pool := testutil.SetupDB(t)
	connCfg := pool.Config().ConnConfig
	port := freePort(t)

	// config の検証は db.password を必須にするが、テスト DB は trust 認証で
	// パスワード無しのことがある（ローカル）。空なら任意の値を置く ---
	// trust 認証では無視され、CI（`postgres://rokuban:rokuban@...`）では
	// URL 側の値がそのまま使われる。
	password := connCfg.Password
	if password == "" {
		password = "unused-under-trust-auth"
	}

	live := ""
	if liveEnabled {
		live = `
live:
  enabled: true
  segment_dir: /tmp/rokuban-capabilities-test
  profiles:
    - name: hd
      video_codec: libx264
      audio_codec: aac
`
	}
	path := writeServerTestConfig(t, fmt.Sprintf(`
server:
  listen: "127.0.0.1:%d"
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
  media_dir: /tmp/rokuban-capabilities-test-media
%s`, port, connCfg.Host, connCfg.Port, connCfg.User, password, connCfg.Database, live))

	ctx, cancel := context.WithCancel(context.Background())
	// exited は「コマンドが返った」ことを表す（close するので何度でも読める。
	// 起動待ちの側と Cleanup の側の 2 箇所から見るため、値を 1 度しか取れない
	// チャネルにしない --- 片方が受け取ると他方が永久に待つ）。
	exited := make(chan struct{})
	var exitErr error
	go func() {
		root := newRootCmd()
		root.SetArgs([]string{"server", "--roles", roles, "--config", path})
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

	url := fmt.Sprintf("http://127.0.0.1:%d/api/capabilities", port)
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /api/capabilities (roles=%s) = %d, want 200", roles, resp.StatusCode)
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered on %s: %v", url, err)
		}
		select {
		case <-exited:
			t.Fatalf("server exited before answering: %v", exitErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// live.enabled は config の値であって「このプロセスの役割」ではない。
// **api ロールを持たないプロセスに聞いても同じ答えを返す**（生成ルートは
// ロールで絞られないので、答えがロールで変わると同一デプロイの中で矛盾する）。
func TestServerCapabilities_LiveIsRoleIndependent(t *testing.T) {
	for _, roles := range []string{"api", "notifier"} {
		t.Run(roles, func(t *testing.T) {
			body := runServerForCapabilities(t, roles, true)
			if body["live"] != true {
				t.Errorf(`roles=%s: "live" = %#v, want true（config は live.enabled: true）`,
					roles, body["live"])
			}
		})
	}
}

// 逆方向: config が無効なら（どのロールでも）false。両方向を見ないと、
// 常に true を返す実装が上のテストを通してしまう。
func TestServerCapabilities_LiveFalseWhenConfigDisabled(t *testing.T) {
	body := runServerForCapabilities(t, "api", false)
	if body["live"] != false {
		t.Errorf(`"live" = %#v, want false（config に live: 節が無い）`, body["live"])
	}
}
