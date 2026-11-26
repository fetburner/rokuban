// ci-source-deps: -tags=s3conformance ./...
// 全パッケージの s3conformance タグの _test.go を読んで検査するので、ソースを読む宣言にする。

// Package s3conformance はテストだけを持つ。`//go:build s3conformance` の _test.go に書かれた
// Test 関数が、CI の s3-conformance ジョブの `go test -tags s3conformance` の step のどれかで
// 実行されることを検査する。タグ無しの `go test ./...` はこれらのファイルをコンパイルせず、
// step に載らない Test は誰も実行しないまま緑になる。この検査は test ジョブが回す
// `go list ./...` の対象なので、専用の job は要らない。
//
// 対象は s3conformance だけ。conformance タグ（internal/mirakc/conformance）は
// mirakc-conformance ジョブが `go list -tags conformance` でパッケージごと実行するので、
// 関数単位の照合は要らない。
//
// 計画（ci.yml の env の S3_CONFORMANCE_PACKAGES。plan ジョブの test 計画が citest に渡す一覧）と step のパッケージ集合の一致も、
// TestS3ConformancePackagesMatchSteps が検査する。
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
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/goccy/go-yaml"
)

const (
	tag    = "s3conformance"
	ciOpen = "../../.github/workflows/ci.yml" // テストのカレントディレクトリからの相対パス
	ciName = ".github/workflows/ci.yml"       // メッセージに出すリポジトリ相対のパス
	ciJob  = "s3-conformance"
)

// testFunc は s3conformance のテスト関数 1 本。file はリポジトリ相対のパス。
type testFunc struct{ file, name string }

// step は s3-conformance ジョブの `go test` 1 本。pkg は path.Clean 済みの "./internal/..."。
// run が空なら、そのパッケージの全テストを実行する。
type step struct{ pkg, run string }

func TestEveryS3ConformanceTestHasStep(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(ciOpen)
	if err != nil {
		t.Fatalf("%s を読めない: %v", ciName, err)
	}
	steps, err := parseSteps(src)
	if err != nil {
		t.Fatalf("%s: %v", ciName, err)
	}
	tests, err := collectTests("../..")
	if err != nil {
		t.Fatalf("テスト関数を集められない: %v", err)
	}
	if len(tests) == 0 {
		t.Fatalf("s3conformance のテスト関数が 1 つも見つからない（検査が空振りしている）")
	}
	for _, msg := range uncovered(tests, steps) {
		t.Error(msg)
	}
}

