//go:build conformance

// Command fixturetuner は internal/mirakc/conformance テストが mirakc の
// tuners[].command として使う偽チューナー。標準出力へ ARIB SI 付きの合成 TS を実時間で
// 流し続ける。mirakc コンテナへバイナリごと bind mount して使う想定。引数は読まない
// （mirakc の command は文字列で、引数は Mustache テンプレートの展開結果に依存しやすい）
// が、病態ケースだけは
// ROKUBAN_FIXTURE_CASE 環境変数（preceding-extension / running-status / following /
// event-id-reset）で切り替える。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/fetburner/rokuban/internal/mirakc/conformance/fixture"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// mirakc がチューナーを閉じるときは概ねプロセスを kill する。stdout への書き込み失敗
	// （パイプが閉じられた）も ctx キャンセルも、どちらも「正常に止められた」として exit 0
	// にする。
	cfg := fixture.NewConfig()
	if len(os.Args) > 1 {
		cfg = fixture.NewConfigForChannel(os.Args[1])
	}
	if name := os.Getenv("ROKUBAN_FIXTURE_CASE"); name != "" {
		cfg.Case = name
	}
	_ = fixture.Run(ctx, os.Stdout, cfg)
}
