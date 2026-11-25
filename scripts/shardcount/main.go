// ci-extra-deps: none

// Command shardcount は CI で選ばれたパッケージの見積もり時間から、go test の shard 数を
// 標準出力に 1 つ書く。
//
// 標準入力は 1 行に 1 つの import path。見積もりは --timing-files の glob に一致する
// test2json の記録から取る。main の実行が保存した実測で、パッケージ単位の pass / fail / skip
// 事象の Elapsed を使う。同じパッケージが複数の記録にあれば最大を取る。
//
// 使い方: go run ./scripts/shardcount --timing-files 'timings/*.json' < packages.txt
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxShards は shard 数の上限。main の全件実行も同じ数で分ける。
const maxShards = 3

func main() {
	pattern := flag.String("timing-files", "", "test2json の記録に一致する glob（例: timings/*.json）")
	parallel := flag.Int("parallel", 0, "shard 内の実効並列度 k。go test -p の並行数ではなく、実測の合計 ÷ 実時間の比（test は 3、直列の conformance は 1）")
	setupSec := flag.Float64("setup-sec", 0, "shard 1 本の準備時間（秒）。テスト本体より前に毎回かかる分")
	flag.Parse()
	if *pattern == "" || *parallel < 1 || *setupSec <= 0 {
		fmt.Fprintln(os.Stderr, "shardcount: --timing-files、--parallel（1 以上）、--setup-sec（正の数）を指定してください")
		os.Exit(2)
	}
	if err := run(os.Stdin, os.Stdout, *pattern, *parallel, *setupSec); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// shardCount は見積もり時間 totalSec（秒）から shard 数を返す。
//
// shard 内は parallel で並行に回るので、n 本に割ったときの 1 本の実時間は totalSec/(parallel·n)
// とみなす（計測: run 45591817397 の test shard は、Elapsed の合計 ÷ step の実時間が 2.9〜3.2。
// step 時間にはビルドも入る）。n-1 本から n 本にすると実時間は totalSec/(parallel·(n-1)) から
// totalSec/(parallel·n) へ縮み、その差は totalSec/(parallel·n(n-1))。
// 縮みが準備 setupSec 秒以上のときだけ n 本目を足す。つまり totalSec >= setupSec·parallel·n(n-1)。
// 結果の境界は parallel=3・setupSec=50 なら 2 本が 300、3 本が 900。
//
// 最長パッケージが実時間の下限になるが、モデルには入れない。その分は本数が多い側（従来寄り）に外れる。
//
// unmeasured が真（実測の無いパッケージが 1 つでもある）なら見積もれないので、上限を返す。
// 新しいパッケージを含む PR は、これまでの固定の 3 本と同じ振り分けになる。
// 見積もりの欠けを「shard が多すぎる」（ランナー時間が余る）側の失敗に寄せるためである。
// 「shard が少なすぎて待つ」にはならない。
func shardCount(totalSec float64, unmeasured bool, parallel int, setupSec float64) int {
	if unmeasured {
		return maxShards
	}
	n := 1
	for k := 2; k <= maxShards; k++ {
		if totalSec >= setupSec*float64(parallel*k*(k-1)) {
			n = k
		}
	}
	return n
}

// event は test2json の 1 行のうち、このプログラムが読むフィールドだけ。
// 形は go doc cmd/test2json の TestEvent が契約。
type event struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
}

func run(stdin io.Reader, stdout io.Writer, pattern string, parallel int, setupSec float64) error {
	pkgs, err := readPackages(stdin)
	if err != nil {
		return fmt.Errorf("標準入力のパッケージを読めません: %w", err)
	}
	est, err := estimates(pattern)
	if err != nil {
		return err
	}
	var total float64
	unmeasured := false
	for _, pkg := range pkgs {
		sec, ok := est[pkg]
		if !ok {
			unmeasured = true
			continue
		}
		total += sec
	}
	_, err = fmt.Fprintln(stdout, shardCount(total, unmeasured, parallel, setupSec))
	return err
}

func readPackages(r io.Reader) ([]string, error) {
	var pkgs []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, sc.Err()
}

// estimates は記録からパッケージごとの見積もり秒を返す。実測のあるパッケージだけを含む。
// パッケージ単位の pass / fail / skip 事象（Test が空）があれば実測とみなす。
// no test files のパッケージは skip で Elapsed が 0 なので、0 秒の実測になる。
func estimates(pattern string) (map[string]float64, error) {
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("timing-files の glob が不正です: %w", err)
	}
	est := make(map[string]float64)
	for _, name := range files {
		if err := readRecord(name, est); err != nil {
			return nil, err
		}
	}
	return est, nil
}

func readRecord(name string, est map[string]float64) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	for {
		var ev event
		err := dec.Decode(&ev)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s を test2json として読めません: %w", name, err)
		}
		if ev.Test != "" {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
		default:
			continue
		}
		if prev, ok := est[ev.Package]; !ok || ev.Elapsed > prev {
			est[ev.Package] = ev.Elapsed
		}
	}
}
