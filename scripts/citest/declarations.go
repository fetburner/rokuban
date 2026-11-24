package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"slices"
	"strconv"
	"strings"
)

type extraDependency struct {
	tags    string
	pattern string
}

// declarations は .go ファイルの package 節より前の宣言をまとめて表す。
type declarations struct {
	deps []extraDependency
	none bool
	// files は ci-extra-files の宣言。モジュールルートからの相対パスで、ディレクトリは配下全体を表す。
	files []string
}

// declaresDeps は ci-extra-deps の宣言（none を含む）があるかを返す。
func (d declarations) declaresDeps() bool {
	return d.none || len(d.deps) > 0
}

// readDeclarations は .go ファイルの package 節より前の `// ci-extra-deps:` と `// ci-extra-files:` を読む。
// ci-extra-deps の 1 行には任意の -tags=tag1,tag2 と、モジュールルートから go list に渡す空白区切りの
// パッケージ指定を書く。複数行でサブプロセスごとのタグを表せる。タグの省略はタグ無しを表す。
// 固定の追加依存が無い場合は `// ci-extra-deps: none` だけを書く。例えば citest の
// go list の対象は CLI の入力と、そこから辿った宣言で決まる。
// ci-extra-files は、実行時にパッケージの外のファイルを読むテストが書く。空白区切りの
// モジュールルートからの相対パスで、ファイルはそのファイルの変更に、ディレクトリは配下のどの変更にも一致する。
// 一致した変更があるとパッケージを選ぶ。
// 呼び出しと宣言を同じファイルに置けば、呼び出しを移しても依存が付いていく。コメントなら
// ビルドタグに関係なく、型検査や依存の取得をせずに検査できる。
func readDeclarations(path string) (*ast.File, declarations, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, declarations{}, fmt.Errorf("parse %s: %w", path, err)
	}
	var decls declarations
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, comment := range group.List {
			if value, ok := strings.CutPrefix(comment.Text, "// ci-extra-files:"); ok {
				files, err := parseExtraFiles(value)
				if err != nil {
					return nil, declarations{}, fmt.Errorf("%s: %w", path, err)
				}
				decls.files = append(decls.files, files...)
				continue
			}
			value, ok := strings.CutPrefix(comment.Text, "// ci-extra-deps:")
			if !ok {
				continue
			}
			if strings.TrimSpace(value) == "none" {
				decls.none = true
				continue
			}
			line, err := parseExtraDependencies(value)
			if err != nil {
				return nil, declarations{}, fmt.Errorf("%s: %w", path, err)
			}
			decls.deps = append(decls.deps, line...)
		}
	}
	if decls.none && len(decls.deps) > 0 {
		return nil, declarations{}, fmt.Errorf("%s: ci-extra-deps: none とパッケージ指定は併用できません", path)
	}
	return file, decls, nil
}

// parseExtraFiles は ci-extra-files の値を読む。絶対パスと `..` を含むパスは、
// 変更との照合が root の外を指すので拒む。モジュールルート（.）は全変更に一致して
// 意味が広すぎるので、現在は使う宣言が無く拒む。
func parseExtraFiles(value string) ([]string, error) {
	words := strings.Fields(value)
	if len(words) == 0 {
		return nil, fmt.Errorf("ci-extra-files: パス指定がありません")
	}
	files := make([]string, 0, len(words))
	for _, word := range words {
		clean := path.Clean(word)
		if path.IsAbs(word) || slices.Contains(strings.Split(word, "/"), "..") || clean == "." {
			return nil, fmt.Errorf("ci-extra-files: 不正なパス指定 %q", word)
		}
		files = append(files, clean)
	}
	return files, nil
}

func parseExtraDependencies(value string) ([]extraDependency, error) {
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
	deps := make([]extraDependency, 0, len(words))
	for _, word := range words {
		if word == "none" || strings.HasPrefix(word, "-") {
			return nil, fmt.Errorf("ci-extra-deps: 不正なパッケージ指定 %q", word)
		}
		deps = append(deps, extraDependency{tags: tags, pattern: word})
	}
	return deps, nil
}

// callsGo は os/exec の Command と CommandContext にリテラルの "go" を渡す形を検査する。
// 検査はリテラルを前提にするので、exec.Command(goBin, …) のような変数経由は見落とす。
func callsGo(file *ast.File) bool {
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		if path != "os/exec" {
			continue
		}
		name := "exec"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		for node := range ast.Preorder(file) {
			if call, ok := node.(*ast.CallExpr); ok && callsGoCommand(call, name) {
				return true
			}
		}
	}
	return false
}

func callsGoCommand(call *ast.CallExpr, execName string) bool {
	var method string
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == execName {
			method = fun.Sel.Name
		}
	case *ast.Ident:
		if execName == "." {
			method = fun.Name
		}
	}

	var index int
	switch method {
	case "Command":
	case "CommandContext":
		index = 1 // context.Context の次がコマンド名。
	default:
		return false
	}
	if len(call.Args) <= index {
		return false
	}
	arg, ok := ast.Unparen(call.Args[index]).(*ast.BasicLit)
	if !ok || arg.Kind != token.STRING {
		return false
	}
	value, _ := strconv.Unquote(arg.Value)
	return value == "go"
}
