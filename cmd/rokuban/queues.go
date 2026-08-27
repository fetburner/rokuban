package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/worker"
)

const (
	// queuesFlagName は worker ロールが引くキューを絞る起動形態フラグ名。
	//
	// **config キー（`worker.queues`）と同じものを CLI からも指定できるようにする
	// のが目的**（設定値を CLI で渡すことの一般的な許可ではない。
	// docs/configuration.md §やらないこと）。k8s のロール分割では ConfigMap は
	// 1 個で全 Pod が同じ config.yml を共有し、**Pod ごとに違うのは argv だけ**に
	// する（docs/operations.md §マニフェストの配布形式）。ScaledJob はキュー単位
	// に作るので、キューを config キーでしか指定できないと ScaledJob の数だけ
	// ConfigMap が増え、その決定が崩れる。`internal/config` 自身も
	// `worker.queues` を「デプロイ時のパラメータであって site の属性ではない」と
	// 位置づけている（docs/configuration.md §スキーマ構造）。
	queuesFlagName = "queues"

	// onceFlagName は 1 件消化モードのフラグ名。
	//
	// KEDA ScaledJob は Job の自己終了を前提にした機構なので、`--roles worker` の
	// まま載せると Job が succeeded に到達しない（docs/operations.md §5）。
	onceFlagName = "once"

	// onceIdleTimeoutFlagName は 1 件消化モードで「1 件も claim できないまま
	// 待つ」上限のフラグ名。
	//
	// **名前に idle を入れているのは、実行中のジョブには効かないことを名前で
	// 示すため。** `--once-timeout` だと「Job 全体の制限時間」と読めてしまい、
	// 数時間のエンコードを打ち切る設定だと誤解されうる（打ち切らないことが
	// ScaledJob を選んだ理由そのもの。worker.OnceGate のコメント参照）。
	onceIdleTimeoutFlagName = "once-idle-timeout"

	// softStopTimeoutFlagName は SIGTERM を受けてから実行中のジョブを打ち切る
	// までの猶予（River の `SoftStopTimeout`）のフラグ名。
	//
	// **config キーにしない。** k8s では ConfigMap は 1 個で全 Pod が同じ
	// config.yml を共有する（docs/operations.md §マニフェストの配布形式）一方、
	// この値は**ワークロードごとに桁で違う** --- 数時間の encode を載せる
	// ScaledJob と、数秒で終わる ruler / reconciler では、対になる
	// `terminationGracePeriodSeconds` が桁で違う（docs/operations.md §5
	// 「長時間ジョブと短いジョブを混ぜない」）。config キーにすると、その
	// 桁の違いを ConfigMap を増やさずには表現できない。`--queues` を argv に
	// 寄せたのと同じ理由である。
	//
	// **対になる k8s 側の値と同じ Pod spec に並べて書けることが、この選択の
	// 実益**でもある。真の上限は `terminationGracePeriodSeconds` 経過後の
	// SIGKILL であり、それを超える猶予は実現されない（cmd/rokuban/server.go の
	// shutdownBudget のコメント参照）。
	softStopTimeoutFlagName = "soft-stop-timeout"

	// defaultOnceIdleTimeout は onceIdleTimeoutFlagName の既定値。
	//
	// この値が効くのは KEDA が滞留を過大評価して Job を起こしすぎた
	// （overshoot した）ときだけで、そのとき Job は仕事を掴めないまま
	// maxReplicaCount の枠を占める。短すぎると起動しただけの Job が
	// 増え、長すぎるとその枠が空かない。
	//
	// 30 秒という値は、River の fetch 周期（river.FetchPollIntervalDefault =
	// 1 秒）の数十倍という相対関係だけを根拠にしている。**コンテナ起動から
	// 最初の fetch までに実際どれだけ掛かるかは未計測**（DB 接続と
	// マイグレーション検査を含むため 1 秒では終わらない、という以上のことは
	// 測っていない）。
	defaultOnceIdleTimeout = 30 * time.Second
)

