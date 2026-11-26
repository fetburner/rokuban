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
// 見積もりは a が 60 秒（テストの 30 秒ではない）、b が 50 秒、none が 0 秒。
func writeRecords(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("timing-0.json", strings.Join([]string{
		`{"Action":"run","Package":"example.com/m/a","Test":"TestX"}`,
		`{"Action":"pass","Package":"example.com/m/a","Test":"TestX","Elapsed":30}`,
		`{"Action":"pass","Package":"example.com/m/a","Elapsed":60}`,
		`{"Action":"skip","Package":"example.com/m/none","Elapsed":0}`,
		`{"Action":"output","Package":"example.com/m/b","Output":"ok"}`,
	}, "\n"))
	write("timing-1.json", `{"Action":"fail","Package":"example.com/m/b","Elapsed":50}`+"\n")
	return filepath.Join(dir, "timing-*.json")
}

// PR の本数の決め方（--parallel と --setup-sec）。合計 110 秒は、parallel=3・準備 50 秒なら 1 本、
// parallel=1・準備 40 秒なら 2 本になる。
func TestRunEstimatesShardCount(t *testing.T) {
	pattern := writeRecords(t)
	cases := []struct {
		name  string
		input string
		plan  plan
		want  string
	}{
		{
			"test: 合計 110 秒は 1 本",
			"example.com/m/a\nexample.com/m/b\n",
			plan{parallel: 3, setupSec: 50},
			`{"include":[{"id":0,"estimatedRuntime":"1m50s","packages":"example.com/m/a example.com/m/b","description":"0 - a and 1 others"}]}` + "\n",
		},
		{
			"conformance: 合計 110 秒は 2 本",
			"example.com/m/a\nexample.com/m/b\n",
			plan{parallel: 1, setupSec: 40},
			`{"include":[{"id":0,"estimatedRuntime":"1m0s","packages":"example.com/m/a","description":"0 - a"},{"id":1,"estimatedRuntime":"50s","packages":"example.com/m/b","description":"1 - b"}]}` + "\n",
		},
		{
			"実測の無いパッケージが混じると上限の 3 本（パッケージは 2 本なので 2 本）",
			"example.com/m/a\nexample.com/m/new\n",
			plan{parallel: 3, setupSec: 50},
			`{"include":[{"id":0,"estimatedRuntime":"1m0s","packages":"example.com/m/a","description":"0 - a"},{"id":1,"estimatedRuntime":"0s","packages":"example.com/m/new","description":"1 - new"}]}` + "\n",
		},
		{
			"空行は無視する",
			"\nexample.com/m/a\n\n",
			plan{parallel: 1, setupSec: 40},
			`{"include":[{"id":0,"estimatedRuntime":"1m0s","packages":"example.com/m/a","description":"0 - a"}]}` + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(strings.NewReader(tc.input), &out, "example.com/m", pattern, tc.plan); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("got\n%s\nwant\n%s", out.String(), tc.want)
			}
		})
	}
}

// main の全件実行（--shards の固定）。空の shard は作らず、パッケージ数で頭打ちにする。
func TestRunFixedShards(t *testing.T) {
	pattern := writeRecords(t)
	cases := []struct {
		name  string
		input string
		shard int
		want  string
	}{
		{
			"3 本に固定。最長から貪欲に詰め、テスト無し（0 秒）は最も軽い shard へ",
			"example.com/m/a\nexample.com/m/b\nexample.com/m/none\n",
			3,
			`{"include":[{"id":0,"estimatedRuntime":"1m0s","packages":"example.com/m/a","description":"0 - a"},{"id":1,"estimatedRuntime":"50s","packages":"example.com/m/b","description":"1 - b"},{"id":2,"estimatedRuntime":"0s","packages":"example.com/m/none","description":"2 - none"}]}` + "\n",
		},
		{
			"パッケージが本数より少なければ空の shard を作らない",
			"example.com/m/a\nexample.com/m/b\n",
			3,
			`{"include":[{"id":0,"estimatedRuntime":"1m0s","packages":"example.com/m/a","description":"0 - a"},{"id":1,"estimatedRuntime":"50s","packages":"example.com/m/b","description":"1 - b"}]}` + "\n",
		},
		{
			"1 本でも同じ形で出す",
			"example.com/m/a\nexample.com/m/b\n",
			1,
			`{"include":[{"id":0,"estimatedRuntime":"1m50s","packages":"example.com/m/a example.com/m/b","description":"0 - a and 1 others"}]}` + "\n",
		},
		{
			"貪欲法: 見積もりの大きい順に、合計の最も小さい shard へ入れる。記録に無い c と d は 0 秒（a:60, b:50 を 2 本に）",
			"example.com/m/a\nexample.com/m/b\nexample.com/m/c\nexample.com/m/d\n",
			2,
			`{"include":[{"id":0,"estimatedRuntime":"1m0s","packages":"example.com/m/a","description":"0 - a"},{"id":1,"estimatedRuntime":"50s","packages":"example.com/m/b example.com/m/c example.com/m/d","description":"1 - b and 2 others"}]}` + "\n",
		},
		{
			"パッケージ 0 件は空の include",
			"",
			3,
			`{"include":[]}` + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(strings.NewReader(tc.input), &out, "example.com/m", pattern, plan{shards: tc.shard}); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("got\n%s\nwant\n%s", out.String(), tc.want)
			}
		})
	}
}

// モジュールそのもののパッケージは "." と書く。
func TestRelativePackagePath(t *testing.T) {
	cases := map[string]string{
		"example.com/m":     ".",
		"example.com/m/a/b": "a/b",
	}
	for pkg, want := range cases {
		if got := relativePackagePath(pkg, "example.com/m"); got != want {
			t.Errorf("relativePackagePath(%q) = %q, want %q", pkg, got, want)
		}
	}
}
