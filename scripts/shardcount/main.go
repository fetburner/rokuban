// ci-extra-deps: none

// Command shardcount は CI の go test の shard の計画を、GitHub Actions の matrix の JSON として
// 標準出力に 1 行書く。形は gotestsum tool ci-matrix と同じ（include の各要素に id・estimatedRuntime・
// packages・description）。ci.yml は gotestsum を使わず、この出力だけを読む。
//
// 標準入力は 1 行に 1 つの import path。見積もりは --timing-files の glob に一致する test2json の記録から取る。
// main の実行が保存した実測で、パッケージ単位の pass / fail / skip 事象の Elapsed を使う。
// 同じパッケージが複数の記録にあれば最大を取る。実測の無いパッケージは 0 秒と見積もる。
//
// 振り分けは最長処理時間順の貪欲法（LPT）。見積もりの大きい順に、合計の最も小さい shard へ入れる。
// 同点は、パッケージ数の少ない shard、それでも同点なら番号の小さい shard を選ぶ（決定的にするため）。
// パッケージ数より多くは分けない。空の shard は作らない。
//
// 本数は 2 通りで決める。
//   - --parallel と --setup-sec を渡すと、見積もりの合計から本数を決める（PR。shardCount を見る）
//   - --shards を渡すと、その本数に固定する（main の全件実行）
//
// モジュールのパスはカレントの go.mod から読む。説明文の相対パスに使うので、リポジトリのルートで実行する。
//
// 使い方:
//
//	go run ./scripts/shardcount --timing-files 'timings/*.json' --parallel 3 --setup-sec 50 < packages.txt
//	go run ./scripts/shardcount --timing-files 'timings/*.json' --shards 3 < packages.txt
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
	"slices"
	"sort"
	"strings"
	"time"
)

// maxShards は shard 数の上限。PR で本数を見積もりから決めるときの上限であり、
// 実測の無いパッケージがあるときに返す本数でもある。
const maxShards = 3

