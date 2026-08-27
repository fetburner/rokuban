package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/fetburner/rokuban/internal/worker"
)

// workerRole は resolveWorkerQueues に渡す既定のロール集合（キューを引くのは
// worker ロールだけなので、キュー解決のテストはこれを前提にする）。
var workerRole = []string{"worker"}

func newQueuesTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringSlice(queuesFlagName, nil, "")
	cmd.Flags().Bool(onceFlagName, false, "")
	cmd.Flags().Duration(onceIdleTimeoutFlagName, defaultOnceIdleTimeout, "")
	cmd.Flags().Duration(softStopTimeoutFlagName, worker.DefaultSoftStopTimeout, "")
	return cmd
}

// --queues 未指定なら config の worker.queues がそのまま使われること
// （既存構成の挙動を変えない）。
func TestResolveWorkerQueues_UnspecifiedUsesConfig(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	got, err := resolveWorkerQueues(cmd, []string{"ruler", "reconciler"}, workerRole)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(got, ",") != "ruler,reconciler" {
		t.Errorf("queues = %v, want [ruler reconciler]", got)
	}

	// config も空なら空（= 全キュー）のまま。
	empty, err := resolveWorkerQueues(newQueuesTestCmd(t), nil, workerRole)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("queues = %v, want empty (既定は全キュー)", empty)
	}
}

// --queues で指定した集合が使われること（config が空のとき）。
func TestResolveWorkerQueues_FlagWins(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, "encode"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := resolveWorkerQueues(cmd, nil, workerRole)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(got, ",") != "encode" {
		t.Errorf("queues = %v, want [encode]", got)
	}
}

// 同じ名前の重複は 1 つに畳むこと。1 件消化モードの「ちょうど 1 キュー」検査に
// `--queues ingest,ingest` が 2 キューとして映らないようにするため
// （resolveSiteBinding が --sites の重複を畳むのと同じ理由）。
func TestResolveWorkerQueues_DuplicatesAreFolded(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, "ingest,ingest"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := resolveWorkerQueues(cmd, nil, workerRole)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "ingest" {
		t.Errorf("queues = %v, want [ingest]", got)
	}
}

// **--queues と worker.queues の両方指定は起動エラー。** 黙って片方が勝つと、
// monolith（config だけ）と k8s（argv だけ）で購読集合の出所が分かれ、
// 「config を直したのに効かない」を起動ログ以外から知る手段が無くなる。
func TestResolveWorkerQueues_BothSpecified_IsError(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, "encode"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, err := resolveWorkerQueues(cmd, []string{"ruler"}, workerRole)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want to mention mutually exclusive", err)
	}
}

// --queues=（明示的な空）は「全キュー」にせず起動エラーにすること。
// ScaledJob の argv でテンプレート変数が未展開になった事故が
// 「全キューを引く Pod」に化けないようにするため。
func TestResolveWorkerQueues_ExplicitEmpty_IsError(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, err := resolveWorkerQueues(cmd, nil, workerRole)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "at least one queue") {
		t.Errorf("err = %v, want to mention \"at least one queue\"", err)
	}
}

// 未知のキュー名は起動エラーにすること（typo で静かに何も引かなくなる事故を
// 防ぐ。エラーは argv 由来だと分かる形で出す）。
func TestResolveWorkerQueues_UnknownQueue_IsError(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, "encode,bogus"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, err := resolveWorkerQueues(cmd, nil, workerRole)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "--"+queuesFlagName) {
		t.Errorf("err = %v, want to mention the unknown name and --%s", err, queuesFlagName)
	}

	// 未知の名前も重複を畳む（`bogus, bogus` と並べない）。
	cmd = newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, "bogus,bogus"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, err = resolveWorkerQueues(cmd, nil, workerRole)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "bogus, bogus") {
		t.Errorf("err = %v, want the unknown list folded", err)
	}
}

