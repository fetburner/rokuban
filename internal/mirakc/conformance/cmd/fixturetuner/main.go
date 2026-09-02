//go:build conformance

// Command fixturetuner は internal/mirakc/conformance テストが mirakc の
// tuners[].command として使う偽チューナー。標準出力へ ARIB SI 付きの合成 TS を実時間で
// 流し続ける。mirakc コンテナへバイナリごと bind mount して使う想定なので、引数・
// 環境変数はいずれも読まない（呼び出しの Mustache テンプレート変数は無視してよい —
// このフィクスチャは常に同じ 1 サービス・1 番組しか表現しない）。
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
	_ = fixture.Run(ctx, os.Stdout, fixture.NewConfig())
}