func main() {
	pattern := flag.String("timing-files", "", "test2json の記録に一致する glob（例: timings/*.json）")
	shards := flag.Int("shards", 0, "shard の本数を固定する（1 以上）。--parallel と --setup-sec と同時には使わない")
	parallel := flag.Int("parallel", 0, "shard 内の実効並列度 k。go test -p の並行数ではなく、実測の合計 ÷ 実時間の比（test は 3、直列の conformance は 1）")
	setupSec := flag.Float64("setup-sec", 0, "shard 1 本の準備時間（秒）。テスト本体より前に毎回かかる分")
	flag.Parse()
	if *pattern == "" {
		fail("shardcount: --timing-files を指定してください")
	}
	fixed := *shards > 0
	if fixed && (*parallel != 0 || *setupSec != 0) {
		fail("shardcount: --shards と --parallel・--setup-sec は同時に指定できません")
	}
	if !fixed && (*parallel < 1 || *setupSec <= 0) {
		fail("shardcount: --shards を指定しないなら --parallel（1 以上）と --setup-sec（正の数）を指定してください")
	}
	module, err := readModulePath("go.mod")
	if err != nil {
		fail(err.Error())
	}
	p := plan{shards: *shards, parallel: *parallel, setupSec: *setupSec}
	if err := run(os.Stdin, os.Stdout, module, *pattern, p); err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

// plan は本数の決め方。shards が 0 より大きければ固定、そうでなければ parallel と setupSec で見積もる。
type plan struct {
	shards   int
	parallel int
	setupSec float64
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

// partition は matrix の include の 1 要素。キーの名前は gotestsum tool ci-matrix の出力と同じ。
type partition struct {
	ID               int    `json:"id"`
	EstimatedRuntime string `json:"estimatedRuntime"`
	Packages         string `json:"packages"`
	Description      string `json:"description"`
}

type matrix struct {
	Include []partition `json:"include"`
}

func run(stdin io.Reader, stdout io.Writer, module, pattern string, p plan) error {
	pkgs, err := readPackages(stdin)
	if err != nil {
		return fmt.Errorf("標準入力のパッケージを読めません: %w", err)
	}
	est, err := estimates(pattern)
	if err != nil {
		return err
	}
	shards := p.shards
	if shards == 0 {
		var total time.Duration
		unmeasured := false
		for _, pkg := range pkgs {
			d, ok := est[pkg]
			if !ok {
				unmeasured = true
				continue
			}
			total += d
		}
		shards = shardCount(total.Seconds(), unmeasured, p.parallel, p.setupSec)
	}
	return json.NewEncoder(stdout).Encode(buildMatrix(pkgs, est, shards, module))
}

// buildMatrix はパッケージを最大 shards 本に振り分けた matrix を作る。
// パッケージが shards 本より少なければ、空の shard は作らない。
// 実測の無いパッケージは 0 秒として振り分ける。
func buildMatrix(pkgs []string, est map[string]time.Duration, shards int, module string) matrix {
	m := matrix{Include: []partition{}}
	n := min(shards, len(pkgs))
	if n == 0 {
		return m
	}

	sorted := slices.Clone(pkgs)
	sort.SliceStable(sorted, func(i, j int) bool {
		if est[sorted[i]] != est[sorted[j]] {
			return est[sorted[i]] > est[sorted[j]]
		}
		return sorted[i] < sorted[j]
	})

	type bucket struct {
		total time.Duration
		pkgs  []string
	}
	buckets := make([]bucket, n)
	for _, pkg := range sorted {
		best := 0
		for i := 1; i < n; i++ {
			if buckets[i].total < buckets[best].total ||
				(buckets[i].total == buckets[best].total && len(buckets[i].pkgs) < len(buckets[best].pkgs)) {
				best = i
			}
		}
		buckets[best].total += est[pkg]
		buckets[best].pkgs = append(buckets[best].pkgs, pkg)
	}

	for i, b := range buckets {
		desc := fmt.Sprintf("%d - %s", i, relativePackagePath(b.pkgs[0], module))
		if len(b.pkgs) > 1 {
			desc += fmt.Sprintf(" and %d others", len(b.pkgs)-1)
		}
		m.Include = append(m.Include, partition{
			ID:               i,
			EstimatedRuntime: b.total.String(),
			Packages:         strings.Join(b.pkgs, " "),
			Description:      desc,
		})
	}
	return m
}

// relativePackagePath は import path からモジュールの接頭辞を外す。モジュールそのものは "." になる。
func relativePackagePath(pkg, module string) string {
	if pkg == module {
		return "."
	}
	return strings.TrimPrefix(pkg, module+"/")
}

// readModulePath は go.mod の module ディレクティブのパスを返す。
func readModulePath(gomod string) (string, error) {
	raw, err := os.ReadFile(gomod)
	if err != nil {
		return "", fmt.Errorf("モジュールのパスを読めません（リポジトリのルートで実行してください）: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("%s に module ディレクティブが無い", gomod)
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

// estimates は記録からパッケージごとの見積もり時間を返す。実測のあるパッケージだけを含む。
// パッケージ単位の pass / fail / skip 事象（Test が空）があれば実測とみなす。
// no test files のパッケージは skip で Elapsed が 0 なので、0 秒の実測になる。
func estimates(pattern string) (map[string]time.Duration, error) {
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("timing-files の glob が不正です: %w", err)
	}
	est := make(map[string]time.Duration)
	for _, name := range files {
		if err := readRecord(name, est); err != nil {
			return nil, err
		}
	}
	return est, nil
}

func readRecord(name string, est map[string]time.Duration) error {
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
		// Elapsed は秒の浮動小数。gotestsum の testjson と同じくミリ秒へ切り捨てる（1.018 は浮動小数で 1.017999… になり、
		// 丸めると ci-matrix と振り分けが変わる）。
		d := time.Duration(ev.Elapsed*1000) * time.Millisecond
		if prev, ok := est[ev.Package]; !ok || d > prev {
			est[ev.Package] = d
		}
	}
}
