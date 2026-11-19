// Package testexec は、テストが実行ファイル（ffmpeg の代役のスクリプトなど）を書いて
// すぐ exec するための補助を置く。
//
// 置き場を internal/testutil にしない理由: testutil は internal/config を import している。
// internal/config の内部テスト（package config）から testutil を使うと import が循環するため、
// 標準ライブラリだけに依存する葉のパッケージに分けている。
package testexec

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

// WriteExecutable は path に content を書き、実行可能（0o700）にする。
// テストで実行ファイルを書くときは os.WriteFile ではなくこの関数を使う。
//
// os.WriteFile の直後に exec すると、並列テストの fork が書き込み用 fd を
// 子プロセスに持ち越し、exec が ETXTBSY（text file busy）で失敗することがある
// （再現: TestWriteExecutableSurvivesConcurrentFork。計測値はそのテストの doc comment）。
// この関数は syscall.ForkLock を read で取り、open・write・close の間に fork が
// 起きないようにする。exec はロックの外で行うこと。fork は ForkLock を write で
// 取るため、exec まで握ると自分がデッドロックする。
// 数百バイトのスクリプト専用。大きなファイルには使わない（その間、全 goroutine の fork が止まる）。
func WriteExecutable(t testing.TB, path string, content []byte) {
	t.Helper()
	if err := writeExecutable(path, content); err != nil {
		t.Fatalf("testexec.WriteExecutable: %v", err)
	}
}

// writeExecutable は WriteExecutable の本体。テストの goroutine 以外（t.Fatal が使えない場所）から
// 呼べるように、エラーを返す形にしている。
func writeExecutable(path string, content []byte) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
