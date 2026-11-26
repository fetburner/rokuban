// ci-extra-files: .github/workflows/ci.yml package.json

// Package docslintpaths はテストだけを持つ。ci.yml の docs-lint の paths-filter の `md` と `md_lint`
// （変更された Markdown の検査に使う、added|modified だけの一覧）が、package.json の `docs:lint` の対象
// （シングルクォートで囲んだ glob の全部）と順序を除いて一致することを検査する。一覧が複数あって片方だけ直すと、
// 対象の Markdown を変えた PR で lint が回らなくなる。この検査は test ジョブが回す `go list ./...` の対象なので、専用の job は要らない。
package docslintpaths

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

const (
	ciPath  = "../../.github/workflows/ci.yml" // テストのカレントディレクトリからの相対パス
	pkgPath = "../../package.json"
	jobName = "docs-lint"
)

// docsLintFilters は docs-lint の paths-filter の filters（YAML の文字列）を読んだ形。
// md_lint は変更種別付きの要素を 1 つだけ持つ前提で、要素が合わなければ解析の段階で落ちる。
type docsLintFilters struct {
	MD     []string              `yaml:"md"`
	MDLint []map[string][]string `yaml:"md_lint"`
}

func TestDocsLintFilterMatchesDocsLintScript(t *testing.T) {
	t.Parallel()
	want := docsLintTargets(t)
	f := readDocsLintFilters(t)

	md := append([]string(nil), f.MD...)
	sort.Strings(md)
	if !reflect.DeepEqual(want, md) {
		t.Errorf("ci.yml の docs-lint の paths-filter の md の一覧が package.json の docs:lint の対象と一致しない\n"+
			"  docs:lint:    %q\n  paths-filter: %q\n"+
			"`package.json` の `docs:lint` を変えたら `.github/workflows/ci.yml` の docs-lint の `md` の filter も同じ一覧にする",
			want, md)
	}

	// md_lint は added|modified だけの要素を 1 つ持つ。added だけに変えると modified の Markdown が lint から漏れる。
	if len(f.MDLint) != 1 || len(f.MDLint[0]) != 1 || f.MDLint[0]["added|modified"] == nil {
		t.Fatalf("ci.yml の docs-lint の paths-filter の md_lint は、変更種別 added|modified だけの要素を 1 つ持つ一覧であるべき: %v", f.MDLint)
	}
	changed := append([]string(nil), f.MDLint[0]["added|modified"]...)
	sort.Strings(changed)
	if !reflect.DeepEqual(want, changed) {
		t.Errorf("ci.yml の docs-lint の paths-filter の md_lint（added|modified）が package.json の docs:lint の対象と一致しない\n"+
			"  docs:lint:    %q\n  paths-filter: %q\n"+
			"md_lint は md の一覧を YAML アンカーで共有する形にする",
			want, changed)
	}
}

// docsLintTargets は package.json の docs:lint の引数（シングルクォートで囲んだ glob の全部）を、
// 順序を除いてソートして返す。
func docsLintTargets(t *testing.T) []string {
	t.Helper()
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(readFile(t, pkgPath)), &pkg); err != nil {
		t.Fatalf("package.json を解析できない: %v", err)
	}
	script, ok := pkg.Scripts["docs:lint"]
	if !ok {
		t.Fatalf("package.json に scripts.docs:lint が無い（キー名が変わっていないか確認する）")
	}
	var globs []string
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(script, -1) {
		globs = append(globs, m[1])
	}
	if len(globs) == 0 {
		t.Fatalf("docs:lint に対象の glob が無い: %q", script)
	}
	sort.Strings(globs)
	return globs
}

// readDocsLintFilters は ci.yml の docs-lint job の paths-filter の filters を読む。
// filters の文字列は YAML なので、もう一段 YAML として読む。
func readDocsLintFilters(t *testing.T) docsLintFilters {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, ciPath)), &wf); err != nil {
		t.Fatalf("ci.yml を解析できない: %v", err)
	}
	job, ok := wf.Jobs[jobName]
	if !ok {
		t.Fatalf("ci.yml に job %s が無い", jobName)
	}
	var filters string
	for _, step := range job.Steps {
		if !strings.HasPrefix(step.Uses, "dorny/paths-filter@") {
			continue
		}
		s, ok := step.With["filters"].(string)
		if !ok {
			t.Fatalf("job %s の paths-filter の filters が文字列ではない", jobName)
		}
		filters = s
	}
	if filters == "" {
		t.Fatalf("job %s に dorny/paths-filter の step が無い", jobName)
	}
	var f docsLintFilters
	if err := yaml.Unmarshal([]byte(filters), &f); err != nil {
		t.Fatalf("job %s の filters を解析できない: %v", jobName, err)
	}
	return f
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s を読めない: %v", path, err)
	}
	return string(b)
}
