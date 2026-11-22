package web

import "embed"

// DistFS は Vite ビルド出力 (web/dist/) を埋め込む。
// 追跡済みの web/dist/.gitkeep が go:embed all:dist の置き場を満たすため、pnpm build 前でも Go のビルドは通る。
// SPA を配信するバイナリには pnpm build で本物の出力を用意する。
//
//go:embed all:dist
var DistFS embed.FS
