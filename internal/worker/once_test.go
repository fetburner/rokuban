package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"
)

// onceWorkFunc は OnceGate の middleware を「ジョブ 1 件の Work」として呼べる
// 関数にする。型アサーションはテスト goroutine 側で済ませ、返す関数は別 goroutine
// から呼べるようにしてある（t.Fatalf をテスト goroutine 以外から呼べないため）。
func onceWorkFunc(t *testing.T, g *OnceGate) func(func(context.Context) error) error {
	t.Helper()
	mw, ok := g.Middleware().(rivertype.WorkerMiddleware)
	if !ok {
		t.Fatalf("Middleware() = %T, want rivertype.WorkerMiddleware", g.Middleware())
	}
	return func(inner func(context.Context) error) error {
		return mw.Work(context.Background(), &rivertype.JobRow{}, inner)
	}
}

// waitOutcome は Wait を別 goroutine で回し、**有限時間で戻ること**まで見る。
//
// claim 済みの Wait はタイムアウトを持たない（実行中のジョブを打ち切らない）
// 設計なので、「middleware が done を閉じない」類の変異は Wait が戻らない形で
// 現れる。そのまま呼ぶと go test 全体のタイムアウト（45 秒後の panic ダンプ）に
// なり、何が壊れたのか出力から読めない --- ここで打ち切ってテストの失敗として
// 報告する。
func waitOutcome(t *testing.T, g *OnceGate, idleTimeout time.Duration) OnceOutcome {
	t.Helper()
	out := make(chan OnceOutcome, 1)
	go func() { out <- g.Wait(context.Background(), idleTimeout) }()
	limit := idleTimeout + 5*time.Second
	select {
	case o := <-out:
		return o
	case <-time.After(limit):
		t.Fatalf("Wait が %s 以内に戻らない（1 件消化モードの Job が終了しない形）", limit)
		return OnceOutcomeCanceled
	}
}

// ジョブ 1 件が Work を抜けたら Wait が job_done を返すこと（1 件消化モードの
// 本線）。idleTimeout を十分長く取ってあるので、job_done はタイマー経由では
// 出ない。
//
// このテストが検出すべき変異: middleware が done を閉じない。実測ではその変異で
// waitOutcome が `Wait が 7s 以内に戻らない` で落ちる（1 件消化モードの Job が
// 終了しない = 判定 2.4 が永久に FAIL する形そのもの）。
func TestOnceGate_WaitReturnsJobDoneAfterOneJob(t *testing.T) {
	g := NewOnceGate()
	work := onceWorkFunc(t, g)

	go func() { _ = work(func(context.Context) error { return nil }) }()

	if got := waitOutcome(t, g, 2*time.Second); got != OnceOutcomeJobDone {
		t.Errorf("Wait() = %v, want %v", got, OnceOutcomeJobDone)
	}
}

// ジョブを 1 件も claim できないまま待ち時間を使い切ったら idle_timeout を
// 返すこと（KEDA が overshoot して起こした空振りの Job を終わらせる経路）。
//
// このテストが検出すべき変異: タイマー分岐を削る / claim 前でも waitDone へ
// 落とす（Wait が戻らなくなり、テストがタイムアウトで落ちる）。
func TestOnceGate_WaitReturnsIdleTimeoutWhenNothingClaimed(t *testing.T) {
	g := NewOnceGate()

	if got := waitOutcome(t, g, 20*time.Millisecond); got != OnceOutcomeIdleTimeout {
		t.Errorf("Wait() = %v, want %v", got, OnceOutcomeIdleTimeout)
	}
}

