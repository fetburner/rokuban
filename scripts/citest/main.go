// ci-extra-deps: none

// Command citest は変更ファイルから CI で回す Go パッケージを選ぶ。
// モジュールルートで go run ./scripts/citest -tags conformance ./internal/mirakc/conformance/... を実行する。
// 標準入力には git diff --name-only -z --no-renames のように NUL 区切りの相対パスを渡す。
// -base には PR の base の commit を渡す。go.mod・go.sum の変更で、版が変わったモジュールだけを
// 依存に持つパッケージを選ぶために使う。空なら go.mod・go.sum の変更は全件になる。
// 標準出力は選ばれたパッケージの import path を 1 行に 1 つずつ返す。
// -body-only を付けると、テスト対象の _test.go の import 先（テストの依存）を除き、
// 実行されるコードの依存だけで判定する。対象パッケージ自身の _test.go と testdata は含む。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	tags := flag.String("tags", "", "comma-separated Go build tags")
	bodyOnly := flag.Bool("body-only", false, "テストの依存を除き、本体の依存だけで判定する")
	base := flag.String("base", "", "git revision of the PR base; go.mod and go.sum changes are judged against it")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "citest: specify at least one Go package pattern")
		os.Exit(2)
	}
	if err := run(os.Stdin, os.Stdout, *tags, *base, *bodyOnly, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer, tags, base string, bodyOnly bool, patterns []string) error {
	raw, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read changed files: %w", err)
	}
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("find module root: %w", err)
	}
	selected, err := selectPackages(root, tags, base, bodyOnly, patterns, strings.Split(string(raw), "\x00"))
	if err != nil {
		return err
	}
	for _, pkg := range selected {
		if _, err := fmt.Fprintln(out, pkg); err != nil {
			return fmt.Errorf("write selected packages: %w", err)
		}
	}
	return nil
}

type listedPackage struct {
	ImportPath string
	Dir        string
	Module     *struct {
		Path string
		Main bool
	}
	GoFiles            []string
	CgoFiles           []string
	TestGoFiles        []string
	XTestGoFiles       []string
	EmbedPatterns      []string
	TestEmbedPatterns  []string
	XTestEmbedPatterns []string
}

// 変更は祖先ディレクトリごとに相対パスで引く。通常のソース、embed、testdata の
// 判定は同じ一覧を使う。
type changedFiles map[string][]string

