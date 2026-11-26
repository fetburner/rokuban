// ci-source-deps: -tags=s3conformance ./...
// 全パッケージの s3conformance タグの _test.go を読んで検査するので、ソースを読む宣言にする。

// Package s3conformance はテストだけを持つ。s3-conformance ジョブの規約を検査する。
//
// ジョブは 1 本の `go test -tags s3conformance $S3_CONFORMANCE_PACKAGES -run '^TestS3'` で、
// 対象は `//go:build s3conformance` のファイルにある Test と、同じパッケージの名前が TestS3 で始まる
// タグ無しの Test である。そのため次の 2 つを規約にし、ここで両方向に検査する。
//
//   - タグ付きの Test は、名前が TestS3 で始まり、その所属パッケージは S3_CONFORMANCE_PACKAGES に入る。
//     S3_CONFORMANCE_PACKAGES のパッケージは、タグ付きの Test を少なくとも 1 本持つ。
//   - S3_CONFORMANCE_PACKAGES のパッケージのタグ無しの Test は、名前が TestS3 で始まらない。
//     始まると、-run がタグ無しのテストまで拾ってしまう。
//
// 計画（ci.yml の env の S3_CONFORMANCE_PACKAGES。plan ジョブの test 計画が citest に渡す一覧）は
// ci.yml の 1 行を読むだけで、ジョブの step は読まない。
//
// 対象は s3conformance だけ。conformance タグ（internal/mirakc/conformance）は
// mirakc-conformance ジョブが `go list -tags conformance` でパッケージごと実行するので、対象外。
package s3conformance

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/goccy/go-yaml"
)

const (
	tag = "s3conformance"
	// prefix は、タグ付きの Test の名前の接頭辞であり、CI の -run に書く正規表現 ^TestS3 の中身。
	prefix = "TestS3"
	ciOpen = "../../.github/workflows/ci.yml" // テストのカレントディレクトリからの相対パス
	ciName = ".github/workflows/ci.yml"       // メッセージに出すリポジトリ相対のパス
)

// testFunc は Test 関数 1 本。file はリポジトリ相対のパス。
type testFunc struct{ file, name string }

// pkgTests は 1 パッケージのテスト関数。キーは "./internal/..." の形のパッケージ。
type pkgTests struct {
	tagged   []testFunc // s3conformance のタグで付くファイルの Test
	untagged []testFunc // それ以外の _test.go の Test
}

// TestS3ConventionHoldsInTheRepo は、リポジトリの現在の状態が規約を満たすことを検査する。
func TestS3ConventionHoldsInTheRepo(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(ciOpen)
	if err != nil {
		t.Fatalf("%s を読めない: %v", ciName, err)
	}
	planned, err := parsePlanPackages(src)
	if err != nil {
		t.Fatalf("%s: %v", ciName, err)
	}
	tests, err := collectTests("../..")
	if err != nil {
		t.Fatalf("テスト関数を集められない: %v", err)
	}
	if n := countTagged(tests); n == 0 {
		t.Fatalf("s3conformance のテスト関数が 1 つも見つからない（検査が空振りしている）")
	}
	for _, msg := range conventionViolations(tests, planned) {
		t.Error(msg)
	}
}

// TestConventionViolations は、規約違反の判定と説明を、リテラルの入力で確かめる。
func TestConventionViolations(t *testing.T) {
	t.Parallel()
	const file = "internal/x/x_conformance_test.go"
	tests := map[string]*pkgTests{
		// TestB は名前が規約に合わない（タグ付き）。TestS3Old はタグ無しなのに規約の接頭辞で始まる。
		"./internal/x": {
			tagged:   []testFunc{{file, "TestS3A"}, {file, "TestB"}},
			untagged: []testFunc{{"internal/x/y_test.go", "TestS3Old"}, {"internal/x/y_test.go", "TestPlain"}},
		},
		// ./internal/z はタグ付きの Test を持つが、S3_CONFORMANCE_PACKAGES に無い。
		"./internal/z": {tagged: []testFunc{{"internal/z/z_conformance_test.go", "TestS3Z"}}},
		// ./internal/w は S3_CONFORMANCE_PACKAGES に入っているが、タグ付きの Test が 1 本も無い。
		"./internal/w": {untagged: []testFunc{{"internal/w/w_test.go", "TestW"}}},
	}
	planned := []string{"./internal/x", "./internal/w"}
	got := conventionViolations(tests, planned)
	// 各説明は、後ろに手順の案内が続くので前方一致で見る。
	want := []string{
		"internal/x/x_conformance_test.go の TestB は s3conformance のタグ付きだが、名前が TestS3 で始まらない",
		"internal/x/y_test.go の TestS3Old はタグ無しだが、名前が TestS3 で始まる",
		"パッケージ ./internal/z はタグ付きの Test を持つが、S3_CONFORMANCE_PACKAGES に無い",
		"S3_CONFORMANCE_PACKAGES のパッケージ ./internal/w にタグ付きの Test が無い",
	}
	if len(got) != len(want) {
		t.Fatalf("conventionViolations は %d 件 (%q)、want %d 件", len(got), got, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("message %d = %q, want prefix %q", i, got[i], want[i])
		}
	}
}

