package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeSelectionFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	// 実物の go list に渡せる最小モジュール。rokuban 本体のビルドや web/dist は不要。
	root := t.TempDir()
	t.Setenv("GOWORK", "off")
	files := map[string]string{
		"go.mod":                         "module example.com/fixture\n\ngo 1.24\n",
		"internal/mirakc/client.go":      "package mirakc\n",
		"internal/worker/worker.go":      "package worker\n",
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
			name:    "GitHub workflow",
			changed: ".github/workflows/ci.yml",
			want:    allConformance,
		},
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
			got, err := selectPackages(root, "conformance", []string{"./conformance/..."}, []string{tt.changed})
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

func TestMatchesFileDeclaration(t *testing.T) {
	tests := []struct {
		name    string
		changed []string
		decl    string
		want    bool
	}{
		{name: "module root matches any change", changed: []string{"docs/overview.md"}, decl: ".", want: true},
		{name: "exact file", changed: []string{"openapi.yaml"}, decl: "openapi.yaml", want: true},
		{name: "exact file with other name", changed: []string{"openapi.yml"}, decl: "openapi.yaml", want: false},
		{name: "directory matches beneath", changed: []string{"internal/jobs/a.go"}, decl: "internal/jobs", want: true},
		{name: "directory does not match its own name", changed: []string{"internal/jobs"}, decl: "internal/jobs/x", want: false},
		{name: "directory boundary", changed: []string{"internal/jobsx/a.go"}, decl: "internal/jobs", want: false},
		{name: "no changes", changed: nil, decl: ".", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := packageSelection{paths: tt.changed}
			if got := s.matchesFile(tt.decl); got != tt.want {
				t.Errorf("matchesFile(%q) with changes %v = %v, want %v", tt.decl, tt.changed, got, tt.want)
			}
		})
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
		{name: "module root", line: ".", want: []string{"."}},
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
