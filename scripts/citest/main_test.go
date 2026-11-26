// ci-source-deps: -tags=conformance,s3conformance,ruleguard ./...
// 全パッケージの .go（_test.go を含む）を読んで検査するので、ソースを読む宣言にする。
// タグを並べるのは、タグ付きのファイルしか無いディレクトリ（例: ruleguard だけの tools/lintrules）も列挙させるため。
// ignore は並べない。ignore のファイルが同じディレクトリの他のファイルと別のパッケージ（生成用の main など）だと、
// go list が found packages で失敗するため（一時モジュールで確認）。そのため、ignore のファイルしか無い
// ディレクトリの変更では選ばれない。

package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func writeSelectionFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	// 実物の go list に渡せる最小モジュール。rokuban 本体のビルドや web/dist は不要。
	root := t.TempDir()
	t.Setenv("GOWORK", "off")
	files := map[string]string{
		"go.mod":                         "module example.com/fixture\n\ngo 1.24\n",
		"internal/mirakc/client.go":      "package mirakc\n",
		"internal/mirakc/client_test.go": "package mirakc\n",
		"internal/worker/worker.go":      "package worker\n",
		"internal/worker/worker_test.go": "package worker\n",
		"internal/db/migrations/001.sql": "SELECT 1;",
		"internal/db/migrate.go": `package db

import _ "embed"

//go:embed migrations/*.sql
var migration string
`,
		"cmd/rokuban/main.go": `package main

import (
	_ "example.com/fixture/internal/db"
	_ "example.com/fixture/internal/worker"
)

func main() {}
`,
		"cmd/fixturetuner/main.go": `//go:build conformance

package main

func main() {}
`,
		"internal/harness/tuner.go": `//go:build conformance

// ci-extra-deps: -tags=conformance ./cmd/fixturetuner

package harness

import "os/exec"

func Build() error { return exec.Command("go", "build", "-tags", "conformance", "./cmd/fixturetuner").Run() }
`,
		"conformance/lifecycle/testdata/fixture.json": "{}",
		"conformance/lifecycle/embedded/data.txt":     "fixture",
		"conformance/lifecycle/lifecycle_test.go": `//go:build conformance

package lifecycle_test

import (
	_ "embed"
	_ "example.com/fixture/internal/harness"
	_ "example.com/fixture/internal/mirakc"
)

//go:embed embedded
var data string
`,
		"conformance/ingestresume/resume_test.go": `//go:build conformance

// ci-extra-deps: ./cmd/rokuban

package ingestresume
`,
		"conformance/fileread/read_test.go": `// ci-extra-files: ./shared/input.txt

package fileread
`,
		"conformance/dirread/read_test.go": `// ci-extra-files: testdata/golden

package dirread
`,
		"conformance/depread/read_test.go": `package depread

import _ "example.com/fixture/internal/shared"
`,
		"internal/shared/shared.go": `// ci-extra-files: schema.sql

package shared
`,
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, files
}

// allConformance は fixture の conformance パッケージ全件（全件選択になる変更の期待値）。
var allConformance = []string{
	"example.com/fixture/conformance/depread",
	"example.com/fixture/conformance/dirread",
	"example.com/fixture/conformance/fileread",
	"example.com/fixture/conformance/ingestresume",
	"example.com/fixture/conformance/lifecycle",
}

