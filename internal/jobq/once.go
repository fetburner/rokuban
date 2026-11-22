package jobq

// OnceOutcome は 1 件消化モードが待機を終えた理由。
// 起動側が River adapter の型を参照せずに結果を受け取り、asynq adapter も
// 同じ型を返せるように、エンジン共通の jobq に置く。
//
// **どの理由でも呼び出し側は exit 0 にする**（ジョブの失敗はエンジンの
// リトライが引き取るので、プロセスの終了コードで表さない。River adapter の
// Config.Once のコメント参照）。この値は「なぜ終わったか」をログに残すためだけにある ---
// 「1 件やって終わった」と「空振りで終わった」を運用側で区別できないと、
// KEDA が Job を起こし続けているのに仕事が進まない状態を見分けられない。
type OnceOutcome int

const (
	// OnceOutcomeJobDone は 1 件のジョブが Work を抜けたことを表す
	// （成功・失敗のどちらも含む）。
	OnceOutcomeJobDone OnceOutcome = iota

	// OnceOutcomeIdleTimeout はジョブを 1 件も claim しないまま待ち時間を
	// 使い切ったことを表す。KEDA が滞留を過大評価して Job を起こしすぎた
	// （overshoot した）ときにこちらへ落ちる。
	OnceOutcomeIdleTimeout

	// OnceOutcomeCanceled は ctx が終了したことを表す（SIGTERM / ノード退避）。
	OnceOutcomeCanceled

	// OnceOutcomeJobUnhandled はジョブが終端に達したが **worker に入らなかった**
	// ことを表す。River では登録されていない kind のジョブを掴んだ場合に起きる
	// （River adapter の subscribeOnceEvents のコメント）。
	//
	// job_done と分けているのは、これが「仕事をした」ではなく「掴んで失敗させ、
	// 試行回数を 1 つ潰した」だからである。CronJob や api が新しいイメージで、
	// worker が古いイメージという版ずれで起きうるので、KEDA が Job を起こし
	// 続けているのに何も進まない状態をログで名指しできるようにする。
	OnceOutcomeJobUnhandled
)

// String は OnceOutcome のログ用の名前を返す。
func (o OnceOutcome) String() string {
	switch o {
	case OnceOutcomeJobDone:
		return "job_done"
	case OnceOutcomeIdleTimeout:
		return "idle_timeout"
	case OnceOutcomeCanceled:
		return "canceled"
	case OnceOutcomeJobUnhandled:
		return "job_unhandled"
	default:
		return "unknown"
	}
}
