package worker

import (
	"os/exec"
	"time"
)

// workerExecWaitDelay は、ctx キャンセル後に子プロセスが継承した標準入出力の
// fd を握っていても、Cmd.Wait がコピー goroutine を待ち続ける時間の上限。
// streamer/live.go と同じ 5 秒に揃える。エンコードやサムネイルの処理時間そのもの
// には影響せず、プロセスを kill した後の後始末だけを制限する値なので、ジョブの
// 寿命がライブ配信より長くてもこの上限でよい。
const workerExecWaitDelay = 5 * time.Second

// setWorkerExecWaitDelay は worker 内で実行する外部コマンドに共通の WaitDelay を
// 設定する。exec.Cmd が *os.File 以外の stdout/stderr を使う場合、Wait は内部の
// コピー goroutine も待つため、孫プロセスが fd を継承したケースに上限が必要になる。
func setWorkerExecWaitDelay(cmd *exec.Cmd) {
	cmd.WaitDelay = workerExecWaitDelay
}
