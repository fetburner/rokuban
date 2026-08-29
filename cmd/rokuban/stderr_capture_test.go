package main

import (
	"io"
	"os"
	"testing"
)

// captureStderr は os.Stderr を一時的にパイプへ差し替え、書かれたバイト列を
// dst へコピーし続ける。
//
// **`slog.SetDefault` を先回りで差し替える方式は使えない。** loadConfig が
// config の log.level / log.format から新しいロガーを構成して
// slog.SetDefault で上書きするので（cmd/rokuban/logger.go）、テスト側が
// 先に据えたロガーは `rokuban server` を実プロセスと同じ経路（root コマンド
// 経由）で起動した時点で失われる。実際に起こる os.Stderr への書き込みを
// 捕まえるしかない。
func captureStderr(t *testing.T, dst io.Writer) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	prev := os.Stderr
	os.Stderr = w

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(dst, r)
		close(done)
	}()

	t.Cleanup(func() {
		os.Stderr = prev
		_ = w.Close()
		<-done
		_ = r.Close()
	})
}
