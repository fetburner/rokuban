// Package cideps は Go ファイルの ci-extra-deps 宣言を解析する。
package cideps

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// Dependency は ci-extra-deps で宣言された Go パッケージ依存を表す。
type Dependency struct {
	tags    string
	pattern string
}

// Tags は依存に指定されたビルドタグを返す。
func (dep Dependency) Tags() string { return dep.tags }

// Pattern は依存の Go パッケージ指定を返す。
func (dep Dependency) Pattern() string { return dep.pattern }

// Read は Go ファイルの package 節より前にある ci-extra-deps 宣言を読む。
// 1 行に任意の -tags=tag1,tag2 と、モジュールルートから go list に渡す空白区切りの
// パッケージ指定を書く。複数行でサブプロセスごとのタグを表せる。タグの省略はタグ無しを表す。
// 固定の追加依存が無い場合は `// ci-extra-deps: none` だけを書く。例えば citest の
// go list の対象は CLI の入力と、そこから辿った宣言で決まる。
// 呼び出しと宣言を同じファイルに置けば、呼び出しを移しても依存が付いていく。コメントなら
// ビルドタグに関係なく、型検査や依存の取得をせずに検査できる。
func Read(path string) (*ast.File, []Dependency, bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	var deps []Dependency
	none := false
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			value, ok := strings.CutPrefix(comment.Text, "// ci-extra-deps:")
			if !ok {
				continue
			}
			if strings.TrimSpace(value) == "none" {
				none = true
				continue
			}
			line, err := Parse(value)
			if err != nil {
				return nil, nil, false, fmt.Errorf("%s: %w", path, err)
			}
			deps = append(deps, line...)
		}
	}
	if none && len(deps) > 0 {
		return nil, nil, false, fmt.Errorf("%s: ci-extra-deps: none とパッケージ指定は併用できません", path)
	}
	return file, deps, none || len(deps) > 0, nil
}

// Parse は ci-extra-deps 宣言 1 行のタグとパッケージ指定を解析する。
func Parse(value string) ([]Dependency, error) {
	words := strings.Fields(value)
	var tags string
	if len(words) > 0 {
		if value, ok := strings.CutPrefix(words[0], "-tags="); ok {
			tags, words = value, words[1:]
		}
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("ci-extra-deps: パッケージ指定がありません")
	}
	deps := make([]Dependency, 0, len(words))
	for _, word := range words {
		if word == "none" || strings.HasPrefix(word, "-") {
			return nil, fmt.Errorf("ci-extra-deps: 不正なパッケージ指定 %q", word)
		}
		deps = append(deps, Dependency{tags: tags, pattern: word})
	}
	return deps, nil
}
