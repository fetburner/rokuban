package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/fetburner/rokuban/internal/testutil"
)

// このファイルは river@v0.40.0 を対象に、SubscribeOnceEvents の doc コメントが
// 依拠する前提のうち「終端は completed/failed/cancelled/snoozed の 4
// EventKind で網羅される」を実 River client + 実 DB で固定する（issue #518）。
//
// もう一つの前提（未登録 kind では WorkerMiddleware が一度も呼ばれない）は
// cmd/rokuban.TestServerCmd_OnceModeExitsOnUnhandledJobKind が本番と同じ
// 配線（RunE 経由。試行回数が 1 回しか潰れないことも見ている）で既に
// 固定済みなので、ここには弱い重複を足さない。
//
// once_test.go 側は OnceGate.wait/waitDone のロジックを合成イベントで
// 検証しているが、「その合成イベントが実際に届く形と一致しているか」は
// River 側の実装に依っており、River の更新でここが崩れてもコンパイルも
// 既存 CI も緑のままになりうる。ここでは river.NewClient に実ワーカーを
// 登録し、実際に Work を走らせて得た本物の *river.Event で同じ経路を通す。

// onceRiverOutcome は Wait を別 goroutine で回し、OnceOutcome と、events から
// 実際に読んだ最初の *river.Event の Kind を一緒に返す。
//
// **events を素通しの 1 段 tee で経由させる。** Wait 自身は読んだ Event の
// Kind を外に返さないので、「期待した EventKind が実際に届いたか」
// （例: river.JobCancel が退化して JobFailed になっていないか）を見るには、
// Wait に渡す前に 1 度だけ中継してその値を控える必要がある。
//
// 控えた Kind は変数ではなくバッファ付きチャネルで返す。Wait が
// idleTimeout 側で戻ると tee は読まれないまま中継 goroutine だけが走り続け、
// 共有変数だと戻り値の読み出しと競合する（go test -race で落ちる）。
func onceRiverOutcome(t *testing.T, gate *OnceGate, idleTimeout time.Duration, events <-chan *river.Event) (OnceOutcome, river.EventKind) {
	t.Helper()

	tee := make(chan *river.Event, 1)
	kinds := make(chan river.EventKind, 1)
	go func() {
		defer close(tee)
		defer close(kinds)
		if ev, ok := <-events; ok {
			kinds <- ev.Kind
			tee <- ev
		}
	}()

	out := make(chan OnceOutcome, 1)
	go func() { out <- gate.Wait(context.Background(), idleTimeout, tee) }()
	limit := idleTimeout + 5*time.Second
	select {
	case o := <-out:
		// 中継 goroutine は Wait が events 分岐で戻ったときには必ず
		// kinds へ送り終えている（tee より先に送るため）。idleTimeout で
		// 戻った場合はまだ <-events で止まっているので、ここは塞がずに
		// ゼロ値を返す（Kind の assertion 側が期待値との差で落ちる）。
		select {
		case k := <-kinds:
			return o, k
		default:
			return o, ""
		}
	case <-time.After(limit):
		t.Fatalf("Wait が %s 以内に戻らない（1 件消化モードの Job が終了しない形）", limit)
		return OnceOutcomeCanceled, ""
	}
}

// onceEventControlArgs は Work の終わり方を呼び出し側から選べる、
// このテストファイル専用のジョブ。
// TestSubscribeOnceEvents_AllTerminalKindsEndWaitViaEventsWithoutMiddleware が
// completed/failed/cancelled/snoozed の 4 通りを実際に起こすのに使う。
type onceEventControlArgs struct {
	// Outcome は "complete" / "fail" / "cancel" / "snooze" のいずれか。
	Outcome string `json:"outcome"`
}

func (onceEventControlArgs) Kind() string { return "once_river_test_control" }

type onceEventControlWorker struct {
	river.WorkerDefaults[onceEventControlArgs]
}

func (w *onceEventControlWorker) Work(_ context.Context, job *river.Job[onceEventControlArgs]) error {
	switch job.Args.Outcome {
	case "complete":
		return nil
	case "fail":
		return errors.New("once_river_test: intentional failure")
	case "cancel":
		return river.JobCancel(errors.New("once_river_test: intentional cancel"))
	case "snooze":
		return river.JobSnooze(10 * time.Millisecond)
	default:
		return errors.New("once_river_test: unknown outcome " + job.Args.Outcome)
	}
}

