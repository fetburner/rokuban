// ci-extra-deps: ./internal/api
// ci-extra-deps: -tags=s3conformance,conformance ./internal/api
// ci-extra-files: .golangci.yml

// Package apideps はテストだけを持つ。api ロールの推移的な import に、禁止された
// パッケージが届いていないことを `go test ./...` で検査する（不変条件 1）。
//
// depguard の api-role は直接の import しか見ない。共有するパッケージに依存を 1 つ
// 足すだけで、api から禁止先へ推移的に届いても検出できないため、`go list -deps` の
// 結果を見る。禁止の一覧は `.golangci.yml` の api-role の deny から読む（2 か所に写さない）。
// 一覧の読み方が壊れて空で通ることを防ぐため、期待する項目を literal で固定している。
// DB は使わない。ビルドタグで依存が変わるので、タグ無しと s3conformance,conformance の両方を見る。
package apideps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

const apiPackage = "github.com/fetburner/rokuban/internal/api"

// wantDeny は api-role の deny に必ず載っているべき pkg の literal。
// 一覧を消したり、読み方が壊れたりしたときに空の一覧で通らないよう、ここに固定する。
var wantDeny = []string{
	"github.com/fetburner/rokuban/internal/worker",
	"github.com/fetburner/rokuban/internal/mirakc",
	"github.com/fetburner/rokuban/internal/mediapath",
	"github.com/fetburner/rokuban/internal/storemarker",
	"github.com/fetburner/rokuban/internal/ffargs",
	"github.com/fetburner/rokuban/internal/tsstat",
	"os/exec",
	"github.com/riverqueue/river",
}

// wantTags は検査するビルドタグの組。タグ無しと、タグ付きの conformance 系の両方を見る。
var wantTags = []string{"", "s3conformance,conformance"}

type golangciFile struct {
	Linters struct {
		Settings struct {
			Depguard struct {
				Rules map[string]struct {
					Deny []struct {
						Pkg string `yaml:"pkg"`
					} `yaml:"deny"`
				} `yaml:"rules"`
			} `yaml:"depguard"`
		} `yaml:"settings"`
	} `yaml:"linters"`
}

// apiRoleDeny は .golangci.yml の depguard の api-role の deny を返す。
func apiRoleDeny(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../.golangci.yml")
	if err != nil {
		t.Fatalf(".golangci.yml を読めない: %v", err)
	}
	var cfg golangciFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf(".golangci.yml を解析できない: %v", err)
	}
	rule, ok := cfg.Linters.Settings.Depguard.Rules["api-role"]
	if !ok {
		t.Fatalf(".golangci.yml の linters.settings.depguard.rules に api-role が無い（規則名が変わっていないか確認する）")
	}
	var deny []string
	for _, d := range rule.Deny {
		deny = append(deny, d.Pkg)
	}
	for _, want := range wantDeny {
		if !slices.Contains(deny, want) {
			t.Fatalf("api-role の deny に %q が無い。規則を消したなら、このテストの wantDeny も同じ PR で直す", want)
		}
	}
	return deny
}

// listed は `go list -deps -json` の 1 パッケージ分。
type listed struct {
	ImportPath string
	Imports    []string
}

// importGraph は api の推移的な import 関係（パッケージ → 直接の import）を、ビルドタグごとに返す。
func importGraph(t *testing.T, tags string) map[string][]string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("go が PATH に無い。このテストは go list を呼ぶ: %v", err)
	}
	args := []string{"list", "-deps", "-json"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./internal/api")
	cmd := exec.Command("go", args...)
	cmd.Dir = "../.."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	graph := make(map[string][]string)
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listed
		if err := dec.Decode(&p); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("go list の出力を解析できない: %v", err)
		}
		graph[p.ImportPath] = p.Imports
	}
	if _, ok := graph[apiPackage]; !ok {
		t.Fatalf("go list -deps の結果に %s が無い", apiPackage)
	}
	return graph
}

// violations は api から推移的に届く禁止パッケージを、import の経路つきで返す。
// 経路は幅優先で求めた最短のもの。禁止の判定は depguard と同じく前方一致。
func violations(graph map[string][]string, deny []string) []string {
	parent := map[string]string{apiPackage: ""}
	queue := []string{apiPackage}
	var out []string
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if pkg != apiPackage && matchesDeny(pkg, deny) {
			var chain []string
			for p := pkg; p != ""; p = parent[p] {
				chain = append([]string{p}, chain...)
			}
			out = append(out, strings.Join(chain, " → "))
		}
		for _, imp := range graph[pkg] {
			if _, seen := parent[imp]; !seen {
				parent[imp] = pkg
				queue = append(queue, imp)
			}
		}
	}
	return out
}

func matchesDeny(pkg string, deny []string) bool {
	for _, d := range deny {
		if strings.HasPrefix(pkg, d) {
			return true
		}
	}
	return false
}

func TestAPIRoleTransitiveImports(t *testing.T) {
	deny := apiRoleDeny(t)
	for _, tags := range wantTags {
		t.Run(fmt.Sprintf("tags=%q", tags), func(t *testing.T) {
			got := violations(importGraph(t, tags), deny)
			if len(got) > 0 {
				t.Errorf("api ロールの推移的な import に禁止されたパッケージが届いている（depguard の api-role は直接の import しか見ない）。経路を断つ:\n  %s",
					strings.Join(got, "\n  "))
			}
		})
	}
}