// resolveWorkerQueues は `--queues` フラグと `worker.queues` から、この
// プロセスが引くキューの論理名を決める。
//
// 解決規則:
//   - フラグ未指定 → config の値をそのまま使う（空なら全キュー）
//   - `--queues ingest` 等 → その集合を使う（重複は 1 つに畳む）
//   - `--queues=`（明示的な空）→ 起動エラー。「全キュー」を意味させない ---
//     ScaledJob の argv で値が空になる事故（テンプレートの変数未展開など）が
//     「全キューを引く Pod」に化けると、site 束縛キューまで掴んで
//     verifySite で全滅する構成が黙って生まれる
//   - フラグと config の両方が非空 → 起動エラー（下記）
//   - worker ロールが無い → 起動エラー。キューを引くのは worker ロールだけ
//     （`--roles api --queues=encode` は encode の ScaledJob の argv からの
//     写し間違いで起きうる形で、黙って何もしない Pod になる）。
//     `--once-idle-timeout` を `--once` 無しで書いたときと同じ扱いにする ---
//     効かないフラグを黙って無視しない
//
// **両方指定を勝ち負けで解決しない。** 黙って片方が勝つと、monolith
// （config だけ）と k8s（argv だけ）で購読集合の出所が分かれ、「config を直したのに
// 効かない」を起動ログ以外から知る手段が無くなる。fail-fast で落とす
// （docs/operations.md §DB 接続失敗と同じ方針）。
func resolveWorkerQueues(cmd *cobra.Command, configured []string, roles []string) ([]string, error) {
	if !cmd.Flags().Changed(queuesFlagName) {
		return configured, nil
	}
	names, err := cmd.Flags().GetStringSlice(queuesFlagName)
	if err != nil {
		return nil, err
	}
	// **検査の順序は「フラグが効くか → 値が妥当か → config と衝突しないか」。**
	// 逆順にすると、直すべき点と違うものを報告する --- 排他を先に見ると
	// `--roles api --queues=encode`（+ config 指定）が「排他」と言われ、
	// 空の検査を後に回すと「flag:  / config: ruler」という値の抜けたエラーになる。
	if !slices.Contains(roles, "worker") {
		return nil, fmt.Errorf("--%s has no effect without the worker role (got roles [%s]): "+
			"only the worker role pulls queues", queuesFlagName, strings.Join(roles, ", "))
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("--%s: pass at least one queue name (valid: %s); "+
			"an empty value is rejected because it would silently mean \"all queues\"",
			queuesFlagName, strings.Join(worker.AllQueueNames(), ", "))
	}
	if len(configured) > 0 {
		return nil, fmt.Errorf("--%s and worker.queues are mutually exclusive "+
			"(flag: %s / config: %s); the deployment must own exactly one of them",
			queuesFlagName, strings.Join(names, ", "), strings.Join(configured, ", "))
	}

	// 未知の名前はここで弾く（buildRiverConfig にも同じ検査があるが、そちらの
	// エラーは `worker.queues:` と名乗るので、argv から来た値の出自が分からなく
	// なる）。同じ名前の重複は 1 つに畳む --- `--queues ingest,ingest` が
	// 1 件消化モードの「ちょうど 1 キュー」検査に 2 キューとして映らないように
	// するため（resolveSiteBinding が `--sites tokyo,tokyo` を畳むのと同じ理由）。
	valid := worker.AllQueueNames()
	resolved := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	var unknown []string
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if !slices.Contains(valid, n) {
			unknown = append(unknown, n)
			continue
		}
		resolved = append(resolved, n)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("--%s: unknown queue(s) %s (valid: %s)",
			queuesFlagName, strings.Join(unknown, ", "), strings.Join(valid, ", "))
	}
	return resolved, nil
}

// validateOnceMode は 1 件消化モードのロール集合を検査する。
//
// **worker ロール単独に限る。** 1 件消化モードはプロセスを畳むことが仕事なので、
// 常駐が前提の役（api / notifier / streamer は接続を受け続け、watcher は
// mirakc の SSE を張り続ける）と同居させると、ジョブ 1 件でその役ごと落ちる。
// `--all` もここで弾かれる。
func validateOnceMode(roles []string) error {
	if len(roles) != 1 || roles[0] != "worker" {
		return fmt.Errorf("--%s: requires exactly the worker role (--roles worker), got [%s]: "+
			"once mode exits after one job, which would take down long-lived roles with it",
			onceFlagName, strings.Join(roles, ", "))
	}
	return nil
}

// resolveOnce は `--once` / `--once-idle-timeout` を解決する。1 件消化モードで
// なければ (nil, 0, nil) を返す。
func resolveOnce(cmd *cobra.Command, roles []string) (*worker.OnceGate, time.Duration, error) {
	once, err := cmd.Flags().GetBool(onceFlagName)
	if err != nil {
		return nil, 0, err
	}
	if !once {
		// **効かないフラグを黙って無視しない。** ScaledJob の argv に
		// `--once-idle-timeout` だけ書いて `--once` を書き忘れると、Job は
		// 永久に終わらないのに argv は「1 件で終わるつもり」に見える ---
		// 症状は「KEDA の Job が succeeded にならない」で、argv を読んでも
		// 気付けない。
		if cmd.Flags().Changed(onceIdleTimeoutFlagName) {
			return nil, 0, fmt.Errorf("--%s has no effect without --%s", onceIdleTimeoutFlagName, onceFlagName)
		}
		return nil, 0, nil
	}
	if err := validateOnceMode(roles); err != nil {
		return nil, 0, err
	}
	idleTimeout, err := resolveOnceIdleTimeout(cmd)
	if err != nil {
		return nil, 0, err
	}
	return worker.NewOnceGate(), idleTimeout, nil
}