// **実行中のジョブは idleTimeout で打ち切らないこと。** ScaledJob を選んだ理由
// そのもの（「ジョブは完走するまで殺されない」。docs/operations.md §5）なので、
// これが壊れると 1 件消化モードは数時間のエンコードを途中で捨てる形になる。
//
// **発火済みのタイムアウトチャネルを渡して wait を直接呼ぶ。** Wait 経由では
// この分岐を踏めない --- select 評価の時点で実時間のタイマーはまだ発火して
// いないため、started が先に選ばれてタイマー分岐に入らない（実測: 先行チェックを
// 削る変異が Wait 経由のテストでは 3 回連続で通ってしまった）。
//
// 20 回繰り返すのは、タイマー分岐の再確認を削る変異を確実に落とすため ---
// started と timeout の両方が準備できているので select はランダムに選び、
// 1 回だけでは約半分の確率で通ってしまう。
func TestOnceGate_IdleTimeoutDoesNotCutRunningJob(t *testing.T) {
	for i := range 20 {
		g := NewOnceGate()
		work := onceWorkFunc(t, g)

		release := make(chan struct{})
		go func() { _ = work(func(context.Context) error { <-release; return nil }) }()

		// ジョブが Work に入った（claim 済みになった）ことを待つ。
		select {
		case <-g.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: ジョブが Work に入らなかった", i)
		}

		// タイムアウトは既に発火済み。
		fired := make(chan time.Time, 1)
		fired <- time.Time{}

		outcome := make(chan OnceOutcome, 1)
		go func() { outcome <- g.wait(context.Background(), fired) }()

		select {
		case got := <-outcome:
			close(release)
			t.Fatalf("iteration %d: 実行中のジョブがあるのに wait が %v を返した", i, got)
		case <-time.After(10 * time.Millisecond):
		}

		close(release)
		if got := <-outcome; got != OnceOutcomeJobDone {
			t.Fatalf("iteration %d: wait() = %v, want %v", i, got, OnceOutcomeJobDone)
		}
	}
}

// **middleware が Work のエラーを握り潰さないこと。** 握り潰すと River は
// 失敗したジョブを completed として確定させ、二度と再試行しない
// （at-least-once が壊れる）。プロセスの終了コードを 0 にするのは
// cmd/rokuban 側の責任であって、ここでエラーを消すことではない。
//
// あわせて、失敗でも「1 件消化した」として Wait が job_done を返すことも見る
// （失敗ジョブで Job が終わらないと、KEDA が起こした Job が居座る）。
func TestOnceGate_MiddlewarePropagatesWorkerError(t *testing.T) {
	g := NewOnceGate()
	work := onceWorkFunc(t, g)

	wantErr := errors.New("boom")
	if got := work(func(context.Context) error { return wantErr }); !errors.Is(got, wantErr) {
		t.Errorf("middleware が返したエラー = %v, want %v（握り潰すと River が completed で確定させる）", got, wantErr)
	}

	if got := waitOutcome(t, g, 2*time.Second); got != OnceOutcomeJobDone {
		t.Errorf("Wait() = %v, want %v（失敗でも 1 件消化として終わること）", got, OnceOutcomeJobDone)
	}
}

