package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/fetburner/rokuban/internal/mirakc"
)

// specPath は openapi.yaml（形の権威。CLAUDE.md「形の権威は openapi.yaml」）。
const specPath = "../../openapi.yaml"

// TestSpecServiceBoundsMatchGo は `?service=` の値域が openapi.yaml と Go の
// 実装で一致していることを検査する。
//
// **同じ数字が 3 箇所にある。** `openapi.yaml` の `maximum`（宣言）、
// `internal/mirakc.MaxServiceID`（サーバの検査）、そして openapi.yaml から
// 生成される `web/src/api/zod.ts`（フロントの検査）。生成物は spec に自動で
// 追随するが、**Go の定数は追随しない** --- spec の `maximum` を書き換えると
// フロントだけが新しい値になり、サーバは古い値のまま黙って食い違う。
// その状態では「フロントが弾いた値をサーバが受ける」「その逆」が起きる。
//
// oapi-codegen はスキーマの数値制約を生成コードに落とさない（`openapi_gen.go`
// に minimum / maximum は 1 つも現れない。`splitServiceIDs` の doc コメント参照）
// ので、この一致を機械的に見る場所は他に無い。
//
// spec 側を書き換えたらこのテストが落ちる。落ちたら Go の定数も直す。
func TestSpecServiceBoundsMatchGo(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parsing %s: %v", specPath, err)
	}

	bounds := serviceParamBounds(t, spec)
	// `?service=` を持つのは ListPrograms と ListRecordings の 2 つ。片方だけを
	// 見つけて満足しないよう件数も固定する（spec 側で名前が変わったら気付く）。
	if len(bounds) != 2 {
		t.Fatalf("found %d `service` query parameters in the spec, want 2 "+
			"(ListPrograms / ListRecordings)", len(bounds))
	}
	for _, b := range bounds {
		if b.min != 1 {
			t.Errorf("%s: minimum = %d, want 1", b.where, b.min)
		}
		if b.max != int64(mirakc.MaxServiceID) {
			t.Errorf("%s: maximum = %d, but internal/mirakc.MaxServiceID = %d "+
				"（spec とサーバの検査がずれている。フロントは spec から生成されるので、"+
				"この状態ではフロントとサーバで受理する値が食い違う）",
				b.where, b.max, mirakc.MaxServiceID)
		}
	}
}

type serviceBound struct {
	where    string
	min, max int64
}

// serviceParamBounds は spec 中の `service` クエリパラメータの items 値域を集める。
//
// パスを決め打ちで辿らず全 path / 全 method を走査するのは、エンドポイントが
// 増えたときに黙って見落とさないため（`?service=` を持つ新しい経路ができたら
// 上の件数チェックが落ちて気付ける）。
func serviceParamBounds(t *testing.T, spec map[string]any) []serviceBound {
	t.Helper()
	paths, _ := spec["paths"].(map[string]any)
	var out []serviceBound
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, op := range methods {
			opMap, ok := op.(map[string]any)
			if !ok {
				continue
			}
			params, _ := opMap["parameters"].([]any)
			for _, p := range params {
				pm, _ := p.(map[string]any)
				if name, _ := pm["name"].(string); name != "service" {
					continue
				}
				if in, _ := pm["in"].(string); in != "query" {
					continue
				}
				schema, _ := pm["schema"].(map[string]any)
				items, _ := schema["items"].(map[string]any)
				min, minOK := toInt64(items["minimum"])
				max, maxOK := toInt64(items["maximum"])
				if !minOK || !maxOK {
					t.Errorf("%s %s: `service` の items に minimum/maximum が無い "+
						"（値域の宣言が消えると生成される zod スキーマからも検査が消える）",
						method, path)
					continue
				}
				out = append(out, serviceBound{where: method + " " + path, min: min, max: max})
			}
		}
	}
	return out
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case uint64:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
