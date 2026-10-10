package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 孫プロセスが stdout を握ったまま残っても、ctx キャンセル後に runCMTool が戻る
// （WaitDelay が無いと CombinedOutput は孫の終了まで返らず、Work の復帰が遅れる）。
func TestRunCMToolReturnsAfterCancelWhenGrandchildHoldsStdout(t *testing.T) {
	setShortWorkerExecWaitDelay(t)
	dir := t.TempDir()
	tool := filepath.Join(dir, "fake-tool.sh")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nsleep 10 &\nsleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runCMTool(ctx, dir, tool)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runCMTool error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runCMTool did not return within 5s of cancel; a grandchild holding stdout blocks it")
	}
}