// stopOnceProcess は 1 件消化モードの停止手順。
//
// **順序が意味を持つので 1 か所にまとめる。** stopRiver（riverClient.Stop）は
// producer の fetch を止めて実行中のジョブを待つ graceful stop、cancelProcess
// （signal.NotifyContext の stop）は Start に渡した ctx の cancel である。
//
// **順序の理由は SoftStopTimeout の導入で変わった（危険度が下がった）。**
// かつては work ctx が start ctx を継いでいたため、cancelProcess が
// StopAndCancel 相当のハードストップになり、逆順にすると実行中のジョブが
// **即座に**打ち切られた。いまは work ctx が start ctx から切り離されるので
// （river@v0.40.0/client.go の workParentCtx。SoftStopTimeout > 0 のとき）、
// 逆順でも打ち切りは soft stop の猶予まで遅れる。それでも順序はこのままにする
// --- graceful stop を先に撃つほうが、猶予を消費せずに完走できる。
//
// stopRiver に渡す ctx は cancel されないものにする。**この呼び出し自身に上限を
// 置かない**のは、「実行中のジョブを打ち切らない」が ScaledJob を選んだ理由その
// ものだからである（常駐 worker が持つ RunE の `stopRiverForShutdown` は、
// 1 件消化モードでは先にこちらが完了するため到達しない）。
//
// **実際の上限は `--soft-stop-timeout` の猶予である。** 猶予が切れれば River が
// work ctx を cancel するので、掴んでいたジョブは打ち切られる ---
// 数時間のエンコードを載せるキューでは、既定の 30 秒では足りない
// （docs/operations.md §5「Deployment 併用時」の対で引き上げる指針）。
// **River が保証するのは work ctx の cancel までで、`Stop` が戻ることまでは
// 保証しない**（ctx を見ないワーカーは止まらず、completer の停止待ちにも上限が
// 無い。river@v0.40.0/client.go の `StopAllParallel` 直前の TODO）。最終的な
// 上限は k8s の `terminationGracePeriodSeconds` 経過後の SIGKILL になる。
//
// エラーを握り潰さないのは作法として。現在の呼び出しでは ctx が cancel
// されないので `Stop` は nil しか返さない（`Stop` が非 nil を返すのは
// 渡した ctx が終わったときだけ）。
//
// 順序と ctx の扱いは TestStopOnceProcess が固定する。**振る舞いのテストでは
// 守れない** --- 実行中のジョブが残る窓は実測 0/25 で踏めなかったので、順序を
// 入れ替えても、この関数の呼び出し自体を `stop()` 1 行に置き換えても、
// 実 DB のテストは緑のままである。
func stopOnceProcess(ctx context.Context, stopRiver func(context.Context) error, cancelProcess func()) {
	if err := stopRiver(context.WithoutCancel(ctx)); err != nil {
		slog.Error("stopping river client (once mode)", "err", err)
	}
	cancelProcess()
}

// resolveSoftStopTimeout は SIGTERM を受けてから実行中のジョブを打ち切るまでの
// 猶予（worker.ClientConfig.SoftStopTimeout）を決める。
//
// 検査は 2 つで、どちらも「効かない / 意味が反転する値」を黙って受け取らない
// ためにある（`--queues` / `--once-idle-timeout` と同じ方針）:
//
//   - worker ロールが無いプロセスでの指定は起動エラー。River クライアントを
//     Start するのは worker ロールだけなので（resolveRiverClientKind）、
//     `--roles watcher --soft-stop-timeout 5m` は何も待たずに畳む。
//   - 0 以下は起動エラー。**0 は「無制限」ではなく「待たない」である** ---
//     River は SoftStopTimeout が 0 のとき work ctx を start ctx から継ぐので、
//     `--soft-stop-timeout 0` は SIGTERM を StopAndCancel 相当にする。
//     「上限を外したい」つもりで書いた 0 が、意図と正反対の
//     「実行中のジョブを即座に打ち切る」になる。
func resolveSoftStopTimeout(cmd *cobra.Command, roles []string) (time.Duration, error) {
	d, err := cmd.Flags().GetDuration(softStopTimeoutFlagName)
	if err != nil {
		return 0, err
	}
	if cmd.Flags().Changed(softStopTimeoutFlagName) && !slices.Contains(roles, "worker") {
		return 0, fmt.Errorf("--%s has no effect without the worker role (got roles [%s]): "+
			"only the worker role runs jobs to drain", softStopTimeoutFlagName, strings.Join(roles, ", "))
	}
	if d <= 0 {
		return 0, fmt.Errorf("--%s must be positive, got %s: "+
			"zero does not mean \"wait forever\" --- River inherits the work context from the "+
			"start context when the soft stop timeout is unset, which makes SIGTERM cut running jobs immediately",
			softStopTimeoutFlagName, d)
	}
	return d, nil
}

// resolveOnceIdleTimeout は 1 件消化モードの未 claim 待ち上限を返す。
func resolveOnceIdleTimeout(cmd *cobra.Command) (time.Duration, error) {
	d, err := cmd.Flags().GetDuration(onceIdleTimeoutFlagName)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		// 0 や負を「無制限」に読み替えない。無制限は「空振りの Job が
		// maxReplicaCount の枠を握ったまま残る」形なので、意図して選ぶなら
		// 明示的に長い値を書くべきである。
		return 0, fmt.Errorf("--%s must be positive, got %s", onceIdleTimeoutFlagName, d)
	}
	return d, nil
}