// TestFileTestsFollowsBuildTag は、ファイルのタグ判定と Test 関数の集め方を確かめる。
func TestFileTestsFollowsBuildTag(t *testing.T) {
	t.Parallel()
	taggedSrc := "//go:build s3conformance\n\npackage x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\nfunc helper() {}\nfunc testLower(t *testing.T) {}\nfunc (x) TestMethod(t *testing.T) {}\n"
	untagged := "package x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"
	negated := "//go:build !s3conformance\n\npackage x\n\nfunc TestA(t *testing.T) {}\n"
	otherTag := "//go:build conformance\n\npackage x\n\nfunc TestA(t *testing.T) {}\n"

	names, isTagged, err := fileTests([]byte(taggedSrc))
	if err != nil || !isTagged || strings.Join(names, " ") != "TestA" {
		t.Errorf("tagged: got %v, %v, %v; want [TestA] tagged", names, isTagged, err)
	}
	for name, src := range map[string]string{"untagged": untagged, "negated": negated, "otherTag": otherTag} {
		names, isTagged, err := fileTests([]byte(src))
		if err != nil || isTagged || strings.Join(names, " ") != "TestA" {
			t.Errorf("%s: got %v, %v, %v; want [TestA] not tagged", name, names, isTagged, err)
		}
	}
}

// parsePlanPackages は ci.yml の workflow env の S3_CONFORMANCE_PACKAGES を "./internal/..." の形で返す。
func parsePlanPackages(src []byte) ([]string, error) {
	var doc struct {
		Env map[string]string `yaml:"env"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("ci.yml を解析できない: %w", err)
	}
	list := strings.Fields(doc.Env["S3_CONFORMANCE_PACKAGES"])
	if len(list) == 0 {
		return nil, fmt.Errorf("env に S3_CONFORMANCE_PACKAGES が無いか空")
	}
	pkgs := make([]string, 0, len(list))
	for _, p := range list {
		pkgs = append(pkgs, "./"+path.Clean(p))
	}
	return pkgs, nil
}

// conventionViolations は規約違反の説明を、順序を決めて返す。
func conventionViolations(tests map[string]*pkgTests, planned []string) []string {
	inPlan := map[string]bool{}
	for _, p := range planned {
		inPlan[p] = true
	}
	dirs := make([]string, 0, len(tests))
	for d := range tests {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var msgs []string
	for _, dir := range dirs {
		pt := tests[dir]
		for _, tf := range pt.tagged {
			if !strings.HasPrefix(tf.name, prefix) {
				msgs = append(msgs, fmt.Sprintf(
					"%s の %s は s3conformance のタグ付きだが、名前が %s で始まらない。CI の s3-conformance ジョブは -run '^%s' で選ぶので、このテストは実行されない",
					tf.file, tf.name, prefix, prefix))
			}
		}
		if len(pt.tagged) > 0 && !inPlan[dir] {
			msgs = append(msgs, fmt.Sprintf(
				"パッケージ %s はタグ付きの Test を持つが、S3_CONFORMANCE_PACKAGES に無い。PR はこのパッケージの変更で s3-conformance を回さない（%s の env）",
				dir, ciName))
		}
		if inPlan[dir] {
			for _, tf := range pt.untagged {
				if strings.HasPrefix(tf.name, prefix) {
					msgs = append(msgs, fmt.Sprintf(
						"%s の %s はタグ無しだが、名前が %s で始まる。s3-conformance ジョブの -run が拾ってしまう。名前を変えるか、接頭辞を外す",
						tf.file, tf.name, prefix))
				}
			}
		}
	}
	for _, p := range planned {
		if pt, ok := tests[p]; !ok || len(pt.tagged) == 0 {
			msgs = append(msgs, fmt.Sprintf(
				"S3_CONFORMANCE_PACKAGES のパッケージ %s にタグ付きの Test が無い。一覧から外すか、タグ付きの Test を足す", p))
		}
	}
	return msgs
}

func countTagged(tests map[string]*pkgTests) int {
	n := 0
	for _, pt := range tests {
		n += len(pt.tagged)
	}
	return n
}

// collectTests は root 以下の _test.go から Test 関数を集め、パッケージごとに返す。
func collectTests(root string) (map[string]*pkgTests, error) {
	tests := map[string]*pkgTests{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		names, isTagged, err := fileTests(src)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if len(names) == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		dir := "./" + filepath.ToSlash(filepath.Dir(rel))
		pt := tests[dir]
		if pt == nil {
			pt = &pkgTests{}
			tests[dir] = pt
		}
		for _, n := range names {
			tf := testFunc{file: filepath.ToSlash(rel), name: n}
			if isTagged {
				pt.tagged = append(pt.tagged, tf)
			} else {
				pt.untagged = append(pt.untagged, tf)
			}
		}
		return nil
	})
	return tests, err
}

// fileTests は _test.go の全文 src の Test 関数名を返し、s3conformance のタグで付くかを返す。
// 判定は `//go:build` の式を s3conformance だけ真として評価する。タグだけで決まる形
// （`//go:build s3conformance`）のファイルがこのリポジトリの全てで、他の GOOS 条件と組む形は無い。
func fileTests(src []byte) (names []string, isTagged bool, err error) {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, false, err
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && isTestName(fn.Name.Name) {
			names = append(names, fn.Name.Name)
		}
	}
	return names, tagged(src), nil
}

// tagged は package 節より前の //go:build 行を s3conformance だけ真として評価する。
func tagged(src []byte) bool {
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "package ") {
			return false
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		return err == nil && expr.Eval(func(t string) bool { return t == tag })
	}
	return false
}

// isTestName は go test が Test 関数として扱う名前かを返す（Test の後ろは小文字で続かない）。
func isTestName(n string) bool {
	rest, ok := strings.CutPrefix(n, "Test")
	return ok && (rest == "" || !unicode.IsLower(rune(rest[0])))
}
