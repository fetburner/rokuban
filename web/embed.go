package web

import "embed"

// DistFS は Vite ビルド出力 (web/dist/) を埋め込む。
// embed ディレクティブが参照するため、Go ビルド前に pnpm build が必要。
// CI では .github/workflows/ci.yml でビルドステップを追加する。
//
//go:embed all:dist
var DistFS embed.FS
