// ci-extra-deps: none

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

// moduleChanges は base と head の go.mod・go.sum を比べ、依存グラフの判定で絞れる
// 変更のモジュールパスを返す。対象は版が変わったモジュールと、go.sum の既存の行が
// 消えた・書き換わったモジュール。go.sum の行の追加だけは何もビルドを変えないので対象にしない。
// require と tool 以外の go.mod の行（go・toolchain・godebug・exclude・replace など）の変更は、
// 依存グラフの外の判定に効くかもしれないので full を返す。tool 行の変更は Go のテスト選定には効かない
// （生成物の検査と lint は CI が別に全件で回す）ので、何も返さない。
func moduleChanges(root, base string, goMod, goSum bool) (modules map[string]bool, full bool, err error) {
	modules = make(map[string]bool)
	baseSum, err := gitShow(root, base, "go.sum")
	if err != nil {
		return nil, false, err
	}
	headSum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		return nil, false, fmt.Errorf("read go.sum: %w", err)
	}
	if goSum {
		for path := range removedSumModules(baseSum, headSum) {
			modules[path] = true
		}
	}
	if !goMod {
		return modules, false, nil
	}

	baseMod, err := gitShow(root, base, "go.mod")
	if err != nil {
		return nil, false, err
	}
	// base の解決には base のツリーが要る。go.mod と go.sum だけを一時ディレクトリに置けば、
	// go list -m は本体のソースを読まずに解決できる。
	baseDir, err := os.MkdirTemp("", "citest-base-")
	if err != nil {
		return nil, false, fmt.Errorf("create base module dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(baseDir) }()
	if err := os.WriteFile(filepath.Join(baseDir, "go.mod"), baseMod, 0o600); err != nil {
		return nil, false, fmt.Errorf("write base go.mod: %w", err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "go.sum"), baseSum, 0o600); err != nil {
		return nil, false, fmt.Errorf("write base go.sum: %w", err)
	}

	baseRest, err := goModRest(baseDir)
	if err != nil {
		return nil, false, err
	}
	headRest, err := goModRest(root)
	if err != nil {
		return nil, false, err
	}
	if !reflect.DeepEqual(baseRest, headRest) {
		return nil, true, nil
	}

	baseVersions, err := moduleVersions(baseDir)
	if err != nil {
		return nil, false, err
	}
	headVersions, err := moduleVersions(root)
	if err != nil {
		return nil, false, err
	}
	for path, version := range baseVersions {
		if headVersions[path] != version {
			modules[path] = true
		}
	}
	for path, version := range headVersions {
		if baseVersions[path] != version {
			modules[path] = true
		}
	}
	return modules, false, nil
}

// removedSumModules は base にあって head に無い go.sum の行のモジュールパスを返す。
func removedSumModules(base, head []byte) map[string]bool {
	present := make(map[string]bool)
	for _, line := range sumLines(head) {
		present[line] = true
	}
	removed := make(map[string]bool)
	for _, line := range sumLines(base) {
		if !present[line] {
			removed[strings.Fields(line)[0]] = true
		}
	}
	return removed
}

// sumLines は go.sum の空でない行を、空白を詰めて返す。
func sumLines(data []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			lines = append(lines, strings.Join(fields, " "))
		}
	}
	return lines
}

// goModRest は go.mod の内容から、絞り込みに使ってよい行を除いた残りを返す。
// 許可リストに反転してあり、新しいディレクティブは既定で比べ対象になる（全件に倒れる）。
// 除くのは Module（パスだけの比較は版の判定と重なる）、Require（版は moduleVersions が比べる）、
// Tool（Go のテスト選定には効かない）の 3 つ。go・toolchain・godebug・exclude・replace・retract・
// 今後増えるディレクティブは、差があれば全件になる。
func goModRest(dir string) (map[string]any, error) {
	out, err := goOutput(dir, "mod", "edit", "-json")
	if err != nil {
		return nil, err
	}
	var rest map[string]any
	if err := json.Unmarshal(out, &rest); err != nil {
		return nil, fmt.Errorf("decode go mod edit -json: %w", err)
	}
	for _, key := range []string{"Module", "Require", "Tool"} {
		delete(rest, key)
	}
	return rest, nil
}

// moduleVersions は dir の解決済みモジュールを、パスから「版と置換先」への対応で返す。
// 主モジュールは含めない。
func moduleVersions(dir string) (map[string]string, error) {
	out, err := goOutput(dir, "list", "-m", "-json", "all")
	if err != nil {
		return nil, err
	}
	type listedModule struct {
		Path    string
		Version string
		Main    bool
		Replace *listedModule
	}
	versions := make(map[string]string)
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var m listedModule
		if err := decoder.Decode(&m); errors.Is(err, io.EOF) {
			return versions, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode go list -m: %w", err)
		}
		if m.Main {
			continue
		}
		key := m.Version
		if m.Replace != nil {
			key += " => " + m.Replace.Path + " " + m.Replace.Version
		}
		versions[m.Path] = key
	}
}

// goOutput は dir で go の子コマンドを実行し、標準出力を返す。
func goOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s in %s: %w\n%s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return out, nil
}

// gitShow は rev の name の内容を返す。
func gitShow(root, rev, name string) ([]byte, error) {
	cmd := exec.Command("git", "show", rev+":"+name)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w\n%s", rev, name, err, stderr.String())
	}
	return out, nil
}
