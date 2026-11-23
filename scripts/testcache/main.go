// ci-extra-deps: none

// Command testcache は、CI の Go テストキャッシュを準備する。
package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fetburner/rokuban/scripts/internal/cideps"
)

func main() {
	var err error
	switch {
	case len(os.Args) == 1 || os.Args[1] == "normalize":
		err = normalizeWorkspaceMTimes(".")
	case len(os.Args) == 2 && os.Args[1] == "declared-packages":
		err = printDeclaredPackages(os.Stdout)
	default:
		err = errors.New("usage: go run ./scripts/testcache [normalize|declared-packages]")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "testcache:", err)
		os.Exit(1)
	}
}

// normalizeWorkspaceMTimes は、追跡済みファイルとシンボリックリンクの mtime を blob のハッシュから、
// 作業ツリーのディレクトリの mtime を直下のエントリ名から決める。.git の中には降りず、
// .git ディレクトリ自身も同じ規則で正規化する。正しさは go help test の契約だけに依る:
// "Tests that open files within the package's module or that consult environment variables only match future runs in which the files and environment variables are unchanged."
// 内容が変われば mtime も変わるので、Go が stat を見る実装でも内容を見る実装でも、変更したファイルを読むテストは再実行される。
// 内容が同じなら stat も同じになり、checkout をやり直しても結果が当たる。
// 過去の時刻にするのは、最近変わったファイルを読んだ結果をキャッシュしない Go の実装詳細に合わせるためで、
// 当たり率にだけ効き、正しさの根拠ではない。
func normalizeWorkspaceMTimes(root string) error {
	formatCmd := exec.Command("git", "rev-parse", "--show-object-format")
	formatCmd.Dir = root
	format, err := formatCmd.Output()
	if err != nil {
		return fmt.Errorf("read Git object format: %w", err)
	}
	objectFormat := strings.TrimSpace(string(format))
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return fmt.Errorf("unsupported Git object format %q", objectFormat)
	}

	listCmd := exec.Command("git", "ls-files", "-z")
	listCmd.Dir = root
	tracked, err := listCmd.Output()
	if err != nil {
		return fmt.Errorf("list tracked files: %w", err)
	}
	files := bytes.Split(tracked, []byte{0})
	paths := make([]string, 0, len(files))
	for _, name := range files {
		if len(name) != 0 {
			paths = append(paths, string(name))
		}
	}
	return normalize(root, paths, objectFormat)
}

func normalize(root string, tracked []string, objectFormat string) error {
	for _, name := range tracked {
		path := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat tracked file %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			mtime, err := symlinkMTime(path, objectFormat)
			if err != nil {
				return fmt.Errorf("hash tracked symlink %q: %w", name, err)
			}
			ts := []unix.Timespec{unix.NsecToTimespec(mtime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
			if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return fmt.Errorf("set mtime for symlink %q: %w", name, err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		mtime, err := contentMTime(path, objectFormat)
		if err != nil {
			return fmt.Errorf("hash tracked file %q: %w", name, err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			return fmt.Errorf("set mtime for %q: %w", name, err)
		}
	}

	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		if path != root && entry.Name() == ".git" {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk working-tree directories: %w", err)
	}
	slices.SortFunc(dirs, func(a, b string) int {
		return strings.Compare(b, a) // 子から親へ処理する。
	})
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read directory %q: %w", dir, err)
		}
		mtime := directoryMTime(entries)
		if err := os.Chtimes(dir, mtime, mtime); err != nil {
			return fmt.Errorf("set directory mtime for %q: %w", dir, err)
		}
	}
	return nil
}

func symlinkMTime(path, objectFormat string) (time.Time, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return time.Time{}, err
	}
	return blobMTime([]byte(target), objectFormat), nil
}

func contentMTime(path, objectFormat string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, err
	}
	var h hash.Hash
	if objectFormat == "sha256" {
		h = sha256.New()
	} else {
		h = sha1.New()
	}
	if _, err := fmt.Fprintf(h, "blob %d\x00", info.Size()); err != nil {
		return time.Time{}, err
	}
	if _, err := io.Copy(h, f); err != nil {
		return time.Time{}, err
	}
	return hashMTime(h.Sum(nil)), nil
}

func blobMTime(content []byte, objectFormat string) time.Time {
	var h hash.Hash
	if objectFormat == "sha256" {
		h = sha256.New()
	} else {
		h = sha1.New()
	}
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(content))
	_, _ = h.Write(content)
	return hashMTime(h.Sum(nil))
}

func directoryMTime(entries []os.DirEntry) time.Time {
	h := sha256.New()
	for _, entry := range entries { // os.ReadDir は名前順に返す。
		name := []byte(entry.Name())
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(name)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(name)
	}
	return hashMTime(h.Sum(nil))
}

func hashMTime(digest []byte) time.Time {
	// 25 年分のナノ秒は uint64 に収まり、CI の checkout 時刻より過去になる。
	const pastRange = uint64(25 * 365 * 24 * time.Hour)
	nanos := binary.BigEndian.Uint64(digest[:8]) % pastRange
	return time.Unix(1+int64(nanos/uint64(time.Second)), int64(nanos%uint64(time.Second)))
}

type listedPackage struct {
	Dir          string
	ImportPath   string
	ForTest      string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string
}

// printDeclaredPackages はテストバイナリの外に依存を追加する宣言を持つ、
// 有効なタグなしパッケージを列挙する。go list が test-shard の既定タグを適用し、
// 追加タグ付きの宣言はここでは無視する。
func printDeclaredPackages(out io.Writer) error {
	cmd := exec.Command("go", "list", "-test", "-json", "./...")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open go list output: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start go list: %w", err)
	}
	var packages []string
	decoder := json.NewDecoder(stdout)
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			_ = cmd.Wait()
			return fmt.Errorf("decode go list package: %w", err)
		}
		if pkg.ForTest != "" || strings.HasSuffix(pkg.ImportPath, ".test") {
			continue
		}
		files := append(append(append(append([]string{}, pkg.GoFiles...), pkg.CgoFiles...), pkg.TestGoFiles...), pkg.XTestGoFiles...)
		selected := false
		for _, file := range files {
			_, deps, _, err := cideps.Read(filepath.Join(pkg.Dir, file))
			if err != nil {
				_ = cmd.Wait()
				return fmt.Errorf("read CI dependency declaration in %s: %w", file, err)
			}
			for _, dep := range deps {
				if dep.Tags() != "" {
					continue
				}
				depCmd := exec.Command("go", "list", "-f", "{{.ImportPath}}", dep.Pattern())
				depCmd.Dir = "."
				depOut, err := depCmd.Output()
				if err != nil {
					_ = cmd.Wait()
					return fmt.Errorf("resolve declared dependency %q: %w", dep.Pattern(), err)
				}
				for _, importPath := range strings.Fields(string(depOut)) {
					if importPath != pkg.ImportPath {
						selected = true
						break
					}
				}
				if selected {
					break
				}
			}
			if selected {
				break
			}
		}
		if selected {
			packages = append(packages, pkg.ImportPath)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	slices.Sort(packages)
	packages = slices.Compact(packages)
	for _, pkg := range packages {
		if _, err := fmt.Fprintln(out, pkg); err != nil {
			return fmt.Errorf("write package list: %w", err)
		}
	}
	return nil
}
