package breaker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
)

// TestAll_MatchesDeclaredConstants は breaker.go の const ブロックが宣言する
// 文字列定数の集合と All が一致することを保証する (issue #199 のレビューで
// 指摘された穴の再発防止)。
//
// All は internal/api の knownCircuitBreakerNames が唯一参照する権威だが、
// All 自体は const 宣言の手書きの複製である。これまでは「Go の定数はリフレ
// クションで列挙できないので、この複製がずれても機械的には捕まえられない」
// と考えていたが、それは誤りだった --- リフレクションではなく go/parser で
// ソースを静的に読めば、コンパイル前に突き合わせられる。この誤った断言は
// PR #265 のレビューで指摘され、このテストに置き換えた。
//
// このテストは「All からループを生成する」形の回帰テスト
// （internal/api/breakers_test.go の TestCircuitBreaker_TripListResumeRoundTripForEveryKnownName）
// では代替できない --- そちらは All の中身をそのままテストケースにするので、
// All に無い定数はループの視界にすら入らない。ここでは All を経由せず
// breaker.go のソースを直接読むことで、All 側の漏れそのものを検出する。
func TestAll_MatchesDeclaredConstants(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to report this test file's path")
	}
	srcPath := filepath.Join(filepath.Dir(thisFile), "breaker.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, srcPath, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", srcPath, err)
	}

	declared := map[string]bool{}
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, value := range valueSpec.Values {
				lit, ok := value.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquoting const literal %s in %s: %v", lit.Value, srcPath, err)
				}
				declared[unquoted] = true
			}
		}
	}

	// パーサの配線そのものが壊れていると declared が空のまま両方の比較を
	// 素通りしてしまう（このテストが「通るだけで何も保証しない」形に
	// 壊れる典型パターン。CLAUDE.md テスト規律）。breaker.go には現に
	// 文字列定数が 3 つあるので、必ず何か拾えているはずである。
	if len(declared) == 0 {
		t.Fatalf("parsed zero string constants from %s — parser wiring is broken (this test would otherwise pass vacuously)", srcPath)
	}

	known := map[string]bool{}
	for _, name := range All {
		known[name] = true
	}

	var missingFromAll, extraInAll []string
	for name := range declared {
		if !known[name] {
			missingFromAll = append(missingFromAll, name)
		}
	}
	for name := range known {
		if !declared[name] {
			extraInAll = append(extraInAll, name)
		}
	}
	sort.Strings(missingFromAll)
	sort.Strings(extraInAll)

	if len(missingFromAll) > 0 {
		t.Errorf("const(s) declared in %s but missing from All: %v (add them to All)", srcPath, missingFromAll)
	}
	if len(extraInAll) > 0 {
		t.Errorf("All contains name(s) with no matching string const in %s: %v", srcPath, extraInAll)
	}
}
