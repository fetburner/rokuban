package main

import (
	"io"
	"os"
	"sync"
	"testing"
)

// stringWriter は captureStderr が読み戻す先に要求する形。syncLogBuffer /
// syncBuffer（allowed_hosts_test.go / server_test.go）はどちらもこれを満たす。
type stringWriter interface {
	io.Writer
	String() string
}

// captureStderr は os.Stderr を一時的にパイプへ差し替え、書かれたバイト列を
// dst へコピーし続ける。戻り値は「その時点までに書かれた分を確実に読み切って
// から dst.String() を返す」関数 --- 呼び出し側はログを読むとき必ずこれ越しに
// 読むこと（dst.String() を直接呼ばない）。
//
// **`slog.SetDefault` を先回りで差し替える方式は使えない。** loadConfig が
// config の log.level / log.format から新しいロガーを構成して
// slog.SetDefault で上書きするので（cmd/rokuban/logger.go）、テスト側が
// 先に据えたロガーは `rokuban server` を実プロセスと同じ経路（root コマンド
// 経由）で起動した時点で失われる。実際に起こる os.Stderr への書き込みを
// 捕まえるしかない。
//
// **コピー用 goroutine と読み手の間に happens-before が要る。** io.Copy は
// 別 goroutine で走り続けるので、書かれた直後に dst.String() を呼んでも
// コピーが追いついている保証はない（パイプへの Write からコピー先への
// Write までの間に、読み手がその隙間で読みに来ればすり抜ける）。
// 「読む前に必ずパイプを閉じてコピー goroutine の終了を待つ」ことで、
// その時点までの全バイトがコピー済みであることを保証する。
//
// このパッケージのテストは `t.Parallel()` を使わない前提で書かれている
// （os.Stderr の差し替えはプロセスグローバルなので、並列実行される他の
// テストのログが混入・欠落しうる）。
func captureStderr(t *testing.T, dst stringWriter) func() string {
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

	var once sync.Once
	drain := func() {
		once.Do(func() {
			_ = w.Close()
			<-done
		})
	}

	t.Cleanup(func() {
		os.Stderr = prev
		drain()
		_ = r.Close()
	})

	return func() string {
		drain()
		return dst.String()
	}
}
