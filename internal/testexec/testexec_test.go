package testexec

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// TestWriteExecutableSurvivesConcurrentFork は、並列に書いて exec する間も
// 別 goroutine が fork を回し続ける状況を作る。writeExecutable から syscall.ForkLock の
// RLock を外すと、書き込み fd を子が持ったまま exec されて ETXTBSY で落ちる。
// ETXTBSY は Linux で確認する。macOS で返るかは未検証なので、それ以外は skip する。
//
// 計測（golang:1.27.1 の linux/arm64 コンテナ、-count=50、1 回の go test 実行の中で
// 8 writer × 20 script）:
//   - RLock あり、-race なし: 50 回中 50 回 pass（0 回 fail）
//   - RLock なし、-race なし: 50 回中 3 回 pass・47 回 fail、別実行で 6 回 pass・44 回 fail
//   - RLock あり、-race あり: 50 回中 50 回 pass（DATA RACE 0 件）
//   - RLock なし、-race あり: 50 回中 14 回 pass・36 回 fail。-race でも再現する
//
// -race は CI と同じフラグだが、CI の runner（linux/amd64）では測っていない。
func TestWriteExecutableSurvivesConcurrentFork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ETXTBSY の再現は Linux でだけ確認している")
	}
	stop := make(chan struct{})
	var forkers sync.WaitGroup
	for range 4 {
		forkers.Add(1)
		go func() {
			defer forkers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = exec.Command("/bin/true").Run()
			}
		}()
	}
	t.Cleanup(func() {
		close(stop)
		forkers.Wait()
	})

	dir := t.TempDir()
	var writers sync.WaitGroup
	for w := range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range 20 {
				path := filepath.Join(dir, fmt.Sprintf("script-%d-%d", w, i))
				if err := writeExecutable(path, []byte("#!/bin/sh\nexit 0\n")); err != nil {
					t.Errorf("write: %v", err)
					continue
				}
				if err := exec.Command(path).Run(); err != nil {
					t.Errorf("exec: %v", err)
				}
			}
		}()
	}
	writers.Wait()
}
