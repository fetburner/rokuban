package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 期待値はすべてリテラルで書く。実装の定数や式と比べるとテストは何も主張しない。
// test ジョブは parallel=3・準備 50 秒（境界 300 / 900）、
// mirakc-conformance ジョブは parallel=1・準備 40 秒（境界 80 / 240）。
func TestShardCount(t *testing.T) {
	cases := []struct {
		name       string
		total      float64
		unmeasured bool
		parallel   int
		setup      float64
		want       int
	}{
		{"test: 空に近い", 0, false, 3, 50, 1},
		{"test: 299 秒は 1 本", 299, false, 3, 50, 1},
		{"test: 300 秒で 2 本", 300, false, 3, 50, 2},
		{"test: 899 秒は 2 本", 899, false, 3, 50, 2},
		{"test: 900 秒で 3 本", 900, false, 3, 50, 3},
		{"test: 上限は 3 本", 5000, false, 3, 50, 3},
		{"test: 実測の無いパッケージは 1 秒でも上限", 10, true, 3, 50, 3},
		{"conformance: 79 秒は 1 本", 79, false, 1, 40, 1},
		{"conformance: 80 秒で 2 本", 80, false, 1, 40, 2},
		{"conformance: 239 秒は 2 本", 239, false, 1, 40, 2},
		{"conformance: 240 秒で 3 本", 240, false, 1, 40, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shardCount(tc.total, tc.unmeasured, tc.parallel, tc.setup); got != tc.want {
				t.Fatalf("shardCount(%v, %v, %d, %v) = %d, want %d", tc.total, tc.unmeasured, tc.parallel, tc.setup, got, tc.want)
			}
		})
	}
}

// 記録は test2json の形（go doc cmd/test2json）。パッケージ単位の事象は Test が空。
func TestRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("timing-0.json", strings.Join([]string{
		`{"Action":"run","Package":"ex/a","Test":"TestX"}`,
		`{"Action":"pass","Package":"ex/a","Test":"TestX","Elapsed":30}`,
		`{"Action":"pass","Package":"ex/a","Elapsed":60}`,
		`{"Action":"skip","Package":"ex/none","Elapsed":0}`,
		`{"Action":"output","Package":"ex/b","Output":"ok"}`,
	}, "\n"))
	write("timing-1.json", `{"Action":"fail","Package":"ex/b","Elapsed":50}`+"\n")

	cases := []struct {
		name     string
		input    string
		parallel int
		setup    float64
		want     string
	}{
		{"test: 合計 110 秒は 1 本（300 秒未満）", "ex/a\nex/b\n", 3, 50, "1\n"},
		{"conformance: 合計 110 秒は 2 本（80 秒以上）", "ex/a\nex/b\n", 1, 40, "2\n"},
		{"テスト無しは 0 秒として実測済み", "ex/none\n", 3, 50, "1\n"},
		{"実測の無いパッケージが混じると 3 本", "ex/a\nex/new\n", 3, 50, "3\n"},
		{"空行は無視する", "\nex/a\n\n", 1, 40, "1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(strings.NewReader(tc.input), &out, filepath.Join(dir, "timing-*.json"), tc.parallel, tc.setup); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("got %q, want %q", out.String(), tc.want)
			}
		})
	}
}