func TestSelectPackages(t *testing.T) {
	root, files := writeSelectionFixture(t)

	tests := []struct {
		name    string
		changed string
		deleted bool
		want    []string
	}{
		{
			name:    "test import",
			changed: "internal/mirakc/client.go",
			want:    []string{"example.com/fixture/conformance/lifecycle"},
		},
		{
			name:    "dependency test file change does not select its importer",
			changed: "internal/mirakc/client_test.go",
			want:    nil,
		},
		{
			name:    "dependency test file deletion does not select its importer",
			changed: "internal/mirakc/client_test.go",
			deleted: true,
			want:    nil,
		},
		{
			name:    "own internal test file selects the package",
			changed: "conformance/fileread/read_test.go",
			want:    []string{"example.com/fixture/conformance/fileread"},
		},
		{
			name:    "own external test file selects the package",
			changed: "conformance/lifecycle/lifecycle_test.go",
			want:    []string{"example.com/fixture/conformance/lifecycle"},
		},
		{
			name:    "dependency test file does not select a ci-extra-deps declarer",
			changed: "internal/worker/worker_test.go",
			want:    nil,
		},
		{
			name:    "unrelated directory",
			changed: "docs/runbook/testing.md",
			want:    nil,
		},
		{
			name:    "ingestresume subprocess dependency",
			changed: "internal/worker/worker.go",
			want:    []string{"example.com/fixture/conformance/ingestresume"},
		},
		{
			name:    "fixturetuner dependency declared by imported harness",
			changed: "cmd/fixturetuner/main.go",
			want:    []string{"example.com/fixture/conformance/lifecycle"},
		},
		{
			name:    "subprocess embed change",
			changed: "internal/db/migrations/001.sql",
			want:    []string{"example.com/fixture/conformance/ingestresume"},
		},
		{
			name:    "subprocess embed deletion",
			changed: "internal/db/migrations/001.sql",
			deleted: true,
			want:    []string{"example.com/fixture/conformance/ingestresume"},
		},
		{
			name:    "external test embed directory",
			changed: "conformance/lifecycle/embedded/data.txt",
			want:    []string{"example.com/fixture/conformance/lifecycle"},
		},
		{
			name:    "package testdata",
			changed: "conformance/lifecycle/testdata/fixture.json",
			want:    []string{"example.com/fixture/conformance/lifecycle"},
		},
		{
			name:    "module definition",
			changed: "go.mod",
			want:    allConformance,
		},
		{
			name:    "module checksums",
			changed: "go.sum",
			want:    allConformance,
		},
		{
			name:    "GitHub workflow ci.yml",
			changed: ".github/workflows/ci.yml",
			want:    allConformance,
		},
		{name: "GitHub composite action", changed: ".github/actions/install-apt-cached/action.yml", want: allConformance},
		{name: "GitHub publish-sha workflow does not select everything", changed: ".github/workflows/publish-sha.yml", want: nil},
		{name: "dependabot config does not select everything", changed: ".github/dependabot.yml", want: nil},
		{
			name:    "selector change",
			changed: "scripts/citest/main.go",
			want:    allConformance,
		},
		{
			name:    "ci-extra-files file declaration matches the changed file",
			changed: "shared/input.txt",
			want:    []string{"example.com/fixture/conformance/fileread"},
		},
		{
			name:    "ci-extra-files file declaration does not match a sibling file",
			changed: "shared/other.txt",
			want:    nil,
		},
		{
			name:    "ci-extra-files directory declaration matches beneath it",
			changed: "testdata/golden/case1/out.json",
			want:    []string{"example.com/fixture/conformance/dirread"},
		},
		{
			name:    "ci-extra-files directory declaration does not match a name prefix",
			changed: "testdata/goldenx/a.json",
			want:    nil,
		},
		{
			name:    "ci-extra-files declared in a non-test file of a dependency",
			changed: "schema.sql",
			want:    []string{"example.com/fixture/conformance/depread"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.deleted {
				file := filepath.Join(root, tt.changed)
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(file, []byte(files[tt.changed]), 0o600); err != nil {
						t.Error(err)
					}
				})
			}
			got, err := selectPackages(root, "conformance", "", false, []string{"./conformance/..."}, []string{tt.changed})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("packages for %q = %v, want %v", tt.changed, got, tt.want)
			}
		})
	}
}

// TestBodyOnlySelection は -body-only の判定範囲を、テストの依存を含む既定の判定と並べて確かめる。
// 期待値はリテラルで書き、既定の判定の結果も同時に見ることで、-body-only が既定を壊していないことも確かめる。
func TestBodyOnlySelection(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GOWORK", "off")
	files := map[string]string{
		"go.mod":                        "module example.com/fixture\n\ngo 1.24\n",
		"internal/body/body.go":         "package body\n\nimport _ \"example.com/fixture/internal/dep\"\n",
		"internal/body/body_test.go":    "package body\n\nimport _ \"example.com/fixture/internal/testonly\"\n",
		"internal/dep/dep.go":           "package dep\n",
		"internal/testonly/testonly.go": "package testonly\n",
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const body = "example.com/fixture/internal/body"
	tests := []struct {
		name        string
		changed     string
		wantDefault []string
		wantBody    []string
	}{
		{name: "test-only dependency", changed: "internal/testonly/testonly.go", wantDefault: []string{body}, wantBody: nil},
		{name: "body dependency", changed: "internal/dep/dep.go", wantDefault: []string{body}, wantBody: []string{body}},
		{name: "target own test file", changed: "internal/body/body_test.go", wantDefault: []string{body}, wantBody: []string{body}},
		{name: "target source file", changed: "internal/body/body.go", wantDefault: []string{body}, wantBody: []string{body}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectPackages(root, "", "", false, []string{"./internal/body"}, []string{tt.changed})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.wantDefault) {
				t.Errorf("default packages for %q = %v, want %v", tt.changed, got, tt.wantDefault)
			}
			got, err = selectPackages(root, "", "", true, []string{"./internal/body"}, []string{tt.changed})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.wantBody) {
				t.Errorf("body-only packages for %q = %v, want %v", tt.changed, got, tt.wantBody)
			}
		})
	}
}

