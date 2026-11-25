// ci-extra-files: .github/workflows/ci.yml package.json

// Package docslintpaths はテストだけを持つ。ci.yml の docs-lint の paths-filter に書いた
// Markdown の一覧が、package.json の `docs:lint` の対象と一致することを検査する。
// 一覧が 2 か所にあって片方だけ直すと、対象の Markdown を変えた PR で lint が回らなくなる。
// この検査は test ジョブが回す `go list ./...` の対象なので、専用の job は要らない。
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
	filter  = "docs" // ci.yml の filters のキー名。steps.changes.outputs.docs と対応する
)

func TestDocsLintFilterMatchesDocsLintScript(t *testing.T) {
	t.Parallel()
	want := docsLintTargets(t)
	got := docsLintFilterMarkdown(t)
	if reflect.DeepEqual(want, got) {
		return
	}
	t.Errorf("ci.yml の docs-lint の paths-filter の Markdown 一覧が package.json の docs:lint の対象と一致しない\n"+
		"  docs:lint:    %q\n  paths-filter: %q\n"+
		"`package.json` の `docs:lint` を変えたら `.github/workflows/ci.yml` の docs-lint の `filters` も同じ一覧にする",
		want, got)
}

// docsLintTargets は package.json の docs:lint の引数のうち、Markdown の glob を返す。
// 引数は単一引用符で囲まれている前提（`docs:lint` の書き方）。
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
	return sortedMarkdown(globs)
}

// docsLintFilterMarkdown は ci.yml の docs-lint job の paths-filter から、`.md` で終わる
// パターンを返す。filter の文字列は YAML なので、もう一段 YAML として読む。
func docsLintFilterMarkdown(t *testing.T) []string {
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
	var parsed map[string][]string
	if err := yaml.Unmarshal([]byte(filters), &parsed); err != nil {
		t.Fatalf("job %s の filters を解析できない: %v", jobName, err)
	}
	patterns, ok := parsed[filter]
	if !ok {
		t.Fatalf("job %s の filters に %q のキーが無い", jobName, filter)
	}
	return sortedMarkdown(patterns)
}

func sortedMarkdown(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.HasSuffix(s, ".md") {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s を読めない: %v", path, err)
	}
	return string(b)
}
