//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/testutil"
)

// 2 発目の SIGTERM のテストは**別プロセスでしか書けない**。シグナルの配送は
// プロセス全体に効くので、in-process で主張どおりに動いたらテストプロセス自身が
// 死ぬ（そして主張どおりに動かなかったときだけ緑になる、反転したテストになる）。
// そこで Go 標準の手口（テストバイナリ自身を `os.Args[0]` で子として起動し、
// 環境変数で子側の入口を選ぶ）を使う。
const (
	// secondSigtermChildEnv が "1" のときだけ子側の入口が動く。
	secondSigtermChildEnv = "ROKUBAN_TEST_SECOND_SIGTERM_CHILD"
	// secondSigtermConfigEnv は親が書いた config のパスを子に渡す。
	secondSigtermConfigEnv = "ROKUBAN_TEST_SECOND_SIGTERM_CONFIG"
	// secondSigtermOnceEnv が "1" なら子を 1 件消化モードで起動する。
	secondSigtermOnceEnv = "ROKUBAN_TEST_SECOND_SIGTERM_ONCE"

	// secondSigtermSoftStop は子の `--soft-stop-timeout`。**1 発目の SIGTERM の
	// あとプロセスが確実に drain の中に居る**ようにするための長さで、待つための
	// 値ではない（親は 2 発目でプロセスを落とすので、通る経路ではこの秒数を
	// 待たない）。短くすると「2 発目が効いた」と「drain がたまたま終わった」の
	// 区別が付かなくなる。
	secondSigtermSoftStop = 60 * time.Second

	// 目印は「子が 1 発目を受け取って drain に入った」ことが外から見える点。
	// **2 発目を送る時刻をこれで決めるのが、このテストが flaky にならない理由**
	// である。固定の待ち時間で送ると、負荷の高い環境で子がまだ 1 発目を処理して
	// いない（= 正しい実装でも 2 発目が捨てられる）ときに偽陽性で赤くなる。
	//
	// **モードで目印が違う。** 常駐 worker は `eg.Wait()` が戻って
	// `slog.Info("shutting down")` に達するが、1 件消化モードでは drain が
	// errgroup の中（stopOnceProcess）で走るので、そこには**まだ到達しない**。
	// あちらの観測点は OnceGate が cancel を観測した行になる。
	shutdownLogMarker  = "shutting down"
	onceCanceledMarker = "once mode finished"
)

// TestServerCmdSecondSigtermChild は子プロセス側の入口。親
// （TestServerCmd_SecondSigtermKillsDrainingProcess）から環境変数付きで
// 起動されたときだけ本体を走らせる。
//
// **`t.Skip` で始まるのは意図的**。この関数は通常の `go test` でも収集されるが、
// 親から起動されたときにしか意味を持たない。
func TestServerCmdSecondSigtermChild(t *testing.T) {
	if os.Getenv(secondSigtermChildEnv) != "1" {
		t.Skip("親（TestServerCmd_SecondSigtermKillsDrainingProcess）から起動されたときだけ動く")
	}
	args := []string{
		"--roles", "worker", "--sites", "home", "--queues=epg",
		"--soft-stop-timeout=" + secondSigtermSoftStop.String(),
	}
	if os.Getenv(secondSigtermOnceEnv) == "1" {
		// idle timeout は効かない（ジョブは既に実行中）。長めに取って、
		// 「掴めないまま時間切れ」と区別が付くようにしておく。
		args = append(args, "--once", "--once-idle-timeout=60s")
	}
	err := runServerCmdWithContext(t, context.Background(), os.Getenv(secondSigtermConfigEnv), args...)
	// **ここに到達した = 2 発目の SIGTERM がプロセスを落としていない。**
	// 親は「シグナルで殺されたこと」を終了ステータスで見るので、正常終了と
	// 区別が付くように失敗として抜ける。
	t.Fatalf("RunE が戻った（2 発目の SIGTERM がプロセスを落としていない）: err=%v", err)
}

// markerWriter は子プロセスのログを溜めつつ、目印の文字列が現れたら seen を閉じる。
//
// パイプ（StderrPipe）ではなく Writer にしてあるのは、`cmd.Wait` が読み切りを
// 待ってくれる形にするため（パイプだと Wait と読み手の競合を呼び出し側が
// 面倒みることになる）。
type markerWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker string
	seen   chan struct{}
	closed bool
}

