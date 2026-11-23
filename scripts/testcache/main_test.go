package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fetburner/rokuban/scripts/internal/cideps"
)

func TestNormalizeAssignsSameMtimeToSameContent(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("one.txt", "same bytes")
	write("nested/two.txt", "same bytes")
	if err := normalize(root, []string{"one.txt", "nested/two.txt"}, "sha1"); err != nil {
		t.Fatal(err)
	}
	first := statMTime(t, filepath.Join(root, "one.txt"))
	second := statMTime(t, filepath.Join(root, "nested/two.txt"))
	if !first.Equal(second) {
		t.Errorf("same content mtimes differ: %v != %v", first, second)
	}
	if !first.Before(time.Now().Add(-2 * time.Second)) {
		t.Errorf("normalized mtime is not safely in the past: %v", first)
	}
}

func TestNormalizeChangesMtimeForSameSizeContentChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "fixture.txt")
	if err := os.WriteFile(path, []byte("FAIL"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracked := []string{"fixture.txt"}
	if err := normalize(root, tracked, "sha1"); err != nil {
		t.Fatal(err)
	}
	before := statMTime(t, path)
	if err := os.WriteFile(path, []byte("PASS"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before, before); err != nil {
		t.Fatal(err)
	}
	if err := normalize(root, tracked, "sha1"); err != nil {
		t.Fatal(err)
	}
	after := statMTime(t, path)
	if before.Equal(after) {
		t.Fatalf("same-size content change kept mtime %v", after)
	}
}

func TestNormalizeAssignsSameMtimeToSameSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "two"} {
		if err := os.Symlink("shared-target", filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := normalize(root, []string{"one", "two"}, "sha1"); err != nil {
		t.Fatal(err)
	}
	first := lstatMTime(t, filepath.Join(root, "one"))
	second := lstatMTime(t, filepath.Join(root, "two"))
	if !first.Equal(second) {
		t.Fatalf("same symlink target mtimes differ: %v != %v", first, second)
	}
}

func TestNormalizeChangesMtimeWhenSymlinkTargetChanges(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "link")
	if err := os.Symlink("target-one", path); err != nil {
		t.Fatal(err)
	}
	tracked := []string{"link"}
	if err := normalize(root, tracked, "sha1"); err != nil {
		t.Fatal(err)
	}
	before := lstatMTime(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target-two", path); err != nil {
		t.Fatal(err)
	}
	if err := normalize(root, tracked, "sha1"); err != nil {
		t.Fatal(err)
	}
	after := lstatMTime(t, path)
	if before.Equal(after) {
		t.Fatalf("changing symlink target kept mtime %v", after)
	}
}

func TestNormalizeSetsSymlinkMtimeWithoutChangingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("target content"), 0o644); err != nil {
		t.Fatal(err)
	}
	targetTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(target, targetTime, targetTime); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}
	if err := normalize(root, []string{"link"}, "sha1"); err != nil {
		t.Fatal(err)
	}
	linkTime := lstatMTime(t, link)
	if !linkTime.Before(time.Now().Add(-2 * time.Second)) {
		t.Fatalf("normalized symlink mtime is not safely in the past: %v", linkTime)
	}
	if got := statMTime(t, target); !got.Equal(targetTime) {
		t.Fatalf("normalizing symlink changed target mtime: got %v, want %v", got, targetTime)
	}
}

func TestNormalizeChangesDirectoryMtimeWhenEntryIsAdded(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "inputs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := normalize(root, nil, "sha1"); err != nil {
		t.Fatal(err)
	}
	before := statMTime(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "new-entry"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := normalize(root, nil, "sha1"); err != nil {
		t.Fatal(err)
	}
	after := statMTime(t, dir)
	if before.Equal(after) {
		t.Fatalf("adding a directory entry kept mtime %v", after)
	}
}

func TestNormalizeKeepsDirectoryMtimeWhenEntriesAreUnchanged(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "inputs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := normalize(root, nil, "sha1"); err != nil {
		t.Fatal(err)
	}
	before := statMTime(t, dir)
	if err := os.Chtimes(dir, before.Add(24*60*60*1e9), before.Add(24*60*60*1e9)); err != nil {
		t.Fatal(err)
	}
	if err := normalize(root, nil, "sha1"); err != nil {
		t.Fatal(err)
	}
	after := statMTime(t, dir)
	if !before.Equal(after) {
		t.Fatalf("unchanged directory entries changed mtime: %v != %v", before, after)
	}
}

func TestNormalizeIncludesGitDirectoryEntry(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := normalize(root, nil, "sha1"); err != nil {
		t.Fatal(err)
	}
	before := rootChildStats(t, root)
	changed := statMTime(t, gitDir).Add(24 * time.Hour)
	if err := os.Chtimes(gitDir, changed, changed); err != nil {
		t.Fatal(err)
	}
	if err := normalize(root, nil, "sha1"); err != nil {
		t.Fatal(err)
	}
	if after := rootChildStats(t, root); !slices.Equal(before, after) {
		t.Fatalf("root child stats differ after normalization:\nbefore: %v\nafter:  %v", before, after)
	}
}

func TestNormalizeWorkspaceMTimesUsesGitTrackedFiles(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	runGit("init", "-q")
	path := filepath.Join(root, "tracked.txt")
	if err := os.WriteFile(path, []byte("tracked content"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.txt")
	if err := normalizeWorkspaceMTimes(root); err != nil {
		t.Fatal(err)
	}
	mtime := statMTime(t, path)
	if !mtime.Before(time.Now().Add(-2 * time.Second)) {
		t.Fatalf("normalized tracked file is not in the past: %v", mtime)
	}
}

func TestDeclaredDependenciesReadsOnlyUntaggedLeadingComments(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "input.go")
	source := "package sample\n\nvar example = `// ci-extra-deps: none`\n"
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	_, deps, _, err := cideps.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Fatal("string literal was treated as a declaration")
	}
	source = "// ci-extra-deps: -tags=conformance ./cmd/example\n// ci-extra-deps: ./internal/example\n\npackage sample\n"
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	_, deps, _, err = cideps.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	var patterns []string
	for _, dep := range deps {
		if dep.Tags() == "" {
			patterns = append(patterns, dep.Pattern())
		}
	}
	if !slices.Equal(patterns, []string{"./internal/example"}) {
		t.Fatalf("untagged dependencies = %v, want [./internal/example]", patterns)
	}
}

type rootChildStat struct {
	name  string
	size  int64
	mode  os.FileMode
	mtime time.Time
}

func rootChildStats(t *testing.T, root string) []rootChildStat {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	stats := make([]rootChildStat, 0, len(entries))
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		stats = append(stats, rootChildStat{name: entry.Name(), size: info.Size(), mode: info.Mode(), mtime: info.ModTime()})
	}
	return stats
}

func statMTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func lstatMTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}