// ctx 終了（SIGTERM / ノード退避）で Wait が canceled を返すこと。
// claim 前と claim 後の両方向を見る。
func TestOnceGate_WaitReturnsCanceledOnContextDone(t *testing.T) {
	t.Run("claim 前", func(t *testing.T) {
		g := NewOnceGate()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got := g.Wait(ctx, time.Hour); got != OnceOutcomeCanceled {
			t.Errorf("Wait() = %v, want %v", got, OnceOutcomeCanceled)
		}
	})

	t.Run("claim 後", func(t *testing.T) {
		g := NewOnceGate()
		work := onceWorkFunc(t, g)
		release := make(chan struct{})
		defer close(release)
		go func() { _ = work(func(context.Context) error { <-release; return nil }) }()
		select {
		case <-g.started:
		case <-time.After(5 * time.Second):
			t.Fatal("ジョブが Work に入らなかった")
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// 実行中でも ctx 終了なら戻る（drain は呼び出し側の riverClient.Stop が担う）。
		if got := g.Wait(ctx, time.Hour); got != OnceOutcomeCanceled {
			t.Errorf("Wait() = %v, want %v", got, OnceOutcomeCanceled)
		}
	})
}

// OnceOutcome.String がログに出す名前を持つこと（運用側が
// 「1 件やって終わった」と「空振りで終わった」を区別する手段）。
func TestOnceOutcome_String(t *testing.T) {
	for _, tt := range []struct {
		outcome OnceOutcome
		want    string
	}{
		{OnceOutcomeJobDone, "job_done"},
		{OnceOutcomeIdleTimeout, "idle_timeout"},
		{OnceOutcomeCanceled, "canceled"},
		{OnceOutcome(99), "unknown"},
	} {
		if got := tt.outcome.String(); got != tt.want {
			t.Errorf("OnceOutcome(%d).String() = %q, want %q", tt.outcome, got, tt.want)
		}
	}
}

// 1 件消化モードでは、そのキューの MaxWorkers が config の concurrency に
// 関わらず 1 になること（KEDA 側が「1 Job = 1 アイテム」で数を合わせている）。
// middleware が 1 本入ることもここで見る（入らないと終了の契機が無い）。
func TestBuildRiverConfig_OnceModeForcesSingleWorker(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		BoundSite:         "tokyo",
		Queues:            []string{ingestQueue},
		IngestConcurrency: 4,
		Once:              NewOnceGate(),
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	qc, ok := riverCfg.Queues["ingest_tokyo"]
	if !ok {
		t.Fatalf("Queues = %v, want to contain ingest_tokyo", riverCfg.Queues)
	}
	if qc.MaxWorkers != 1 {
		t.Errorf("ingest_tokyo MaxWorkers = %d, want 1 (once モードは同時 claim を 1 件に抑える)", qc.MaxWorkers)
	}
	if len(riverCfg.Middleware) != 1 {
		t.Errorf("Middleware = %d 件, want 1 (OnceGate の middleware が終了の契機)", len(riverCfg.Middleware))
	}
}

// 反対方向: 1 件消化モードでなければ concurrency はそのまま効き、middleware も
// 入らないこと（once モードの強制が常時 1 に潰していないことの確認）。
func TestBuildRiverConfig_WithoutOnceMode_KeepsConcurrency(t *testing.T) {
	riverCfg, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		BoundSite:         "tokyo",
		Queues:            []string{ingestQueue},
		IngestConcurrency: 4,
	})
	if err != nil {
		t.Fatalf("buildRiverConfig: %v", err)
	}
	if got := riverCfg.Queues["ingest_tokyo"].MaxWorkers; got != 4 {
		t.Errorf("ingest_tokyo MaxWorkers = %d, want 4", got)
	}
	if len(riverCfg.Middleware) != 0 {
		t.Errorf("Middleware = %d 件, want 0", len(riverCfg.Middleware))
	}
}

// 1 件消化モードはちょうど 1 キューを要求すること。ScaledJob はキュー単位に
// 作る（docs/operations.md §5）ので複数キューに対応する定義が無く、River の
// MaxWorkers はキュー単位なので複数キューでは「同時 claim は 1 件」も
// 成立しない。**既定（空 = 全キュー）も弾く**のが要点 --- 弾かないと
// `--queues` を書き忘れた ScaledJob が全キューを引く。
func TestBuildRiverConfig_OnceModeRejectsNonSingleQueue(t *testing.T) {
	for _, tt := range []struct {
		name   string
		queues []string
	}{
		{"空（= 全キュー）", nil},
		{"2 キュー", []string{ingestQueue, encodeQueue}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
				BoundSite: "tokyo",
				Queues:    tt.queues,
				Once:      NewOnceGate(),
			})
			if err == nil {
				t.Fatal("error を期待したが nil だった")
			}
			if !strings.Contains(err.Error(), "exactly one queue") {
				t.Errorf("err = %v, want to mention \"exactly one queue\"", err)
			}
		})
	}
}

// 1 件消化モードで worker.periodic_jobs が true なら起動エラーになること。
// 1 件で終わる Job がリーダーになると、定期投入の間隔が KEDA のスケール挙動
// （Job の起動回数）で決まってしまう。
func TestBuildRiverConfig_OnceModeRejectsPeriodicJobs(t *testing.T) {
	_, err := buildRiverConfig(NewWorkers(&Deps{}), ClientConfig{
		BoundSite:    "tokyo",
		Queues:       []string{ingestQueue},
		PeriodicJobs: true,
		EpgSyncSite:  "tokyo",
		Once:         NewOnceGate(),
	})
	if err == nil {
		t.Fatal("error を期待したが nil だった")
	}
	if !strings.Contains(err.Error(), "periodic_jobs") {
		t.Errorf("err = %v, want to mention worker.periodic_jobs", err)
	}
}