func newMarkerWriter(marker string) *markerWriter {
	return &markerWriter{marker: marker, seen: make(chan struct{})}
}

func (w *markerWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	// 目印が書き込みの境界をまたいでも拾えるように、都度バッファ全体を見る。
	if !w.closed && strings.Contains(w.buf.String(), w.marker) {
		w.closed = true
		close(w.seen)
	}
	return n, err
}

func (w *markerWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// **drain 中の 2 発目の SIGTERM がプロセスを落とすこと。**
//
// 1 発目の SIGTERM から `eg.Wait()` が戻るまでの間に `signal.NotifyContext` の
// 解除（`stop()`）をしないと、2 発目は signal パッケージのチャネル（バッファ 1・
// 読み手はもう居ない）に落ちて捨てられ、既定動作（プロセス終了）が抑止された
// ままになる。その先に続くのは River の drain で、長さは `--soft-stop-timeout`
// ——— encode を載せる構成には数時間を推奨しているので、**その間ずっと Ctrl-C も
// `kill -TERM` も効かない**（止める手段が SIGKILL だけになる）。
//
// **in-process では書けない**（シグナルはプロセス全体に効くので、主張どおりに
// 動くとテストプロセス自身が死ぬ）。テストバイナリ自身を子として起動し、
// 終了ステータスで見る。
//
// 空虚な成功を避けるために 3 つ確かめる:
//   - **drain の窓が開いていること**: 実行中のジョブ（blockingMirakc で掴んだまま
//     離さない epg_sync）を作り、1 発目のあともプロセスが生きていることを見る。
//     窓が無ければ 2 発目は無意味に通る
//   - **2 発目を送る時刻**: 子が 1 発目を処理したことがログに出てから送る。
//     固定待ちだと正しい実装でも偽陽性で赤くなりうる
//   - **落ち方**: 「終わった」ではなく「SIGTERM で殺された」（WaitStatus.Signaled）
//     ことを見る。正常終了や自前の exit と区別する
//
// **両モードを見る。** 解除の契機を「畳み終えたところ」に置くと常駐 worker では
// 通るが、drain を errgroup の中で回す 1 件消化モードでは畳み終えるまで解除
// されない --- **数時間の猶予を勧めている先が ScaledJob = 1 件消化モード**なので、
// 一番効いてほしい構成でだけ効かない形になる（実測: その実装では常駐側が 2 秒で
// exit 143、1 件消化モードはジョブの完走まで生き残った）。片方だけのテストは
// この差を通す。
func TestServerCmd_SecondSigtermKillsDrainingProcess(t *testing.T) {
	for _, tt := range []struct {
		name   string
		once   bool
		marker string
	}{
		{name: "常駐 worker", once: false, marker: shutdownLogMarker},
		{name: "1 件消化モード", once: true, marker: onceCanceledMarker},
	} {
		t.Run(tt.name, func(t *testing.T) { testSecondSigterm(t, tt.once, tt.marker) })
	}
}

func testSecondSigterm(t *testing.T, once bool, marker string) {
	t.Helper()
	pool := testutil.SetupDB(t)
	// release は閉じない。ジョブは掴まれたまま終わらないので、1 発目のあと
	// プロセスは soft stop の猶予（60 秒）いっぱい drain に留まる。
	mock := newBlockingMirakc(t)

	var out bytes.Buffer
	if err := runEnqueue(context.Background(), pool, "epg-sync", "home", &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}
	// httptest も config も親のものだが、子は同じホストの別プロセスなので
	// 127.0.0.1 の URL とファイルパスがそのまま見える。
	cfgPath := writeWorkerTestConfig(t, mock.url)

	logs := newMarkerWriter(marker)
	child := exec.Command(os.Args[0], "-test.run=^TestServerCmdSecondSigtermChild$", "-test.v")
	child.Env = append(os.Environ(),
		secondSigtermChildEnv+"=1",
		secondSigtermConfigEnv+"="+cfgPath,
	)
	if once {
		child.Env = append(child.Env, secondSigtermOnceEnv+"=1")
	}
	// server のログは slog（= stderr）に出る。cobra の out/err は
	// runServerCmdWithContext が捨てているので、ここに来るのは slog だけ。
	child.Stderr = logs
	child.Stdout = logs
	if err := child.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}

	// **「閉じる」形にする。** 本体も t.Cleanup も同じ待ちをするので、
	// 受け取ったら消えるチャネルにすると本体が受け取ったぶんだけ cleanup 側が
	// 待ちぼうけになる（workerProc.wait と同じ理由）。waitErr は exited が
	// 閉じたあとだけ読んでよい。
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = child.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		// 主張が成立しなかったときに子を残さない。残すと River クライアントが
		// 生き続け、後続のテストの TRUNCATE / ジョブと競合して原因を隠す。
		select {
		case <-exited:
			return
		default:
		}
		_ = child.Process.Kill()
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			t.Error("Kill しても子プロセスが終わらない")
		}
	})

	// (1) drain の窓を開ける: ジョブが mirakc を掴んだ = River が Work の中に居る。
	select {
	case <-mock.hit:
	case <-exited:
		t.Fatalf("epg_sync が走り出す前に子が終了した: %v\nログ:\n%s", waitErr, logs.String())
	case <-time.After(60 * time.Second):
		t.Fatalf("epg_sync が mirakc に到達しない（ジョブが実行中にならない）\nログ:\n%s", logs.String())
	}

	// 1 発目。ここから先、プロセスは drain（`--soft-stop-timeout` = 60 秒）に入る。
	sigAt := time.Now()
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("1 発目の SIGTERM: %v", err)
	}

	// (2) 子が 1 発目を処理したことをログで待つ（モードごとの目印）。
	select {
	case <-logs.seen:
	case <-exited:
		t.Fatalf("1 発目だけで子が終了した（drain の窓が開いていない）: %v\nログ:\n%s", waitErr, logs.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("1 発目の SIGTERM のあと %q がログに出ない\nログ:\n%s", marker, logs.String())
	}

	// (3) 2 発目を送る前に、窓がまだ開いていることを確かめる。ここで既に
	// 終わっているなら、このあとの主張は何も検出しない（空虚な成功）。
	select {
	case <-exited:
		t.Fatalf("2 発目を送る前に子が終了した（drain の窓が開いていない）: %v\nログ:\n%s", waitErr, logs.String())
	case <-time.After(500 * time.Millisecond):
	}

	// 2 発目。**1 発目で登録が外れていれば既定動作でプロセスが落ちる。**
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("2 発目の SIGTERM: %v", err)
	}

	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatalf("2 発目の SIGTERM から 5 秒たっても子が終わらない"+
			"（signal.NotifyContext の解除が漏れており、--%s の猶予 %s のあいだ "+
			"SIGTERM も Ctrl-C も効かない）\nログ:\n%s",
			softStopTimeoutFlagName, secondSigtermSoftStop, logs.String())
	}
	elapsed := time.Since(sigAt)

	// **「終わった」ではなく「シグナルで殺された」ことを見る。** 正常終了
	// （子の t.Fatalf 経由の exit 1 を含む）と区別しないと、drain を待ちきって
	// 終わった場合も緑になる。
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("子が正常終了した（2 発目がプロセスを落としていない）: err=%v\nログ:\n%s", waitErr, logs.String())
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("WaitStatus が取れない: %T", exitErr.Sys())
	}
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("子の終了 = %v（signaled=%v）, want SIGTERM で殺されること\nログ:\n%s",
			exitErr, status.Signaled(), logs.String())
	}
	// 猶予（60 秒）を待ちきったのではないこと。上の Signaled でほぼ言えているが、
	// 「2 発目に反応した」ことを時間の側からも押さえる。
	if elapsed >= secondSigtermSoftStop {
		t.Errorf("1 発目から終了まで %s（--%s の猶予 %s を待ちきっている）",
			elapsed, softStopTimeoutFlagName, secondSigtermSoftStop)
	}

	// **drain の途中で殺されたことを DB 側からも確かめる。** 猶予を待ちきって
	// 畳んだのなら、River は行を `running` から動かしている（available への
	// 差し戻し）。`running` のままなのは、completer が走る前にプロセスが
	// 消えたということ = 2 発目が効いた窓が本物だったということ。
	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM river_job WHERE kind = 'epg_sync'`,
	).Scan(&state); err != nil {
		t.Fatalf("reading epg_sync job: %v", err)
	}
	if state != "running" {
		t.Errorf("epg_sync state = %q, want %q（drain の途中ではなかった）", state, "running")
	}
}