// TestS3ConformancePackagesMatchSteps は、計画（ci.yml の env の S3_CONFORMANCE_PACKAGES。plan ジョブの test 計画が
// citest に渡す）と s3-conformance ジョブの step のパッケージ集合が一致することを検査する。
// 計画に無いパッケージの step は、そのパッケージだけを変えた PR で実行されない。step に無いパッケージは
// 計画が空振りさせる。どちらも片側だけ直したときに落とす。
func TestS3ConformancePackagesMatchSteps(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(ciOpen)
	if err != nil {
		t.Fatalf("%s を読めない: %v", ciName, err)
	}
	steps, err := parseSteps(src)
	if err != nil {
		t.Fatalf("%s: %v", ciName, err)
	}
	planned, err := parsePlanPackages(src)
	if err != nil {
		t.Fatalf("%s: %v", ciName, err)
	}
	inSteps := map[string]bool{}
	for _, st := range steps {
		inSteps[st.pkg] = true
	}
	inPlan := map[string]bool{}
	for _, p := range planned {
		inPlan[p] = true
	}
	for _, p := range sortedKeys(inSteps) {
		if !inPlan[p] {
			t.Errorf("%s の s3-conformance の step のパッケージ %s が env の S3_CONFORMANCE_PACKAGES に無い。PR はこのパッケージの変更で s3-conformance を回さない", ciName, p)
		}
	}
	for _, p := range sortedKeys(inPlan) {
		if !inSteps[p] {
			t.Errorf("%s の env の S3_CONFORMANCE_PACKAGES の %s に対応する step が s3-conformance に無い", ciName, p)
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

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestStepForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		line string
		want []step // nil は「s3conformance の go test ではない」
	}{
		{"go test -tags s3conformance ./internal/objstore/conformance -count=1 -timeout 5m -v",
			[]step{{"./internal/objstore/conformance", ""}}},
		{"go test -tags s3conformance ./internal/storagemaint -run '^TestS3X$' -count=1 -timeout 5m -v",
			[]step{{"./internal/storagemaint", "^TestS3X$"}}},
		{"go test -tags s3conformance ./internal/objstore/s3store -run '^(TestA|TestB)$' -count=1",
			[]step{{"./internal/objstore/s3store", "^(TestA|TestB)$"}}},
		{"go test -tags=s3conformance ./internal/encode -run=^TestC$",
			[]step{{"./internal/encode", "^TestC$"}}},
		{"go build -tags s3conformance -o out ./internal/streamer/conformance", nil},
		{"go test ./internal/encode -run '^TestC$'", nil},
	}
	for _, tc := range tests {
		got, ok := parseGoTest(tc.line)
		if ok != (tc.want != nil) {
			t.Errorf("%q: s3conformance の step か = %v, want %v", tc.line, ok, tc.want != nil)
			continue
		}
		if strings.Join(stepStrings(got), " ") != strings.Join(stepStrings(tc.want), " ") {
			t.Errorf("%q: 読んだ step = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestParseStepsReadsOnlyTheConformanceJob(t *testing.T) {
	t.Parallel()
	src := []byte(`jobs:
  other:
    steps:
      - run: go test -tags s3conformance ./internal/wrong -run '^TestX$'
  s3-conformance:
    steps:
      - name: multi
        run: |
          echo start
          go test -tags s3conformance ./internal/a -count=1
      - run: go test -tags s3conformance ./internal/b -run '^(TestB)$' -count=1 -timeout 5m -v
`)
	steps, err := parseSteps(src)
	if err != nil {
		t.Fatal(err)
	}
	want := `./internal/a run="" ./internal/b run="^(TestB)$"`
	got := strings.Join(stepStrings(steps), " ")
	if got != want {
		t.Errorf("parseSteps = %q, want %q", got, want)
	}
}

func TestParseStepsRejectsSubtestPattern(t *testing.T) {
	t.Parallel()
	src := []byte(`jobs:
  s3-conformance:
    steps:
      - run: go test -tags s3conformance ./internal/a -run '^TestA/sub$' -count=1
`)
	if _, err := parseSteps(src); err == nil {
		t.Error("parseSteps accepted a -run pattern containing /; want an error")
	}
}

func TestTestFuncsFollowsBuildTag(t *testing.T) {
	t.Parallel()
	taggedSrc := "//go:build s3conformance\n\npackage x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\nfunc helper() {}\nfunc testLower(t *testing.T) {}\nfunc (x) TestMethod(t *testing.T) {}\n"
	untagged := "package x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"
	negated := "//go:build !s3conformance\n\npackage x\n\nfunc TestA(t *testing.T) {}\n"
	otherTag := "//go:build conformance\n\npackage x\n\nfunc TestA(t *testing.T) {}\n"

	got, err := testFuncs([]byte(taggedSrc))
	if err != nil || strings.Join(got, " ") != "TestA" {
		t.Errorf("tagged: got %v, %v; want [TestA]", got, err)
	}
	for name, src := range map[string]string{"untagged": untagged, "negated": negated, "otherTag": otherTag} {
		if got, _ := testFuncs([]byte(src)); len(got) != 0 {
			t.Errorf("%s: got %v; want none", name, got)
		}
	}
}

func TestUncoveredNamesTheMissingTests(t *testing.T) {
	t.Parallel()
	const file = "internal/x/x_conformance_test.go"
	tests := map[string][]testFunc{
		"./internal/x": {{file, "TestA"}, {file, "TestB"}, {file, "TestC"}},
		"./internal/y": {{file, "TestD"}},
	}
	steps := []step{
		{"./internal/x", "^(TestA|TestB)$"},
		{"./internal/y", ""},
	}
	msgs := uncovered(tests, steps)
	if len(msgs) != 1 || !strings.Contains(msgs[0], file) || !strings.Contains(msgs[0], "TestC") {
		t.Fatalf("uncovered = %q; want exactly one message naming %s and TestC", msgs, file)
	}
	// 同じ名前でも別パッケージの step では覆えない。
	if msgs := uncovered(tests, []step{{"./internal/y", ""}, {"./internal/x", "^TestA$"}}); len(msgs) != 2 {
		t.Errorf("uncovered with wrong package = %d messages; want 2 (TestB, TestC)", len(msgs))
	}
}

func stepStrings(steps []step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, fmt.Sprintf("%s run=%q", s.pkg, s.run))
	}
	return out
}

// parseSteps は ci.yml 全文から s3-conformance ジョブの go test の step を集める。
func parseSteps(src []byte) ([]step, error) {
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("ci.yml を解析できない: %w", err)
	}
	job, ok := doc.Jobs[ciJob]
	if !ok {
		return nil, fmt.Errorf("ci.yml に job %s が無い", ciJob)
	}
	var steps []step
	for _, s := range job.Steps {
		for _, line := range strings.Split(s.Run, "\n") {
			found, _ := parseGoTest(line)
			for _, st := range found {
				if _, err := regexp.Compile(st.run); err != nil {
					return nil, fmt.Errorf("-run の正規表現 %q が不正: %w", st.run, err)
				}
				// go test は -run を / で分けてサブテストに当てる。トップレベルの Test 名だけに当てる
				// この照合では読めないので、/ を含む形は受け付けない（足すなら照合を直す）。
				if strings.Contains(st.run, "/") {
					return nil, fmt.Errorf("-run %q に / がある。サブテストの形は読めないので、トップレベルの Test 名で書く", st.run)
				}
			}
			steps = append(steps, found...)
		}
	}
	return steps, nil
}

// parseGoTest は 1 行のシェルコマンドが `go test -tags s3conformance` なら、その step を返す。
// パッケージは "./" で始まる語で、-run は値を次の語（または =）で受ける。-timeout 5m のような
// 他のフラグは読み飛ばす。
func parseGoTest(line string) ([]step, bool) {
	// run 行の引用符の中に空白は無いので、空白で分けて両端の引用符を外せば足りる。
	args := strings.Fields(line)
	for k, a := range args {
		args[k] = strings.Trim(a, `'"`)
	}
	i := 0
	for i+1 < len(args) && (args[i] != "go" || args[i+1] != "test") {
		i++
	}
	if i+1 >= len(args) {
		return nil, false
	}
	var pkgs []string
	var run string
	tagged := false
	for j := i + 2; j < len(args); j++ {
		a := args[j]
		next := func() string {
			if j+1 < len(args) {
				j++
				return args[j]
			}
			return ""
		}
		switch {
		case a == "-tags":
			tagged = hasTag(next())
		case strings.HasPrefix(a, "-tags="):
			tagged = hasTag(strings.TrimPrefix(a, "-tags="))
		case a == "-run":
			run = next()
		case strings.HasPrefix(a, "-run="):
			run = strings.TrimPrefix(a, "-run=")
		case strings.HasPrefix(a, "./"):
			pkgs = append(pkgs, "./"+path.Clean(a))
		}
	}
	if !tagged {
		return nil, false
	}
	steps := make([]step, 0, len(pkgs))
	for _, p := range pkgs {
		steps = append(steps, step{pkg: p, run: run})
	}
	return steps, len(steps) > 0
}

func hasTag(list string) bool {
	for _, t := range strings.Split(list, ",") {
		if t == tag {
			return true
		}
	}
	return false
}

// collectTests は root 以下の s3conformance の _test.go から Test 関数を集め、
// パッケージ（"./internal/x" の形）ごとに返す。
func collectTests(root string) (map[string][]testFunc, error) {
	tests := map[string][]testFunc{}
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
		names, err := testFuncs(src)
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
		for _, n := range names {
			tests[dir] = append(tests[dir], testFunc{file: filepath.ToSlash(rel), name: n})
		}
		return nil
	})
	return tests, err
}

