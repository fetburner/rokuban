package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// workerExecWaitDelay は、ctx キャンセル後に子プロセスが継承した標準入出力の
// fd を握っていても、Cmd.Wait がコピー goroutine を待ち続ける時間の上限。
// streamer/session.go と同じ 5 秒に揃える。エンコードやサムネイルの処理時間そのもの
// には影響せず、プロセスを kill した後の後始末だけを制限する値なので、ジョブの
// 寿命がライブ配信より長くてもこの上限でよい。
//
// テストが短縮できるよう var にしてある（setShortWorkerExecWaitDelay）。本番で
// 書き換える箇所は無い。
var workerExecWaitDelay = 5 * time.Second

// setWorkerExecWaitDelay は worker 内で実行する外部コマンドに共通の WaitDelay を
// 設定する。exec.Cmd が *os.File 以外の stdout/stderr を使う場合、Wait は内部の
// コピー goroutine も待つため、孫プロセスが fd を継承したケースに上限が必要になる。
func setWorkerExecWaitDelay(cmd *exec.Cmd) {
	cmd.WaitDelay = workerExecWaitDelay
}

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	setWorkerExecWaitDelay(cmd)
	// ffprobe の stdout は duration や stream index などの機械可読な値を
	// 返す。stderr の診断メッセージを混ぜると、その値をパースできなくなるため、
	// stdout だけを返し、stderr はコマンド失敗時の診断にだけ使う。
	out, err := cmd.Output()
	if err != nil {
		// ctx キャンセルを WaitDelay-success 分岐より先に見る（encode.go の
		// runEncode と同型）。watchCtx の Cancel が os.ErrProcessDone を返す
		// 競合（プロセスは既に exit 0 していた）では err は一旦 nil のままで、
		// 後から WaitDelay の分岐だけが ErrWaitDelay を立てる。ここで
		// ctx.Err() を先に見ないと、River のシャットダウンが ffmpeg/ffprobe の
		// exit 0 直後に当たったケースを黙って成功扱いにしてしまう。
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			// exit 0 の完走後、孫プロセスが fd を握ったままで WaitDelay が
			// 先に切れた場合（encode.go の runEncode と同型）。コピー
			// goroutine はプロセスが書いた分をプロセス生存中に drain し
			// 続けているので、out がプロセス自身の出力より短く切れることは
			// ない。到達しうるのは逆方向 --- fd を継承した孫プロセスが
			// 強制クローズまでの WaitDelay の窓の間に out へ追記しうること。
			// extractFrame の呼び出しでは out 自体を捨てるので無害。
			// probeDuration は out を ParseFloat するので、追記があれば
			// パース自体が失敗する（未観測 --- 実運用の ffprobe 出力はごく
			// 短時間で読み切れるため、この窓に孫が居合わせた例は無い）。
			// 再試行ループから見分けられるよう記録は残す。
			slog.Warn("commandOutput: process exited successfully but WaitDelay expired before I/O completed",
				"name", name, "wait_delay", workerExecWaitDelay)
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