// TestSelectDeclaredReaders は ci-source-deps の判定を一時モジュールで確かめる。入力は、他パッケージの
// _test.go と、タグが付いたファイルだけのディレクトリ（タグを並べないと go list に出ない）にする。
func TestSelectDeclaredReaders(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GOWORK", "off")
	files := map[string]string{
		"go.mod":                        "module example.com/readers\n\ngo 1.24\n",
		"scripts/reader/reader.go":      "package reader\n",
		"scripts/reader/reader_test.go": "// ci-source-deps: -tags=s3conformance ./...\n\npackage reader\n",
		"internal/other/other.go":       "package other\n",
		"internal/other/other_test.go":  "package other\n",
		"internal/tagged/tagged.go":     "//go:build s3conformance\n\npackage tagged\n",
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const (
		reader = "example.com/readers/scripts/reader"
		other  = "example.com/readers/internal/other"
	)
	tests := []struct {
		name    string
		changed string
		want    []string
	}{
		{name: "another package's test file selects the reader", changed: "internal/other/other_test.go", want: []string{other, reader}},
		{name: "a directory that builds only with the declared tag selects the reader", changed: "internal/tagged/tagged.go", want: []string{reader}},
		{name: "unrelated markdown selects nothing", changed: "docs/runbook/testing.md", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectPackages(root, "", "", false, []string{"./..."}, []string{tt.changed})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("packages for %q = %v, want %v", tt.changed, got, tt.want)
			}
		})
	}
}

// TestSelectRealRepoDeclarations は、パッケージの外のファイルやソースを読むテストの宣言が、読む範囲の変更で
// そのテストのパッケージを選ぶことを、実リポジトリの宣言で検査する。宣言を外すか旧形に戻すと落ちる。
// 対象は表が主張する 3 パッケージに絞る。go list が実リポジトリを読むので、モジュールルートは絶対パスで渡す。
func TestSelectRealRepoDeclarations(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		changed string
		want    []string
	}{
		{name: "k8s base manifest", changed: "deploy/k8s/base/api.yaml", want: []string{"github.com/fetburner/rokuban/deploy/k8s"}},
		{name: "k8s e2e overlay config", changed: "deploy/k8s/overlays/e2e/config.yml", want: []string{"github.com/fetburner/rokuban/deploy/k8s"}},
		{name: "k8s e2e lib script", changed: "deploy/k8s/e2e/lib/env.sh", want: []string{"github.com/fetburner/rokuban/deploy/k8s"}},
		{name: "s3conformance test file in another package", changed: "internal/encode/s3_conformance_test.go", want: []string{"github.com/fetburner/rokuban/scripts/citest", "github.com/fetburner/rokuban/scripts/s3conformance"}},
		{name: "citest reads every test file", changed: "internal/api/spec_bounds_test.go", want: []string{"github.com/fetburner/rokuban/scripts/citest", "github.com/fetburner/rokuban/scripts/s3conformance"}},
		{name: "ruleguard-only directory", changed: "tools/lintrules/rules.go", want: []string{"github.com/fetburner/rokuban/scripts/citest"}},
		{name: "docs runbook selects nothing", changed: "docs/runbook/k8s.md", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectPackages(root, "", "", false, []string{"./deploy/k8s", "./scripts/citest", "./scripts/s3conformance"}, []string{tt.changed})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("packages for %q = %v, want %v", tt.changed, got, tt.want)
			}
		})
	}
}

func TestGoSubprocessDeclarations(t *testing.T) {
	missing, err := missingGoDeclarations(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range missing {
		t.Errorf("%s: go サブプロセスの依存を // ci-extra-deps: でファイル先頭に宣言してください", path)
	}
}

func TestFindsDeclarationMissingFromTaggedSource(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "harness.go")
	source := `//go:build conformance

package harness

import (
	"context"
	"os/exec"
)

func build(ctx context.Context) error {
	return exec.CommandContext(ctx, "go", "build", "./cmd/fixturetuner").Run()
}
`
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	missing, err := missingGoDeclarations(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, []string{file}) {
		t.Errorf("missing declarations = %v, want [%s]", missing, file)
	}
}

