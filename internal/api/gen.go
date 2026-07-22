package api

// openapi.yaml を変更したら go generate ./internal/api/ で再生成する。
// 生成物 (openapi_gen.go) はコミットする（CI で go generate 不要にするため）。
//go:generate oapi-codegen --config=oapi-codegen.yaml ../../openapi.yaml
