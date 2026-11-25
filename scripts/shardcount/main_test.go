package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 期待値はすべてリテラルで書く。実装の定数（setupSec など）と比べるとテストは何も主張しない。
func TestShardCount(t *testing.T) {
	cases := []struct {
		name       string
		total      float64
		unmeasured bool
		want       int
	}{
		{"空に近い", 0, false, 1},
		{"100 秒未満は 1 本", 99, false, 1},
		{"100 秒で 2 本目", 100, false, 2},
		{"300 秒未満は 2 本", 299, false, 2},
		{"300 秒で 3 本目", 300, false, 3},
		{"上限は 3 本", 5000, false, 3},
		{"実測の無いパッケージは 1 秒でも上限", 10, true, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shardCount(tc.total, tc.unmeasured); got != tc.want {
				t.Fatalf("shardCount(%v, %v) = %d, want %d", tc.total, tc.unmeasured, got, tc.want)
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
		name  string
		input string
		want  string
	}{
		{"合計 110 秒は 2 本", "ex/a\nex/b\n", "2\n"},
		{"テスト無しは 0 秒として実測済み", "ex/none\n", "1\n"},
		{"実測の無いパッケージが混じると 3 本", "ex/a\nex/new\n", "3\n"},
		{"空行は無視する", "\nex/a\n\n", "1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(strings.NewReader(tc.input), &out, filepath.Join(dir, "timing-*.json")); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("got %q, want %q", out.String(), tc.want)
			}
		})
	}
}
