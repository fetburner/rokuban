// Package mediaexec は、メディアジョブが共用する外部コマンドの実行と probe を扱う。
// CM 検出・ロゴ候補・encode・thumbnail・seek tiles は、外部コマンドの
// 標準入出力を待つ上限と stdout を使うコマンドの終了結果の扱いを共有する。
package mediaexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// WaitDelay は、コンテキストの終了またはプロセスの終了が観測された後に
// os/exec が標準入出力の完了を待つ上限。
// streamer/session.go と同じ 5 秒に揃える。上限がかかるのは kill 後や
// 正常終了後の後始末なので、ライブ配信よりジョブの寿命が長くてもこの値でよい。
//
// テストで差し替えられるよう var にしてある。差し替えは直列テストで行う。
var WaitDelay = 5 * time.Second

// SetWaitDelay は、worker が実行する外部コマンドに共通の WaitDelay を設定する。
// stdout/stderr を *os.File 以外の Writer に接続すると、Wait はコピー goroutine の
// 終了も待つ。孫プロセスがその fd を継承したまま残る場合に待機の上限が必要になる。
// TestRunCMToolReturnsAfterCancelWhenGrandchildHoldsStdout がキャンセル時の復帰を検査する。
func SetWaitDelay(cmd *exec.Cmd) {
	cmd.WaitDelay = WaitDelay
}

// CommandOutput はコマンドの stdout を返し、失敗時は stderr を診断に使う。
// 終了エラーは ctx のエラーを優先する（TestCommandOutput_CancellationTakesPriorityOverWaitDelay）。
// 正常終了後に WaitDelay が切れた場合は取得済みの出力を返す
// （TestCommandOutput_WaitDelayExpiredOnSuccess_TreatedAsSuccess）。
func CommandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	SetWaitDelay(cmd)
	// ffprobe の stdout は duration や stream index などの機械可読な値を
	// 返す。stderr の診断メッセージを混ぜると、その値をパースできなくなるため、
	// stdout だけを返し、stderr はコマンド失敗時の診断にだけ使う。
	out, err := cmd.Output()
	if err != nil {
		// worker の encodeCommandError と同じ順序で ctx のエラーを判定する。
		// 親が exit 0 した直後に os/exec の watchCtx がキャンセルを拾い、
		// Cmd.Cancel が os.ErrProcessDone を返すと、キャンセルは Wait の
		// エラーにならない（Cmd.Cancel の契約）。孫が fd を保持していれば
		// 後から ErrWaitDelay が返りうる。ctx.Err() を先に見て、queue worker
		// の停止が重なったときに成功扱いになるのを防ぐ
		// （TestCommandOutput_CancellationTakesPriorityOverWaitDelay）。
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			// 親は exit 0 で完走したが、孫が fd を保持したまま待機の上限に達した。
			// コピー goroutine は親の生存中から読み取るので、終了までに取り込んだ
			// 出力は pipe を閉じた後も out に残る。親が出す短い stdout が切れずに
			// 残ることは TestCommandOutput_WaitDelayExpiredOnSuccess_TreatedAsSuccess
			// が検査する。
			// 孫は pipe が閉じられるまで out に追記しうる。worker の
			// ThumbnailWorker.extractFrame は stdout を捨てる。コンテナの尺を読む
			// ProbeFormatDuration は stdout を ParseFloat するので、数値以外の
			// 追記が混じるとパースが失敗しうる（実運用では未観測）。
			// 再試行ループから見分けられるよう記録は残す。
			slog.Warn("commandOutput: process exited successfully but WaitDelay expired before I/O completed",
				"name", name, "wait_delay", WaitDelay)
			return out, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return out, fmt.Errorf("%s %v: %w\nstderr: %s", name, args, err, truncateOutput([]byte(stderr)))
			}
		}
		return out, fmt.Errorf("%s %v: %w", name, args, err)
	}
	return out, nil
}

func truncateOutput(b []byte) string {
	const max = 2 << 10
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