// **worker ロールが無いのに --queues を渡したら起動エラー。** キューを引くのは
// worker ロールだけなので、`--roles api --queues=encode`（encode の ScaledJob の
// argv からの写し間違い）は黙って何もしない Pod になる。`--once-idle-timeout` を
// `--once` 無しで書いたときと同じ扱いにする。
func TestResolveWorkerQueues_WithoutWorkerRole_IsError(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, "encode"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, err := resolveWorkerQueues(cmd, nil, []string{"api"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no effect without the worker role") {
		t.Errorf("err = %v, want to mention the missing worker role", err)
	}

	// --all 相当（worker を含む）なら通る。
	cmd = newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, "encode"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := resolveWorkerQueues(cmd, nil, allRoles); err != nil {
		t.Errorf("unexpected error for --all: %v", err)
	}
}

// worker ロールが無く、かつ config も指定されている場合は **ロールの方**を
// 報告すること。排他を先に見ると「排他」と言われ、直すべき点（ロール）が
// 出力から読めない。
func TestResolveWorkerQueues_WithoutWorkerRoleAndConfig_ReportsRole(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, "encode"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, err := resolveWorkerQueues(cmd, []string{"ruler"}, []string{"api"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no effect without the worker role") {
		t.Errorf("err = %v, want the role error（排他エラーではなく）", err)
	}
}

// `--queues=`（明示的な空）と config が同時に指定された場合、**空の方**を
// 報告すること。順序を逆にすると「flag:  / config: ruler」という値の抜けた
// 排他エラーになり、何を直せばよいか読めない。
func TestResolveWorkerQueues_ExplicitEmptyWithConfig_ReportsEmpty(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, err := resolveWorkerQueues(cmd, []string{"ruler"}, workerRole)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "at least one queue") {
		t.Errorf("err = %v, want the empty-value error（排他エラーではなく）", err)
	}

	// worker ロールが無い場合は、空より先にロールを報告する（検査の順序は
	// 「フラグが効くか → 値が妥当か → config と衝突しないか」）。
	cmd = newQueuesTestCmd(t)
	if err := cmd.Flags().Set(queuesFlagName, ""); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := resolveWorkerQueues(cmd, nil, []string{"api"}); err == nil {
		t.Fatal("expected error, got nil")
	} else if !strings.Contains(err.Error(), "no effect without the worker role") {
		t.Errorf("err = %v, want the role error", err)
	}
}

// `--roles worker,worker` の重複が畳まれること。畳まないと --once の
// 「ちょうど worker 1 つ」検査が引っかかり、紛らわしいエラーになる
// （プール上限は db.maxConnsForRoles が元から畳んでいるので無関係）。
func TestResolveRoles_FoldsDuplicates(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().Bool("all", false, "")
	cmd.Flags().StringSlice("roles", nil, "")
	if err := cmd.Flags().Set("roles", "worker,worker"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	roles, err := resolveRoles(cmd)
	if err != nil {
		t.Fatalf("resolveRoles: %v", err)
	}
	if len(roles) != 1 || roles[0] != "worker" {
		t.Errorf("roles = %v, want [worker]", roles)
	}
	if err := validateOnceMode(roles); err != nil {
		t.Errorf("validateOnceMode(%v) = %v, want nil", roles, err)
	}
}

// 1 件消化モードは worker ロール単独に限ること。常駐が前提の役と同居させると
// ジョブ 1 件でその役ごと落ちる。
func TestValidateOnceMode(t *testing.T) {
	tests := []struct {
		name    string
		roles   []string
		wantErr bool
	}{
		{"worker 単独", []string{"worker"}, false},
		{"worker + api", []string{"worker", "api"}, true},
		{"--all 相当", allRoles, true},
		{"watcher 単独", []string{"watcher"}, true},
		{"ロールなし", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateOnceMode(tt.roles)
			if tt.wantErr && err == nil {
				t.Errorf("validateOnceMode(%v) = nil, want error", tt.roles)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateOnceMode(%v) = %v, want nil", tt.roles, err)
			}
		})
	}
}

// 未 claim 待ちの上限は既定 30 秒で、明示指定が効き、0 / 負は起動エラーになること
// （0 を「無制限」に読み替えない --- 空振りの Job が maxReplicaCount の枠を
// 握ったまま残る形を暗黙に選ばせない）。
func TestResolveOnceIdleTimeout(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	got, err := resolveOnceIdleTimeout(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 30*time.Second {
		t.Errorf("既定 = %s, want 30s", got)
	}

	cmd = newQueuesTestCmd(t)
	if err := cmd.Flags().Set(onceIdleTimeoutFlagName, "5m"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err = resolveOnceIdleTimeout(cmd); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if got != 5*time.Minute {
		t.Errorf("--%s=5m -> %s, want 5m", onceIdleTimeoutFlagName, got)
	}

	for _, v := range []string{"0", "-1s"} {
		cmd = newQueuesTestCmd(t)
		if err := cmd.Flags().Set(onceIdleTimeoutFlagName, v); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if _, err := resolveOnceIdleTimeout(cmd); err == nil {
			t.Errorf("--%s=%s: expected error, got nil", onceIdleTimeoutFlagName, v)
		}
	}
}

// resolveOnce: --once が無ければ gate を作らないこと、あれば作ること、
// そして **--once-idle-timeout だけを書いた argv を弾くこと**。
// 弾かないと、Job は永久に終わらないのに argv は「1 件で終わるつもり」に見える。
func TestResolveOnce(t *testing.T) {
	// --once 無し: gate なし
	cmd := newQueuesTestCmd(t)
	gate, idle, err := resolveOnce(cmd, []string{"worker"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gate != nil || idle != 0 {
		t.Errorf("gate = %v, idle = %s, want nil / 0", gate, idle)
	}

	// --once あり: gate と既定のタイムアウト
	cmd = newQueuesTestCmd(t)
	if err := cmd.Flags().Set(onceFlagName, "true"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	gate, idle, err = resolveOnce(cmd, []string{"worker"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gate == nil {
		t.Error("gate = nil, want non-nil")
	}
	// **実装の定数と比べない**（定数を変えても通るテストになる）。
	if idle != 30*time.Second {
		t.Errorf("idle = %s, want 30s", idle)
	}

	// --once-idle-timeout だけ: 起動エラー
	cmd = newQueuesTestCmd(t)
	if err := cmd.Flags().Set(onceIdleTimeoutFlagName, "5s"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_, _, err = resolveOnce(cmd, []string{"worker"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "has no effect without") {
		t.Errorf("err = %v, want to say the flag has no effect without --%s", err, onceFlagName)
	}

	// --once + 不正なロール: validateOnceMode のエラーが伝わる
	cmd = newQueuesTestCmd(t)
	if err := cmd.Flags().Set(onceFlagName, "true"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, _, err := resolveOnce(cmd, []string{"worker", "api"}); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// **1 件消化モードの停止は「River を graceful に止めてからプロセスを畳む」順序で
// あること。** 逆順にすると、掴んでいたジョブが完走に使える時間が
// `--soft-stop-timeout` の猶予まで縮む（プロセスの畳み = signal ctx の cancel が
// soft stop を開始させるため）。数時間のエンコードを打ち切らないことが ScaledJob
// を選んだ理由そのものなので、猶予を消費せずに完走できる順序を選ぶ。
//
// **順序の理由は SoftStopTimeout の導入で変わった（危険度が下がった）。**
// かつては work ctx が start ctx を継いでいたため、逆順は即座のハードストップ
// だった。テストの主張（順序と ctx 未 cancel）は変えていない。
//
// **この順序は振る舞いのテストでは守れない** --- 実行中のジョブが
// 残る窓は実測 0/25 で踏めなかったので、順序を入れ替えても
// TestServerCmd_OnceMode* は緑のままになる。だからここで直接固定する。
//
// あわせて、stopRiver に渡る ctx が cancel 済みでないことも見る。cancel 済みの
// ctx を渡すと River の Stop は即座に戻り（ctx.Err()）、実行中を待たない。
func TestStopOnceProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// 呼び出し側の ctx が既に終わっていても、graceful stop は待てなければならない。
	cancel()

	var order []string
	var riverCtxAlive bool
	stopRiver := func(stopCtx context.Context) error {
		order = append(order, "river")
		riverCtxAlive = stopCtx.Err() == nil
		return nil
	}
	cancelProcess := func() { order = append(order, "process") }

	stopOnceProcess(ctx, stopRiver, cancelProcess)

	if strings.Join(order, ",") != "river,process" {
		t.Errorf("停止の順序 = %v, want [river process]", order)
	}
	if !riverCtxAlive {
		t.Error("stopRiver に cancel 済みの ctx が渡っている（Stop が実行中を待たずに戻る）")
	}
}

// server サブコマンドに --queues / --once / --once-idle-timeout /
// --soft-stop-timeout が実際に生えていること。**このテストが無いと、フラグ登録を
// 消しても resolveWorkerQueues の単体テストは通り続ける**（フラグは
// newQueuesTestCmd が自前で登録しているため）。
func TestNewServerCmd_HasQueueAndOnceFlags(t *testing.T) {
	cmd := newServerCmd()
	for _, name := range []string{queuesFlagName, onceFlagName, onceIdleTimeoutFlagName, softStopTimeoutFlagName} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("server サブコマンドに --%s が無い", name)
		}
	}
	// 既定値も実バイナリ側の配線で確認する（テスト用 cmd の既定ではなく）。
	if got := cmd.Flags().Lookup(onceIdleTimeoutFlagName).DefValue; got != "30s" {
		t.Errorf("--%s の既定 = %q, want \"30s\"", onceIdleTimeoutFlagName, got)
	}
	// **既定値はリテラルで書く。** worker.DefaultSoftStopTimeout を参照すると、
	// 定数を変えたときに何も主張しなくなる。ここを変えるときは
	// docs/operations.md §5 の `terminationGracePeriodSeconds` の足し算と
	// docker-compose.yml の stop_grace_period も同じ PR で揃える。
	if got := cmd.Flags().Lookup(softStopTimeoutFlagName).DefValue; got != "30s" {
		t.Errorf("--%s の既定 = %q, want \"30s\"", softStopTimeoutFlagName, got)
	}
}

// **`--soft-stop-timeout` の 0 以下を受け取らないこと。**
//
// 0 は「無制限に待つ」ではなく「待たない」である --- River は
// SoftStopTimeout が 0 のとき work ctx を start ctx から継ぐので、SIGTERM が
// そのまま実行中のジョブの ctx を切る（この issue が直した壊れ方そのもの）。
// 「上限を外す」つもりで書いた 0 が意図と正反対に効く形なので、黙って
// 受け取らない。
func TestResolveSoftStopTimeout_RejectsNonPositive(t *testing.T) {
	for _, v := range []string{"0", "-1s"} {
		cmd := newQueuesTestCmd(t)
		if err := cmd.Flags().Set(softStopTimeoutFlagName, v); err != nil {
			t.Fatalf("Set: %v", err)
		}
		got, err := resolveSoftStopTimeout(cmd, workerRole)
		if err == nil {
			t.Fatalf("--%s=%s: error を期待したが nil（%s が通っている）", softStopTimeoutFlagName, v, got)
		}
		if !strings.Contains(err.Error(), softStopTimeoutFlagName) {
			t.Errorf("err = %v, want to name --%s", err, softStopTimeoutFlagName)
		}
	}

	// 反対方向: 正の値はそのまま通る。
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(softStopTimeoutFlagName, "45s"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := resolveSoftStopTimeout(cmd, workerRole)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 45*time.Second {
		t.Errorf("timeout = %s, want 45s", got)
	}
}

// **worker ロールが無いプロセスでの `--soft-stop-timeout` は起動エラー。**
//
// River クライアントを Start するのは worker ロールだけなので
// （resolveRiverClientKind）、`--roles watcher --soft-stop-timeout 5m` は
// 何も待たずに畳む。効かないフラグを黙って無視すると、ScaledJob の argv から
// 写し間違えた Pod が「drain するつもりで drain しない」形になる
// （`--queues` / `--once-idle-timeout` と同じ扱い）。
func TestResolveSoftStopTimeout_RequiresWorkerRole(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	if err := cmd.Flags().Set(softStopTimeoutFlagName, "45s"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := resolveSoftStopTimeout(cmd, []string{"watcher"}); err == nil {
		t.Fatal("expected error, got nil")
	}

	// 反対方向: 指定していなければ worker ロールが無くてもエラーにしない
	// （既定値は全ロールの cmd に生えているので、指定の有無で判定する）。
	if _, err := resolveSoftStopTimeout(newQueuesTestCmd(t), []string{"watcher"}); err != nil {
		t.Errorf("未指定なら通ること: %v", err)
	}
}
