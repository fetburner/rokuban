package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
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
// しない。呼び出しを消す変異は単体テストの上を通らず CI が緑のまま残ってしまう
// （allowed_hosts_test.go の startServerForAllowedHosts の doc コメントにある
// 配線ミスと同型）。「第 2 引数を決め打ちにする」変異は、この片方向だけでは
// 決め打ち先が偶然「空」であれば検出できない ―― 下の
// TestServerAllowedHostsNonEmpty_NoWarnAtStartup と組みで初めて捕まえられる
// （実測: 第 2 引数を `nil` に決め打ちする変異を入れると、落ちるのは下のテスト
// だけでこのテストは緑のまま通った）。
func TestServerAllowedHostsEmpty_WarnsAtStartup(t *testing.T) {
	_, logs := startServerForAllowedHosts(t, nil, "")

	if !strings.Contains(logs.String(), "server.allowed_hosts is empty") {
		t.Errorf("startup log = %q, want a WARN mentioning empty server.allowed_hosts", logs.String())
	}
}

// TestServerAllowedHostsNonEmpty_NoWarnAtStartup は上のテストとの両方向確認:
// allowed_hosts を明示的に設定した実プロセスは起動時に WARN を出さない。
//
// これが無いと、cmd/rokuban/server.go の
//
//	warnIfAllowedHostsEmpty(slog.Default(), nil)
//
// のように第 2 引数を `cfg.Server.AllowedHosts` から `nil` へ決め打ちする変異が
// 検出されない。空の既定構成でも決め打ちの nil でも WARN は出るので、
// TestServerAllowedHostsEmpty_WarnsAtStartup だけでは両者を区別できず、
// 「allowed_hosts を正しく設定した利用者にも毎回 WARN が出る」という壊れ方が
// CI 緑のまま残ってしまう。
func TestServerAllowedHostsNonEmpty_NoWarnAtStartup(t *testing.T) {
	_, logs := startServerForAllowedHosts(t, []string{"rokuban.local"}, "")

	if strings.Contains(logs.String(), "server.allowed_hosts is empty") {
		t.Errorf("startup log = %q, want no WARN mentioning empty server.allowed_hosts when allowed_hosts is set", logs.String())
	}
}
