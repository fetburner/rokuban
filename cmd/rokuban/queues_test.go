package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func newQueuesTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringSlice(queuesFlagName, nil, "")
	cmd.Flags().Bool(onceFlagName, false, "")
	cmd.Flags().Duration(onceIdleTimeoutFlagName, defaultOnceIdleTimeout, "")
	return cmd
}

// --queues 未指定なら config の worker.queues がそのまま使われること
// （既存構成の挙動を変えない）。
func TestResolveWorkerQueues_UnspecifiedUsesConfig(t *testing.T) {
	cmd := newQueuesTestCmd(t)
	got, err := resolveWorkerQueues(cmd, []string{"ruler", "reconciler"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(got, ",") != "ruler,reconciler" {
		t.Errorf("queues = %v, want [ruler reconciler]", got)
	}

	// config も空なら空（= 全キュー）のまま。
	empty, err := resolveWorkerQueues(newQueuesTestCmd(t), nil)
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
	got, err := resolveWorkerQueues(cmd, nil)
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
	got, err := resolveWorkerQueues(cmd, nil)
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
	_, err := resolveWorkerQueues(cmd, []string{"ruler"})
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
	_, err := resolveWorkerQueues(cmd, nil)
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
	_, err := resolveWorkerQueues(cmd, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "--"+queuesFlagName) {
		t.Errorf("err = %v, want to mention the unknown name and --%s", err, queuesFlagName)
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
	if idle != defaultOnceIdleTimeout {
		t.Errorf("idle = %s, want %s", idle, defaultOnceIdleTimeout)
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

// server サブコマンドに --queues / --once / --once-idle-timeout が実際に
// 生えていること。**このテストが無いと、フラグ登録を消しても
// resolveWorkerQueues の単体テストは通り続ける**（フラグは
// newQueuesTestCmd が自前で登録しているため）。
func TestNewServerCmd_HasQueueAndOnceFlags(t *testing.T) {
	cmd := newServerCmd()
	for _, name := range []string{queuesFlagName, onceFlagName, onceIdleTimeoutFlagName} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("server サブコマンドに --%s が無い", name)
		}
	}
	// 既定値も実バイナリ側の配線で確認する（テスト用 cmd の既定ではなく）。
	if got := cmd.Flags().Lookup(onceIdleTimeoutFlagName).DefValue; got != "30s" {
		t.Errorf("--%s の既定 = %q, want \"30s\"", onceIdleTimeoutFlagName, got)
	}
}