func TestReadExtraFilesDeclaration(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    []string
		wantErr bool
	}{
		{name: "cleans ./ prefix and trailing slash", line: "./openapi.yaml  testdata/", want: []string{"openapi.yaml", "testdata"}},
		{name: "module root is rejected", line: ".", wantErr: true},
		{name: "module root with ./ is rejected", line: "./", wantErr: true},
		{name: "empty list", line: "   ", wantErr: true},
		{name: "absolute path", line: "/etc/passwd", wantErr: true},
		{name: "parent escape", line: "../outside.json", wantErr: true},
		{name: "parent in the middle", line: "a/../../b", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "decl_test.go")
			source := "// ci-extra-files: " + tt.line + "\n\npackage decl\n"
			if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			_, decls, err := readDeclarations(file)
			if tt.wantErr {
				if err == nil {
					t.Errorf("readDeclarations(%q) error = nil, want error (files %v)", tt.line, decls.files)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(decls.files, tt.want) {
				t.Errorf("files for %q = %v, want %v", tt.line, decls.files, tt.want)
			}
		})
	}
}

func missingGoDeclarations(root string) ([]string, error) {
	var missing []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		file, decls, err := readDeclarations(path)
		if err != nil {
			return err
		}
		if !decls.declaresDeps() && callsGo(file) {
			missing = append(missing, path)
		}
		return nil
	})
	return missing, err
}

