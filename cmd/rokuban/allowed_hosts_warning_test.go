package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// TestWarnIfAllowedHostsEmpty_EmptyLogsWarning は、allowed_hosts が空のときに
// WARN ログが出ることを確認する。
func TestWarnIfAllowedHostsEmpty_EmptyLogsWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	warnIfAllowedHostsEmpty(logger, nil)

	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("log = %q, want a WARN-level record", buf.String())
	}
	if !strings.Contains(buf.String(), "server.allowed_hosts is empty") {
		t.Errorf("log = %q, want it to mention server.allowed_hosts being empty", buf.String())
	}
}

// TestWarnIfAllowedHostsEmpty_NonEmptyLogsNothing は上のテストとの両方向確認:
// allowed_hosts が非空なら何もログに出さない（意識して allowlist を設定した
// 構成にまで警告を出すと、警告の意味が薄れる）。
func TestWarnIfAllowedHostsEmpty_NonEmptyLogsNothing(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	warnIfAllowedHostsEmpty(logger, []string{"rokuban.local"})

	if buf.Len() != 0 {
		t.Errorf("log = %q, want no output when allowed_hosts is non-empty", buf.String())
	}
}

// TestServerAllowedHostsEmpty_WarnsAtStartup は、`rokuban server --roles api` を
// 実プロセスと同じ経路（root コマンド → newServerCmd の RunE）で allowed_hosts を
// 空にして起動したとき、実際に slog.Default() へ WARN が出ることを確認する。
//
// warnIfAllowedHostsEmpty 単体のテストは、newServerCmd の RunE がそれを
// cfg.Server.AllowedHosts と slog.Default() で実際に呼んでいることまでは検証
// しない。呼び出しを消す・引数を決め打ちにする変異は単体テストの上を通らず
// CI が緑のまま残ってしまう（allowed_hosts_test.go の startServerForAllowedHosts
// の doc コメントにある配線ミスと同型）。そのためここでは実プロセスを起動し、
// slog のデフォルト出力先を差し替えて WARN が実際に書かれることを確認する。
func TestServerAllowedHostsEmpty_WarnsAtStartup(t *testing.T) {
	pool := testutil.SetupDB(t)
	connCfg := pool.Config().ConnConfig
	port := freePort(t)

	password := connCfg.Password
	if password == "" {
		password = "unused-under-trust-auth"
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
  media_dir: /tmp/rokuban-allowed-hosts-warning-test-media
`, port, connCfg.Host, connCfg.Port, connCfg.User, password, connCfg.Database))

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	ctx, cancel := context.WithCancel(context.Background())
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
	// /healthz は Host allowlist を免除しているので、疎通確認に使える。
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
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

	if !strings.Contains(logBuf.String(), "server.allowed_hosts is empty") {
		t.Errorf("startup log = %q, want a WARN mentioning empty server.allowed_hosts", logBuf.String())
	}
}