func selectPackages(root, tags, base string, bodyOnly bool, patterns, changed []string) ([]string, error) {
	files := make(changedFiles)
	var paths []string
	all, goMod, goSum := false, false, false
	for _, name := range changed {
		if name == "" {
			continue
		}
		name = path.Clean(filepath.ToSlash(name))
		paths = append(paths, name)
		all = all || requiresAllPackages(name)
		goMod = goMod || name == "go.mod"
		goSum = goSum || name == "go.sum"
		dir, rel := path.Dir(name), path.Base(name)
		for {
			absDir := filepath.Join(root, filepath.FromSlash(dir))
			files[absDir] = append(files[absDir], rel)
			if dir == "." || dir == "/" {
				break
			}
			rel, dir = path.Join(path.Base(dir), rel), path.Dir(dir)
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	modules := make(map[string]bool)
	if (goMod || goSum) && !all {
		if base == "" {
			all = true
		} else {
			changedModules, full, err := moduleChanges(root, base, goMod, goSum)
			if err != nil {
				return nil, err
			}
			all, modules = full, changedModules
		}
	}

	targets, err := goList(root, tags, "-json=ImportPath", patterns...)
	if err != nil {
		return nil, err
	}
	check := packageSelection{
		root: root, changed: files, paths: paths, bodyOnly: bodyOnly, modules: modules,
		declarations: make(map[string]declarations),
		extraMatches: make(map[extraDependency]bool),
	}
	var selected []string
	for _, target := range targets {
		if !all {
			affected, err := check.affectedPackage(tags, target.ImportPath)
			if err != nil {
				return nil, err
			}
			if !affected {
				continue
			}
		}
		selected = append(selected, target.ImportPath)
	}
	slices.Sort(selected)
	return slices.Compact(selected), nil
}

// githubAllPackagesPaths は .github/ のうち変更で全件を回すパス。ci.yml の plan ジョブの
// paths-filter にある .github/ 始まりのエントリと同じ glob 表記で書く。両者の一致は
// TestBrowserE2EPathFiltersMatchAllPackagesRule が YAML として検査する。
var githubAllPackagesPaths = []string{".github/workflows/ci.yml", ".github/actions/**"}

// requiresAllPackages は依存グラフだけで絞れない変更を扱う。citest 自身の変更も
// 全件にし、判定の変更が同じ PR の検証を外すことを防ぐ。他のワークフロー（publish-sha.yml など）と
// dependabot.yml は CI の判定に効かない。go.mod・go.sum は selectPackages が版の変化で判定する。
func requiresAllPackages(name string) bool {
	if strings.HasPrefix(name, "scripts/citest/") {
		return true
	}
	return slices.ContainsFunc(githubAllPackagesPaths, func(pattern string) bool {
		return matchGitHubPath(name, pattern)
	})
}

// matchGitHubPath は paths-filter の glob のうち、完全一致と末尾の /** の配下だけを扱う。
func matchGitHubPath(name, pattern string) bool {
	if dir, ok := strings.CutSuffix(pattern, "/**"); ok {
		return strings.HasPrefix(name, dir+"/")
	}
	return name == pattern
}

type packageSelection struct {
	root    string
	changed changedFiles
	// bodyOnly は対象のテストの依存（go list -test が加える派生パッケージ）を依存グラフから外す。
	bodyOnly bool
	// modules は版が変わったモジュールのパス。そのモジュールのパッケージを依存に持つパッケージを選ぶ。
	modules map[string]bool
	// paths は変更されたファイルのモジュールルートからの相対パス。ci-extra-files の照合に使う。
	paths        []string
	declarations map[string]declarations
	extraMatches map[extraDependency]bool
}

// EmbedPatterns を使うと、最後の一致ファイルを削除した後も Go にパターンを問い合わせられる。
const graphFields = "-json=ImportPath,Dir,Module,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles,EmbedPatterns,TestEmbedPatterns,XTestEmbedPatterns"

func (s packageSelection) affectedPackage(tags, target string) (bool, error) {
	deps := []string{"-deps", "-test", target}
	if s.bodyOnly {
		// go list -deps の対象自身の TestGoFiles・XTestGoFiles は残るので、対象の _test.go は判定に入る。
		deps = []string{"-deps", target}
	}
	graph, err := goList(s.root, tags, graphFields, deps...)
	if err != nil {
		return false, err
	}
	for _, pkg := range graph {
		if s.selects(pkg, pkg.ImportPath == target) {
			return true, nil
		}
	}

	for _, pkg := range graph {
		if pkg.Module == nil || !pkg.Module.Main {
			continue
		}
		affected, err := s.matchesDeclarations(pkg, pkg.ImportPath == target)
		if err != nil {
			return false, err
		}
		if affected {
			return true, nil
		}
	}
	return false, nil
}

func (s packageSelection) matchesDeclarations(pkg listedPackage, target bool) (bool, error) {
	files := slices.Concat(pkg.GoFiles, pkg.CgoFiles)
	// go list -test が作る派生パッケージの GoFiles にもテストファイルが現れる。
	files = slices.DeleteFunc(files, func(name string) bool {
		return filepath.IsAbs(name) || strings.HasSuffix(name, "_test.go")
	})
	if target {
		files = append(files, pkg.TestGoFiles...)
		files = append(files, pkg.XTestGoFiles...)
	}
	for _, name := range files {
		filename := filepath.Join(pkg.Dir, name)
		decls, seen := s.declarations[filename]
		if !seen {
			var err error
			_, decls, err = readDeclarations(filename)
			if err != nil {
				return false, err
			}
			s.declarations[filename] = decls
		}
		for _, dep := range decls.deps {
			match, err := s.matchesExtra(dep)
			if err != nil || match {
				return match, err
			}
		}
		for _, decl := range decls.files {
			if s.matchesFile(decl) {
				return true, nil
			}
		}
	}
	return false, nil
}

// matchesFile は ci-extra-files の 1 つのパスに、変更されたファイルが一致するかを返す。
// ディレクトリは配下のファイルに一致する。名前の前方一致だけでは別のディレクトリ（testdata と testdata2）にも一致するので、区切りで判定する。
func (s packageSelection) matchesFile(decl string) bool {
	return slices.ContainsFunc(s.paths, func(name string) bool {
		return name == decl || strings.HasPrefix(name, decl+"/")
	})
}

func (s packageSelection) matchesExtra(dep extraDependency) (bool, error) {
	if match, ok := s.extraMatches[dep]; ok {
		return match, nil
	}
	graph, err := goList(s.root, dep.tags, "-json=Dir,EmbedPatterns,Module", "-deps", dep.pattern)
	if err != nil {
		return false, err
	}
	match := slices.ContainsFunc(graph, func(pkg listedPackage) bool { return s.selects(pkg, false) })
	s.extraMatches[dep] = match
	return match, nil
}

// selects は pkg が選定の根拠になるかを返す。変更ファイルが pkg の選定に効くとき、または pkg が
// 版の変わったモジュールのパッケージのとき。依存グラフの判定（affectedPackage）と ci-extra-deps の
// 宣言先の判定（matchesExtra）は、どちらもこの述語を使う。片方だけ直す形にしないため。
func (s packageSelection) selects(pkg listedPackage, target bool) bool {
	return s.changed.matches(pkg, target) || pkg.Module != nil && s.modules[pkg.Module.Path]
}

// matches は、pkg のディレクトリで変わったファイルが pkg の選定に効くかを返す。
// 直下のファイルと埋め込みは常に一致させるが、_test.go はテスト対象（target）のときだけ一致させる。依存先の _test.go は依存先自身のテストバイナリにしか入らないため。testdata/ とテストの埋め込みもテスト対象のときだけ一致させる。
func (files changedFiles) matches(pkg listedPackage, target bool) bool {
	for _, rel := range files[pkg.Dir] {
		if !strings.Contains(rel, "/") && (target || !strings.HasSuffix(rel, "_test.go")) || matchesEmbed(rel, pkg.EmbedPatterns) {
			return true
		}
		if target && (strings.HasPrefix(rel, "testdata/") ||
			matchesEmbed(rel, pkg.TestEmbedPatterns) || matchesEmbed(rel, pkg.XTestEmbedPatterns)) {
			return true
		}
	}
	return false
}

// matchesEmbed はファイルそのものと、ディレクトリに一致して再帰的に埋め込む
// パターンを扱う。後者のドット・アンダースコアで始まる要素は all: があるときに含む。
func matchesEmbed(rel string, patterns []string) bool {
	for _, pattern := range patterns {
		pattern, all := strings.CutPrefix(pattern, "all:")
		for name := rel; name != "."; name = path.Dir(name) {
			if matched, _ := path.Match(pattern, name); matched {
				return true
			}
			base := path.Base(name)
			if !all && (strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_")) {
				break
			}
		}
	}
	return false
}

func goList(root, tags, fields string, args ...string) ([]listedPackage, error) {
	args = append([]string{"list", "-tags", tags, fields}, args...)
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var packages []listedPackage
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			return packages, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode go list packages: %w", err)
		}
		packages = append(packages, pkg)
	}
}
