package mediaexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testexec"
)

func TestWaitDelayExpiredOnSuccess(t *testing.T) {
	successCmd := runWaitDelayTestCommand(t, "success", 0)
	failureCmd := runWaitDelayTestCommand(t, "failure", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		cmd  *exec.Cmd
		err  error
		want bool
	}{
		{name: "context_canceled", ctx: ctx, cmd: successCmd, err: exec.ErrWaitDelay, want: false},
		{name: "failed_process", ctx: context.Background(), cmd: failureCmd, err: exec.ErrWaitDelay, want: false},
		{name: "successful_process", ctx: context.Background(), cmd: successCmd, err: exec.ErrWaitDelay, want: true},
		{name: "other_error", ctx: context.Background(), cmd: successCmd, err: errors.New("other error"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WaitDelayExpiredOnSuccess(tt.ctx, tt.cmd, tt.err); got != tt.want {
				t.Fatalf("WaitDelayExpiredOnSuccess() = %t, want %t", got, tt.want)
			}
		})
	}
}

func runWaitDelayTestCommand(t *testing.T, name string, exitCode int) *exec.Cmd {
	t.Helper()
	tool := filepath.Join(t.TempDir(), name)
	testexec.WriteExecutable(t, tool, []byte(fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)))
	cmd := exec.Command(tool)
	if err := cmd.Run(); exitCode == 0 && err != nil {
		t.Fatalf("running successful test command: %v", err)
	} else if exitCode != 0 && err == nil {
		t.Fatal("failed test command succeeded")
	}
	return cmd
}

// TestCommandOutput_IncludesStderrOnFailure は失敗時に *exec.ExitError の
// Stderr をエラーに含めることを検査する。cmd.Output() の出力は stdout なので、
// stderr の診断を呼び出し元に届けるには、この分岐でエラーに載せる必要がある。
func TestCommandOutput_IncludesStderrOnFailure(t *testing.T) {
	t.Parallel()
	fake := filepath.Join(t.TempDir(), "fake-ffprobe")
	script := "#!/bin/sh\necho 'diagnostic: something went wrong' >&2\nexit 1\n"
	testexec.WriteExecutable(t, fake, []byte(script))

	_, err := CommandOutput(context.Background(), fake)
	if err == nil || !strings.Contains(err.Error(), "diagnostic: something went wrong") {
		t.Fatalf("CommandOutput() error = %v, want stderr diagnostic", err)
	}
}

// TestCommandOutput_WaitDelayExpiredOnSuccess_TreatedAsSuccess は、worker の
// encodeCommandError と同じく、正常終了後の ErrWaitDelay を成功扱いすることを
// 検査する。stdout の fd を継承した子を残す偽コマンドで CommandOutput を直接
// 呼び、WaitDelay が実際に切れるのを待つため数秒かかる。
func TestCommandOutput_WaitDelayExpiredOnSuccess_TreatedAsSuccess(t *testing.T) {
	tool, marker := installLeakyCommand(t)
	var logBuf bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(logger) })

	start := time.Now()
	out, err := CommandOutput(context.Background(), tool, marker)
	if err != nil {
		t.Fatalf("CommandOutput(): %v", err)
	}
	// 孫が標準出力を保持し、WaitDelay の経路を実際に踏んだことを確かめる。
	elapsed := time.Since(start)
	if elapsed < WaitDelay/2 {
		t.Fatalf("CommandOutput() returned after %s; want roughly WaitDelay (%s), the leaky child fd did not force a WaitDelay wait", elapsed, WaitDelay)
	}
	if elapsed > 5*time.Second {
		t.Errorf("CommandOutput() took %s, want at most 5s", elapsed)
	}
	if !strings.Contains(logBuf.String(), "WaitDelay expired before I/O completed") {
		t.Errorf("log output = %q, want WaitDelay warning", logBuf.String())
	}
	if string(out) != "progress=end\n" {
		t.Errorf("stdout = %q, want progress=end followed by a newline", out)
	}
}

func TestCommandOutput_CancellationTakesPriorityOverWaitDelay(t *testing.T) {
	tool, marker := installLeakyCommand(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := CommandOutput(ctx, tool, marker)
		done <- result{out, err}
	}()
	// 親の終了を待ってからキャンセルし、孫が保持する stdout で WaitDelay の経路を踏む。
	deadline := time.Now().Add(5 * time.Second)
	for {
		pid := commandPID(t, marker, 0)
		if pid > 0 && syscall.Kill(pid, 0) == syscall.ESRCH {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || string(got.out) != "progress=end\n" {
			t.Errorf("CommandOutput() = %q, %v, want captured stdout and context.Canceled", got.out, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CommandOutput() did not return after cancel")
	}
}

// installLeakyCommand は、stdout を継承する子を残して成功終了するコマンドを作る。
func installLeakyCommand(t *testing.T) (tool, marker string) {
	t.Helper()
	previous := WaitDelay
	WaitDelay = time.Second
	t.Cleanup(func() { WaitDelay = previous })
	dir := t.TempDir()
	tool = filepath.Join(dir, "fake-tool")
	marker = filepath.Join(dir, "pids")
	script := "#!/bin/sh\nsleep 10 &\nprintf '%s %s\\n' \"$$\" \"$!\" > \"$1\"\nprintf 'progress=end\\n'\n"
	testexec.WriteExecutable(t, tool, []byte(script))
	start := time.Now()
	t.Cleanup(func() {
		if time.Since(start) < 10*time.Second {
			if pid := commandPID(t, marker, 1); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return tool, marker
}

func commandPID(t *testing.T, marker string, index int) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || !bytes.HasSuffix(data, []byte("\n")) {
		return 0
	}
	pid, err := strconv.Atoi(fields[index])
	if err != nil {
		t.Fatal(err)
	}
	return pid
}
