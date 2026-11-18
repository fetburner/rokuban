// Package versioncheck はテストだけを持つ。リポジトリ内で手作業では揃わなくなる版の
// 組み合わせを `go test ./...` で検査するためのもの。CI の test ジョブが回す
// `go list ./...` の対象なので、専用の job は要らない。
//
// 検査するのは、ファイルの中身だけで判定できるもの（どのランナーでも同じ結果になる）:
//
//   - `.node-version` と `Dockerfile` の `FROM node:` の版
//   - `go.mod` の `go` ディレクティブと `Dockerfile` の `FROM golang:` の版
//   - `web/package.json` の `pnpm.overrides` の値はすべて上限（`<`）を持つ
package versioncheck

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestNodeVersionMatchesDockerfile(t *testing.T) {
	t.Parallel()
	want := strings.TrimSpace(readFile(t, "../../.node-version"))
	got := dockerfileVersion(t, "node")
	if got != want {
		t.Errorf(".node-version (%s) と Dockerfile の FROM node: (%s) が一致しない。"+
			"`.node-version` を上げたら `Dockerfile` の `FROM node:` も同じ版にする（CI は `.node-version` を読み、`Dockerfile` は読めない）",
			want, got)
	}
}

func TestGoDirectiveMatchesDockerfile(t *testing.T) {
	t.Parallel()
	want := goDirective(t, readFile(t, "../../go.mod"))
	got := dockerfileVersion(t, "golang")
	if got != want {
		t.Errorf("go.mod の go ディレクティブ (%s) と Dockerfile の FROM golang: (%s) が一致しない。"+
			"`go.mod` と `Dockerfile` を同じ版にする（揃えないとテストを通したコンパイラと出荷バイナリのコンパイラがずれる）。"+
			"Go を上げるときは `.github/workflows/ci.yml` の golangci-lint `version:` も同じ PR で上げる",
			want, got)
	}
}

func TestPnpmOverridesHaveUpperBound(t *testing.T) {
	t.Parallel()
	var pkg struct {
		Pnpm struct {
			Overrides map[string]string `json:"overrides"`
		} `json:"pnpm"`
	}
	if err := json.Unmarshal([]byte(readFile(t, "../../web/package.json")), &pkg); err != nil {
		t.Fatalf("web/package.json を解析できない: %v", err)
	}
	if len(pkg.Pnpm.Overrides) == 0 {
		t.Fatalf("web/package.json に pnpm.overrides が無い（キー名が変わっていないか確認する）")
	}
	names := make([]string, 0, len(pkg.Pnpm.Overrides))
	for name := range pkg.Pnpm.Overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if v := pkg.Pnpm.Overrides[name]; !strings.Contains(v, "<") {
			t.Errorf("web/package.json の pnpm.overrides[%q] = %q に上限が無い。"+
				"`>=下限 <次のメジャー` の形で上限を書く（例: `\"js-yaml\": \">=4.3.0 <5\"`）。上限が無いと次のメジャーが黙って注入される",
				name, v)
		}
	}
}

// dockerfileVersion は Dockerfile の `FROM` 行から image（`node` / `golang`）の版を返す。
// 版が 1 つに定まらなければ失敗する。
func dockerfileVersion(t *testing.T, image string) string {
	t.Helper()
	versions := fromVersions(readFile(t, "../../Dockerfile"), image)
	if len(versions) != 1 {
		t.Fatalf("Dockerfile の FROM %s: の版が 1 つに定まらない（%d 件: %v）", image, len(versions), versions)
	}
	return versions[0]
}

// fromVersions は Dockerfile の全文 src から image の版を集める。
// `FROM --platform=$BUILDPLATFORM node:24.20.0 AS frontend` のように --flag と AS 句が
// 付いても、イメージ参照（空白区切りで最初の --flag でない語）だけを見る。
// `FROM ${BASE}` のように版を持たない行は対象外。
func fromVersions(src, image string) []string {
	var versions []string
	for _, line := range strings.Split(src, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		var ref string
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "--") {
				ref = f
				break
			}
		}
		if name, version, ok := strings.Cut(ref, ":"); ok && name == image {
			versions = append(versions, version)
		}
	}
	return versions
}

// goDirective は go.mod 全文 src の `go` ディレクティブの版を返す。
func goDirective(t *testing.T, src string) string {
	t.Helper()
	for _, line := range strings.Split(src, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			return fields[1]
		}
	}
	t.Fatalf("go.mod に go ディレクティブが無い")
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s を読めない: %v", path, err)
	}
	return string(b)
}
