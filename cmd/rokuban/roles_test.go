package main

import (
	"slices"
	"testing"

	"github.com/fetburner/rokuban/internal/db"
	"github.com/fetburner/rokuban/internal/worker"
)

// ロール分類の基準は「ソケットを持ち続けるか」であり、「どの仕事をするか」ではない
// （docs/overview.md §ロール分類の基準、issue #24 M2-20）。この基準は docs だけに
// 書いても守られないので、破ったときに落ちるようにしておく。
//
// 守りたいのは「キューが増えるたびにロールが増える」形への回帰。EPGStation の
// トリアージ（issue #10）で見た通り、そうなるとデプロイ単位が仕事の数だけ増え、
// monolithic mode の意味が薄れる。

// キュー駆動の仕事はロールにしない。ここに挙げた名前が allRoles に現れたら、
// それは River のジョブをロールに昇格させてしまったということ。
func TestAllRoles_ExcludesJobNames(t *testing.T) {
	// worker が引くキューの名前。ロール名と衝突してはいけない。
	// worker.AllQueueNames が返す実際のキュー集合から取るので、キューを追加したら
	// 自動的にこのテストの対象に入る（手で並べた定数だと追加時にすり抜ける）。
	for _, q := range worker.AllQueueNames() {
		// watcher キュー（record_sweep）だけは例外。watcher ロールと同名だが、
		// これは「watcher の 3 段構えのうち (c) 定期全量突き合わせ」を切り出した
		// ジョブであり、ロール名を流用しているだけで昇格ではない（M2-18）。
		if q == "watcher" {
			continue
		}
		if slices.Contains(allRoles, q) {
			t.Errorf("queue %q is also a role name: キュー駆動の仕事はすべて worker ロールが引く。"+
				"ロールを増やすのではなく worker.queues で割り当てる（docs/overview.md §ロール分類の基準）", q)
		}
	}
}

// シングルトンは「ソケットを connect し続ける」ロールだけ。listen する側
// （api / notifier / streamer）を 1 プロセスに絞ると水平スケールできなくなり、
// ジョブ（worker）を絞るとキュー長でのスケールが効かなくなる。
func TestSingletonRoles_OnlyOutboundSocketRoles(t *testing.T) {
	// mirakc へ外向き接続を張り続けるロール。増えたらここに足す判断が必要になる。
	outbound := []string{"watcher"}

	for _, r := range singletonRoles {
		if !slices.Contains(allRoles, r) {
			t.Errorf("singleton role %q is not in allRoles", r)
		}
		if !slices.Contains(outbound, r) {
			t.Errorf("singleton role %q は外向き接続を張り続けるロールではない。"+
				"listen 側を絞ると水平スケールが、ジョブを絞るとキュー長スケールが壊れる"+
				"（docs/overview.md §ロール分類の基準）", r)
		}
	}
}

// worker はシングルトンにしない。KEDA で 0〜N にスケールする形が worker ロールの
// 定義そのものなので、これが破れると M2-17 / M2-18 でジョブ化した意味がなくなる。
func TestWorkerRole_NotSingleton(t *testing.T) {
	if slices.Contains(singletonRoles, "worker") {
		t.Error("worker must not be a singleton: キュー長で 0〜N にスケールする形が worker の定義")
	}
}

// notifier はシングルトンにしない。各レプリカが独立に LISTEN し、自分にぶら下がる
// SSE クライアントにだけ配るので、レプリカ間の調停が構造的に要らない（M2-19）。
// reconciler / ruler が「冪等だから複数動いてよい」のとは別の理由づけ。
func TestNotifierRole_NotSingleton(t *testing.T) {
	if !slices.Contains(allRoles, "notifier") {
		t.Fatal("notifier should be a role (SSE は listen し続けるので形はサーバー)")
	}
	if slices.Contains(singletonRoles, "notifier") {
		t.Error("notifier must not be a singleton: 配送先が自分の接続だけなので調停が不要")
	}
}

// --all は全ロールを起動する。ロールを追加したときに allRoles へ入れ忘れると
// monolithic mode から機能が抜け落ちるが、単体では気付きにくい。
func TestResolveRoles_AllCoversEveryRole(t *testing.T) {
	cmd := newServerCmd()
	if err := cmd.Flags().Set("all", "true"); err != nil {
		t.Fatal(err)
	}

	roles, err := resolveRoles(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(roles, allRoles) {
		t.Errorf("--all resolved to %v, want %v", roles, allRoles)
	}
}

// allRoles（この権威）と internal/db.KnownRoles()（プールサイジング budget と
// pooler_compat fail-fast 判定の権威）は独立に管理されている。両方 unexported の
// ままだと「一致している」ことをテストできず、M4-6 で新しいロールを allRoles に
// 足したのに internal/db 側の対応（roleConnBudget のエントリ・
// poolerIncompatibleRoles に入れるかどうかの判断）を忘れても静かに素通りしてしまう
// （新ロールは自動的に db.minAutoMaxConns にフォールバックし、pooler_compat の
// fail-fast も対象外になる。issue #90 レビュー）。
func TestAllRoles_MatchesDBKnownRoles(t *testing.T) {
	got := slices.Clone(allRoles)
	slices.Sort(got)
	want := db.KnownRoles()
	if !slices.Equal(got, want) {
		t.Errorf("allRoles (cmd/rokuban) = %v, db.KnownRoles() (internal/db) = %v; "+
			"ロールを足したら internal/db の roleConnBudget と poolerIncompatibleRoles も更新すること", got, want)
	}
}

func TestResolveRoles_RejectsJobNameAsRole(t *testing.T) {
	// ruler / reconciler をロール名として渡せてしまうと、ジョブ化（M2-17）以前の
	// デプロイ手順がそのまま通ってしまい、実際には何も動かないプロセスが立つ。
	for _, name := range []string{"ruler", "reconciler", "epg", "ingest"} {
		t.Run(name, func(t *testing.T) {
			cmd := newServerCmd()
			if err := cmd.Flags().Set("roles", name); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveRoles(cmd); err == nil {
				t.Errorf("resolveRoles accepted %q as a role; これは worker が引くキューであって"+
					"ロールではない", name)
			}
		})
	}
}