// runIn は dir で外部コマンドを実行し、失敗したら出力を添えて止める。
func runIn(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// writeProxyModule は file:// のモジュールプロキシに 1 版を置く。src は pkg.go の中身。
func writeProxyModule(t *testing.T, proxy, module, version, goMod, src string) {
	t.Helper()
	dir := filepath.Join(proxy, filepath.FromSlash(module), "@v")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	list, err := os.OpenFile(filepath.Join(dir, "list"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(list, version); err != nil {
		t.Fatal(err)
	}
	if err := list.Close(); err != nil {
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	for name, body := range map[string]string{"go.mod": goMod, "pkg.go": src} {
		w, err := zw.Create(module + "@" + version + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		version + ".info": []byte(`{"Version":"` + version + `","Time":"2020-01-01T00:00:00Z"}`),
		version + ".mod":  []byte(goMod),
		version + ".zip":  zipped.Bytes(),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeModuleFixture は版の上げ下げを実物の go list に通す最小モジュールを作り、base を git の commit として返す。
// モジュールは file:// のプロキシに置いて外へ出ない。各判定は git checkout で base に戻してから変更を入れる。
func writeModuleFixture(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOSUMDB", "off")
	proxy := t.TempDir()
	t.Setenv("GOPROXY", "file://"+proxy)
	const goMod = "module %s\n\ngo 1.24\n"
	writeProxyModule(t, proxy, "example.com/mfix/dep", "v1.0.0", fmt.Sprintf(goMod, "example.com/mfix/dep"),
		"package dep\n\nfunc Name() string { return \"dep\" }\n")
	writeProxyModule(t, proxy, "example.com/mfix/dep", "v1.0.1", fmt.Sprintf(goMod, "example.com/mfix/dep"),
		"package dep\n\nfunc Name() string { return \"dep\" }\n")
	midMod := fmt.Sprintf(goMod, "example.com/mfix/mid") + "\nrequire example.com/mfix/dep v1.0.0\n"
	midSrc := "package mid\n\nimport \"example.com/mfix/dep\"\n\nfunc Name() string { return dep.Name() }\n"
	writeProxyModule(t, proxy, "example.com/mfix/mid", "v1.0.0", midMod, midSrc)
	writeProxyModule(t, proxy, "example.com/mfix/mid", "v1.0.1", midMod, midSrc)

	root := t.TempDir()
	files := map[string]string{
		"go.mod":                 "module example.com/mfix\n\ngo 1.24\n\nrequire example.com/mfix/mid v1.0.0\n",
		"viamid/viamid.go":       "package viamid\n\nimport \"example.com/mfix/mid\"\n\nvar Name = mid.Name()\n",
		"unrelated/unrelated.go": "package unrelated\n",
		"cmd/tool/main.go":       "package main\n\nfunc main() {}\n",
		// viamid の依存を ci-extra-deps で宣言するだけで、モジュールは import しない。版上げはこの宣言経由でも選ぶ。
		"declared/declared.go": "// ci-extra-deps: ./viamid\n\npackage declared\n",
		// dep をテストファイルからだけ import する。通常の判定はテストの依存で選び、-body-only は選ばない。
		"testonly/testonly.go":      "package testonly\n",
		"testonly/testonly_test.go": "package testonly\n\nimport _ \"example.com/mfix/dep\"\n",
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runIn(t, root, "go", "mod", "tidy")
	runIn(t, root, "git", "init", "-q")
	runIn(t, root, "git", "add", "-A")
	runIn(t, root, "git", "-c", "user.name=citest", "-c", "user.email=citest@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "base")
	base := strings.TrimSpace(runIn(t, root, "git", "rev-parse", "HEAD"))
	return root, base
}

func TestSelectModuleChanges(t *testing.T) {
	root, base := writeModuleFixture(t)
	allModule := []string{"example.com/mfix/cmd/tool", "example.com/mfix/declared", "example.com/mfix/testonly", "example.com/mfix/unrelated", "example.com/mfix/viamid"}

	bumpDep := func(t *testing.T, root string) { runIn(t, root, "go", "get", "example.com/mfix/dep@v1.0.1") }
	tests := []struct {
		name     string
		bodyOnly bool
		edit     func(t *testing.T, root string)
		want     []string
	}{
		{
			name: "間接依存だけの版上げは、それを推移的に使うパッケージと、その依存を ci-extra-deps で宣言したパッケージを選ぶ",
			edit: bumpDep,
			want: []string{"example.com/mfix/declared", "example.com/mfix/testonly", "example.com/mfix/viamid"},
		},
		{
			name: "テストファイルからだけ使う版上げは、通常の判定では選ぶ",
			edit: bumpDep,
			want: []string{"example.com/mfix/declared", "example.com/mfix/testonly", "example.com/mfix/viamid"},
		},
		{
			name:     "テストファイルからだけ使う版上げは、-body-only では選ばない",
			bodyOnly: true,
			edit:     bumpDep,
			want:     []string{"example.com/mfix/declared", "example.com/mfix/viamid"},
		},
		{
			name: "直接依存の版上げは、それを使うパッケージを選ぶ",
			edit: func(t *testing.T, root string) { runIn(t, root, "go", "get", "example.com/mfix/mid@v1.0.1") },
			want: []string{"example.com/mfix/declared", "example.com/mfix/viamid"},
		},
		{
			name: "go 行の変更は全件",
			edit: func(t *testing.T, root string) {
				p := filepath.Join(root, "go.mod")
				body, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, bytes.Replace(body, []byte("go 1.24"), []byte("go 1.25"), 1), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: allModule,
		},
		{
			name: "godebug 行の変更は全件（既定の GODEBUG は全パッケージの実行時に効く）",
			edit: func(t *testing.T, root string) { runIn(t, root, "go", "mod", "edit", "-godebug=default=go1.21") },
			want: allModule,
		},
		{
			name: "tool 行だけの変更は選ばない",
			edit: func(t *testing.T, root string) {
				runIn(t, root, "go", "mod", "edit", "-tool=example.com/mfix/cmd/tool")
			},
			want: nil,
		},
		{
			name: "go.sum のハッシュ行の追加だけは選ばない",
			edit: func(t *testing.T, root string) {
				f, err := os.OpenFile(filepath.Join(root, "go.sum"), os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString("example.com/mfix/unused v1.0.0 h1:AAAA=\n"); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runIn(t, root, "git", "checkout", "-q", "--", ".")
			tt.edit(t, root)
			got, err := selectPackages(root, "", base, tt.bodyOnly, []string{"./..."}, []string{"go.mod", "go.sum"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("selected = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBrowserE2EPathFiltersMatchAllPackagesRule は ci.yml の plan ジョブの paths-filter にある
// .github/ 始まりのエントリが、requiresAllPackages が全件にする範囲（githubAllPackagesPaths）と完全一致することを検査する。
// 両方向のずれ（Go 側の追加・削除と、YAML 側の変更）を落とす。
func TestBrowserE2EPathFiltersMatchAllPackagesRule(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("ci.yml を YAML として読めない: %v", err)
	}
	var filters string
	for _, step := range workflow.Jobs["plan"].Steps {
		if strings.HasPrefix(step.Uses, "dorny/paths-filter@") {
			filters, _ = step.With["filters"].(string)
		}
	}
	if filters == "" {
		t.Fatal("plan ジョブに dorny/paths-filter の filters がありません")
	}
	// filters 文字列は、フィルタ名から glob の一覧への YAML である。
	var defined map[string][]string
	if err := yaml.Unmarshal([]byte(filters), &defined); err != nil {
		t.Fatalf("paths-filter の filters を YAML として読めない: %v", err)
	}
	var got []string
	for _, entry := range defined["web"] {
		if strings.HasPrefix(entry, ".github/") {
			got = append(got, entry)
		}
	}
	want := slices.Clone(githubAllPackagesPaths)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("paths-filter の .github/ 系 = %v, requiresAllPackages の範囲 = %v", got, want)
	}
}