// **completed/failed/cancelled/snoozed の 4 EventKind すべてが
// SubscribeOnceEvents 経由で届くことを、middleware に頼らず固定する。**
//
// **この配線は本番ではない。** 本番（buildRiverConfig）は cfg.Once != nil
// のとき常に OnceGate.Middleware() も一緒に登録し、登録済み kind は
// WorkUnit != nil なので JobExecutor は必ず execution.MiddlewareChain を
// 組み立てて doInner（= Work）を呼ぶ --- つまり本番では、ここで動かす
// 4 通りのどの終端でも WorkerMiddleware が先に g.done を閉じて Job を
// 終わらせてしまい、events 側の購読が 1 つ欠けていても隠れてしまう
// （SubscribeOnceEvents のコメントの「本番で唯一効いているのは
// EventKindJobFailed だけ」参照）。ここでは middleware をあえて
// river.Config に登録せず、g.started/g.done を一生閉じない配線にすることで、
// 「4 kind それぞれが events だけで Wait を終わらせられるか」という
// 保険側の主張を、middleware という別の検出経路に隠されずに検証する。
//
// gate.started は一度も閉じないので、正しい OnceOutcome は job_unhandled
// になる（waitDone には入らない。この配線で見ているのは「Wait が events
// 分岐で戻ること」であって「waitDone が job_done を返すこと」ではない）。
//
// このテストが検出すべき変異: SubscribeOnceEvents から 4 kind のいずれか
// 1 つを落とす。その kind に対応する subtest だけが idle_timeout まで
// 戻らなくなり、outcome が job_unhandled ではなく idle_timeout になる
// （このファイル内では他の 3 subtest は緑のまま。dropping
// EventKindJobCompleted / EventKindJobFailed / EventKindJobCancelled /
// EventKindJobSnoozed をそれぞれ試し、対応する subtest だけが落ちることを
// 確認済み。ただし EventKindJobFailed を落とすと
// cmd/rokuban.TestServerCmd_OnceModeExitsOnUnhandledJobKind も一緒に赤くなる
// --- それが本番で唯一効いている購読だから。SubscribeOnceEvents のコメント
// 参照）。
func TestSubscribeOnceEvents_AllTerminalKindsEndWaitViaEventsWithoutMiddleware(t *testing.T) {
	for _, tt := range []struct {
		outcomeArg string
		wantKind   river.EventKind
	}{
		{"complete", river.EventKindJobCompleted},
		{"fail", river.EventKindJobFailed},
		{"cancel", river.EventKindJobCancelled},
		{"snooze", river.EventKindJobSnoozed},
	} {
		t.Run(tt.outcomeArg, func(t *testing.T) {
			pool := testutil.SetupDB(t)
			ctx := context.Background()

			workers := river.NewWorkers()
			river.AddWorker(workers, &onceEventControlWorker{})

			client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
				Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
				Workers: workers,
			})
			if err != nil {
				t.Fatalf("creating client: %v", err)
			}

			gate := NewOnceGate()
			events, unsubscribe := SubscribeOnceEvents(client)
			defer unsubscribe()

			clientCtx, clientCancel := context.WithCancel(ctx)
			defer clientCancel()
			if err := client.Start(clientCtx); err != nil {
				t.Fatalf("starting client: %v", err)
			}
			defer func() {
				clientCancel()
				<-client.Stopped()
			}()

			if _, err := client.Insert(ctx, onceEventControlArgs{Outcome: tt.outcomeArg}, nil); err != nil {
				t.Fatalf("inserting job: %v", err)
			}

			const idleTimeout = 2 * time.Second
			outcome, gotKind := onceRiverOutcome(t, gate, idleTimeout, events)

			if outcome != OnceOutcomeJobUnhandled {
				t.Errorf("Wait() = %v, want %v（events だけで終わっているはず。"+
					"middleware は登録していない）", outcome, OnceOutcomeJobUnhandled)
			}
			if gotKind != tt.wantKind {
				t.Errorf("受け取った Event.Kind = %q, want %q", gotKind, tt.wantKind)
			}
		})
	}
}
