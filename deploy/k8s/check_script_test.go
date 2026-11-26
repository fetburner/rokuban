package k8s

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// listOverlays は deploy/k8s/check.sh の --list-overlays の出力を返す。K8S_DIR が空なら、
// スクリプトの置かれたディレクトリ（この package のディレクトリ）を走査する。
func listOverlays(t *testing.T, k8sDir string) []string {
	t.Helper()
	cmd := exec.Command("bash", "check.sh", "--list-overlays")
	if k8sDir != "" {
		cmd.Env = append(os.Environ(), "K8S_DIR="+k8sDir)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("check.sh --list-overlays: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	// 並びは glob の照合順（ロケール依存）なので、比べる前に揃える。
	sort.Strings(got)
	return got
}

// TestCheckScriptFindsEveryOverlay は、check.sh が overlay を名前の列挙ではなく overlays/*/ の走査で拾うことを
// 確かめる。偽の overlay を一時ディレクトリに並べ、名前によらず全部が一覧に出ることを見る。
// 名前を直書きした一覧に戻すと、ここで落ちる。
func TestCheckScriptFindsEveryOverlay(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "kind", "zeta-2"} {
		if err := os.MkdirAll(filepath.Join(root, "overlays", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 通常のファイルは overlay ではない。
	if err := os.WriteFile(filepath.Join(root, "overlays", "NOTES.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := listOverlays(t, root)
	want := []string{"overlays/alpha", "overlays/kind", "overlays/zeta-2"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("check.sh --list-overlays = %q, want %q", got, want)
	}
}

// TestCheckScriptListsTheRepositoryOverlays は、既定の走査先（このディレクトリ）の overlay が、
// ディレクトリの実際の並びと一致することを確かめる。
func TestCheckScriptListsTheRepositoryOverlays(t *testing.T) {
	entries, err := os.ReadDir("overlays")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range entries {
		if e.IsDir() {
			want = append(want, "overlays/"+e.Name())
		}
	}
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatal("overlays/ に overlay が無い（検査が空振りする）")
	}
	got := listOverlays(t, "")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("check.sh --list-overlays = %q, want %q", got, want)
	}
}
