package breaker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestAll_MatchesDeclaredConstants は internal/breaker パッケージが宣言する
// エクスポート済み文字列定数の集合と All が一致することを保証する (issue #199
// のレビューで指摘された穴の再発防止)。
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
// パッケージのソースを直接読むことで、All 側の漏れそのものを検出する。
//
// # スキャン範囲についての 2 つの決定（PR #265 の 2 回目のレビューで指摘）
//
//  1. **エクスポート済み識別子だけを対象にする。** 非公開の文字列定数
//     （例: `probeLabel = "breaker"` のようなログ用ラベル）はブレーカー名
//     ではあり得ない --- ブレーカー名は internal/api や internal/worker /
//     internal/ruler など他パッケージから breaker.XxxDeletes の形で参照
//     される必要があり、非公開では参照できない。これを絞らずに全ての
//     文字列 const を拾うと、無関係な非公開定数を「All に足し忘れた
//     ブレーカー名」と誤診断し、しかも直し方の提案（All に足す）が実際には
//     間違っている（All に足すと knownCircuitBreakerNames がその文字列で
//     resume を受理してしまう）。
//  2. **breaker.go 1 ファイルではなく、パッケージディレクトリ全体
//     （_test.go を除く）を読む。** 1 ファイルだけを対象にすると、将来
//     誰かが別ファイルに定数を分割した瞬間にスキャン対象から静かに外れる
//     （teqst で実測済み: 別ファイルに const を置くとテストは緑のまま
//     通ってしまう）。_test.go を除くのは、テストコード自身が定義する
//     一時的な文字列定数（このファイルにあるような mutation 用の値を含む）
//     をブレーカー名と誤認しないため。
func TestAll_MatchesDeclaredConstants(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to report this test file's path")
	}
	pkgDir := filepath.Dir(thisFile)

	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("reading package directory %s: %v", pkgDir, err)
	}

	fset := token.NewFileSet()
	declared := map[string]bool{}
	scannedFiles := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		srcPath := filepath.Join(pkgDir, name)
		file, err := parser.ParseFile(fset, srcPath, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", srcPath, err)
		}
		scannedFiles++

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
				for i, value := range valueSpec.Values {
					lit, ok := value.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					// エクスポート済み（先頭大文字）の識別子だけを対象に
					// する。非公開の文字列定数は他パッケージから
					// breaker.XxxDeletes の形で参照できないので、
					// ブレーカー名ではあり得ない。
					if i >= len(valueSpec.Names) || !valueSpec.Names[i].IsExported() {
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
	}

	// パーサの配線そのものが壊れていると declared が空のまま両方の比較を
	// 素通りしてしまう（このテストが「通るだけで何も保証しない」形に
	// 壊れる典型パターン。CLAUDE.md テスト規律）。breaker.go には現に
	// エクスポート済み文字列定数が 3 つあるので、必ず何か拾えているはずである。
	if scannedFiles == 0 {
		t.Fatalf("scanned zero .go files in %s — directory listing is broken (this test would otherwise pass vacuously)", pkgDir)
	}
	if len(declared) == 0 {
		t.Fatalf("parsed zero exported string constants from %s (scanned %d file(s)) — parser wiring is broken (this test would otherwise pass vacuously)", pkgDir, scannedFiles)
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
		t.Errorf("exported string const(s) declared in %s but missing from All: %v (add them to All if they are breaker identifiers)", pkgDir, missingFromAll)
	}
	if len(extraInAll) > 0 {
		t.Errorf("All contains name(s) with no matching exported string const in %s: %v", pkgDir, extraInAll)
	}
}