// testFuncs は src が s3conformance のタグで付くファイルなら、その Test 関数名を返す。
// 判定は `//go:build` の式を s3conformance だけ真として評価する。タグだけで決まる形
// （`//go:build s3conformance`）のファイルがこのリポジトリの全てで、他の GOOS 条件と組む形は無い。
func testFuncs(src []byte) ([]string, error) {
	if !tagged(src) {
		return nil, nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Recv == nil && isTestName(fn.Name.Name) {
			names = append(names, fn.Name.Name)
		}
	}
	return names, nil
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

// uncovered は、どの step にも当たらない Test の説明を返す。順序は決定的にする。
func uncovered(tests map[string][]testFunc, steps []step) []string {
	dirs := make([]string, 0, len(tests))
	for d := range tests {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var msgs []string
	for _, dir := range dirs {
		for _, tf := range tests[dir] {
			if covered(dir, tf.name, steps) {
				continue
			}
			msgs = append(msgs, fmt.Sprintf(
				"%s の %s は s3-conformance ジョブのどの step にも当たらず、CI で実行されない。"+
					"%s の %s ジョブに「go test -tags %s %s -run '^%s$' -count=1 -timeout 5m -v」の step を足す",
				tf.file, tf.name, ciName, ciJob, tag, dir, tf.name))
		}
	}
	return msgs
}

func covered(dir, name string, steps []step) bool {
	for _, st := range steps {
		if st.pkg != dir {
			continue
		}
		if st.run == "" {
			return true
		}
		// parseSteps が正規表現の妥当性を検査済みなので、エラーは無視してよい。
		if ok, _ := regexp.MatchString(st.run, name); ok {
			return true
		}
	}
	return false
}
