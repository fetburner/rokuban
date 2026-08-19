package api

// openapi.yaml を変更したら go generate ./internal/api/ で再生成する。
// 生成物 (openapi_gen.go) はコミットする（レビューで API の差分が見える／ビルドに
// oapi-codegen が要らないため）。再生成漏れは CI の generated-diff ジョブが
// 再生成して差分を検証する。
//go:generate oapi-codegen --config=oapi-codegen.yaml ../../openapi.yaml
