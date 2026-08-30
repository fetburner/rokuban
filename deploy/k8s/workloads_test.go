// ロール分割デプロイのワークロード（ScaledJob / CronJob / site 束縛の Pod）に
// 効く検査。manifests_test.go が「オブジェクト間の参照と config の中身」を見るのに
// 対して、こちらは **argv とキュー名とスケジュールの噛み合わせ**を見る。
//
// ここで見ているものは、どれも `kustomize build` にも kubeconform にも見えない:
//
//   - ScaledJob のトリガのクエリが、その Job が実際に購読するキューと違う
//     （症状は「いつまでもスケールしない」か「起きた Job が何もせず終わる」）
//   - site 束縛キューを引く worker に `--sites` が無い（起動時エラー）
//   - encode / thumbnail の worker が ffmpeg 非同梱のイメージを指している
//     （起動時 fail-fast。**キューを増やしたときに写し忘れる**）
//   - `rokuban enqueue` にあるジョブの CronJob が無い（`worker.periodic_jobs:
//     false` の下では、そのパスが一度も走らない構成が黙って出来上がる）
//   - overlay の JSON6902 patch の path が、site 名ではない何かを指している
package k8s

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/fetburner/rokuban/internal/worker"
)

const (
	// officialImage / fullImage は base/kustomization.yaml の `images:` が
	// 置換の対象として宣言している 2 つの名前。**ffmpeg を要る役だけが
	// fullImage を指す**（公式イメージは ffmpeg を同梱しないので、encode /
	// thumbnail キューを購読する worker は起動時に fail-fast する）。
	officialImage = "ghcr.io/fetburner/rokuban"
	fullImage     = "ghcr.io/fetburner/rokuban-full"

	// defaultQueue は「ScaledJob を置かないキュー」。現在どのジョブも
	// river.QueueDefault に入らない（internal/worker の各 InsertOpts が
	// Queue を明示している）。**ジョブを 1 つでも default に入れたら、
	// TestScaledJobsCoverEveryQueue がここで落ちる。**
	defaultQueue = "default"
)

// argsOf はコンテナの command + args を文字列の並びで返す。
func argsOf(c map[string]any) []string {
	var out []string
	for _, key := range []string{"command", "args"} {
		for _, a := range sliceAt(c, key) {
			out = append(out, fmt.Sprint(a))
		}
	}
	return out
}

// flagValue は `--name value` / `--name=value` のどちらの形でも値を返す。
//
// **両方見る。** 片方だけ見ると、`--sites=`（明示的な空 = 中央プロセス）を
// 「フラグが無い」と読んで、site 束縛の検査が素通りする。
func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == "--"+name {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// workload は検査対象 1 件（どのディレクトリ由来かを持つ）。
type workload struct {
	object
	dir string
}

func loadWorkloads(t *testing.T) []workload {
	t.Helper()
	var out []workload
	for _, dir := range []string{baseDir, siteDir} {
		for _, o := range loadDir(t, dir) {
			out = append(out, workload{object: o, dir: dir})
		}
	}
	return out
}

// soleContainer は Pod テンプレートのコンテナがちょうど 1 つであることを
// 要求して返す。**「1 つ目を取る」にしない** --- 2 つ目を足した人が、
// 検査が 1 つ目しか見ていないことに気付けない。
func soleContainer(t *testing.T, w workload) map[string]any {
	t.Helper()
	spec := mapAt(podTemplate(w.object), "spec")
	cs := containers(spec)
	if len(cs) != 1 {
		t.Fatalf("%s (%s) has %d containers, want exactly 1", w.id(), w.dir, len(cs))
	}
	return cs[0]
}

// --- ScaledJob --------------------------------------------------------------

func scaledJobs(t *testing.T) []workload {
	t.Helper()
	var out []workload
	for _, w := range loadWorkloads(t) {
		if w.kind() == "ScaledJob" {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		t.Fatal("no ScaledJob found (nothing would be checked)")
	}
	return out
}

// 全キューにちょうど 1 本の ScaledJob があり、その置き場所（base / site）と
// イメージが、そのキューの要求と一致すること。
//
// **`internal/worker` の側を権威にする。** キュー名の一覧をこのテストに書き写すと、
// キューを 1 つ足した日にマニフェストとテストの**両方**が黙ったままになる
// （そのキューのジョブは永久に誰にも消化されない）。
func TestScaledJobsCoverEveryQueue(t *testing.T) {
	byQueue := map[string][]workload{}
	for _, w := range scaledJobs(t) {
		args := argsOf(soleContainer(t, w))
		q, ok := flagValue(args, "queues")
		if !ok {
			t.Errorf("%s does not pass --queues; it would subscribe to every queue "+
				"(including site-bound ones, which die in verifySite)", w.id())
			continue
		}
		byQueue[q] = append(byQueue[q], w)
	}

	insertsIntoDefault := someJobUsesTheDefaultQueue(t)
	for _, q := range worker.AllQueueNames() {
		if q == defaultQueue && !insertsIntoDefault {
			if got := byQueue[q]; len(got) > 0 {
				t.Errorf("queue %q has a ScaledJob (%s), but no job is inserted into it; "+
					"if that changed, drop this branch instead of keeping a trigger that always returns 0",
					q, got[0].id())
			}
			continue
		}
		got := byQueue[q]
		switch len(got) {
		case 0:
			t.Errorf("queue %q has no ScaledJob; nothing would ever work its jobs "+
				"(worker.periodic_jobs is false, so no long-lived worker picks them up either)", q)
			continue
		case 1:
		default:
			var names []string
			for _, w := range got {
				names = append(names, w.id())
			}
			t.Errorf("queue %q has %d ScaledJobs (%s); KEDA would double-count the same backlog",
				q, len(got), strings.Join(names, ", "))
			continue
		}
		w := got[0]
		args := argsOf(soleContainer(t, w))

		// site 束縛キューはサイト 1 組ぶん（site/）に居て `--sites <site>` を持つ。
		// site 非依存キューは中央（base/）に居て `--sites=`（明示的な空）を持つ。
		site, hasSites := flagValue(args, "sites")
		if worker.RequiresSiteBinding([]string{q}) {
			if w.dir != siteDir {
				t.Errorf("%s subscribes the site-bound queue %q but lives in %s/; "+
					"site-bound queues must be replicated per site (%s/)", w.id(), q, w.dir, siteDir)
			}
			if !hasSites || site != baseSiteName {
				t.Errorf("%s subscribes %q but passes --sites=%q, want %q "+
					"(the physical queue is <queue>_<site>; without the binding the process refuses to start)",
					w.id(), q, site, baseSiteName)
			}
		} else {
			if w.dir != baseDir {
				t.Errorf("%s subscribes the site-independent queue %q but lives in %s/; "+
					"it would be duplicated once per site", w.id(), q, w.dir)
			}
			if !hasSites || site != "" {
				t.Errorf("%s subscribes %q but passes --sites=%q, want an explicit empty value "+
					"(a central process must not bind to a site)", w.id(), q, site)
			}
		}

		// ffmpeg を要るキューだけが `Dockerfile.full` のイメージを指すこと。
		wantImage := officialImage
		if worker.RequiresEncodeTools([]string{q}) {
			wantImage = fullImage
		}
		image := strAt(soleContainer(t, w), "image")
		if name, _, _ := strings.Cut(image, ":"); name != wantImage {
			t.Errorf("%s (queue %q) uses image %q, want %q "+
				"(worker.RequiresEncodeTools decides; the official image has no ffmpeg and fail-fasts)",
				w.id(), q, image, wantImage)
		}
	}

	// 逆向き: 知らないキュー名の ScaledJob が無いこと（起動時エラーになる）。
	for q, ws := range byQueue {
		if !slices.Contains(worker.AllQueueNames(), q) {
			t.Errorf("%s passes --queues %q, which is not a known queue (valid: %s)",
				ws[0].id(), q, strings.Join(worker.AllQueueNames(), ", "))
		}
	}
}

// someJobUsesTheDefaultQueue は `internal/worker` のどれかの `InsertOpts` が
// `river.QueueDefault` を使っているかを見る。
//
// **「誰も入れないから ScaledJob を置かない」という判断を、判断のまま腐らせない
// ため。** 置かない理由が消えた日（誰かが `Queue: river.QueueDefault` と書いた
// 日）に、上の分岐が黙って「無くてよい」と言い続けるのを止める。**それが無いと、
// そのキューのジョブは永久に誰にも消化されない**（`worker.periodic_jobs: false`
// なので常駐 worker も居ない）。
//
// 走査の形が変わって何も見つからなくなる（＝この検査が黙って死ぬ）ことを防ぐ
// ため、**`Queue:` の指定そのものが 1 つも見つからなければ落とす。**
//
// **Go のソースを読む検査はこのファイルで 2 つあり、どちらも go/ast で読む**
// （もう 1 つは enqueueJobsFromSource）。行の文字列一致で書くと、コメントや
// 文字列リテラルの中の同じ語を数えてしまう。
func someJobUsesTheDefaultQueue(t *testing.T) bool {
	t.Helper()
	assignments, defaults := 0, 0
	for _, f := range parseGoFiles(t, "../../internal/worker/*.go") {
		ast.Inspect(f, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Queue" {
				return true
			}
			assignments++
			if sel, ok := kv.Value.(*ast.SelectorExpr); ok && sel.Sel.Name == "QueueDefault" {
				defaults++
			}
			return true
		})
	}
	if assignments == 0 {
		t.Fatal("no `Queue:` field is set anywhere in internal/worker (the scan no longer sees the InsertOpts; this check is blind)")
	}
	return defaults > 0
}

// parseGoFiles は glob に一致する非テストの Go ファイルを構文木にして返す。
func parseGoFiles(t *testing.T, glob string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(glob)
	if err != nil || len(paths) == 0 {
		t.Fatalf("globbing %s: %v", glob, err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", p, err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatalf("%s matched only test files", glob)
	}
	return out
}

// triggerQuery は ScaledJob の 1 本目のトリガのクエリを、空白を 1 つに畳んで返す。
func triggerQuery(t *testing.T, w workload) string {
	t.Helper()
	triggers := sliceAt(w.doc, "spec", "triggers")
	if len(triggers) != 1 {
		t.Fatalf("%s has %d triggers, want exactly 1 (the checks below assume triggers[0])", w.id(), len(triggers))
	}
	tr, _ := triggers[0].(map[string]any)
	if got := strAt(tr, "type"); got != "postgresql" {
		t.Fatalf("%s trigger type = %q, want postgresql", w.id(), got)
	}
	return strings.Join(strings.Fields(strAt(tr, "metadata", "query")), " ")
}

// トリガのクエリが、その ScaledJob が実際に購読する**物理**キュー名を数えていること。
//
// **論理名と物理名は違う。** site 束縛キューは `<論理名>_<site>` に修飾される
// （internal/worker の qualifyQueueName）ので、クエリに論理名を書くと
// **誰も入れないキューを数え続けて永久にスケールしない**。逆にサイトを跨いだ
// 名前を書くと、サイト A のスケーラがサイト B の滞留で Job を起こし、起きた Job は
// verifySite で死んでまた起きる（受け入れ判定ハーネスの判定 5）。
//
// 数える状態が `available` と `retryable` の 2 つであることも一緒に見る。
// `retryable` を落とすと、失敗したジョブを `available` へ戻す `JobScheduler` を
// 動かす常駐クライアントがロール分割構成には居ないので、**失敗したジョブが
// 永久に止まる**。
func TestScaledJobTriggersMatchTheirQueue(t *testing.T) {
	for _, w := range scaledJobs(t) {
		args := argsOf(soleContainer(t, w))
		q, ok := flagValue(args, "queues")
		if !ok {
			continue // TestScaledJobsCoverEveryQueue が報告済み
		}
		physical := q
		if worker.RequiresSiteBinding([]string{q}) {
			physical = q + "_" + baseSiteName
		}
		want := fmt.Sprintf(
			"SELECT count(*) FROM river_job WHERE queue = '%s' AND state IN ('available','retryable')",
			physical)
		if got := triggerQuery(t, w); got != want {
			t.Errorf("%s trigger query\n got: %s\nwant: %s", w.id(), got, want)
		}
	}
}

// 1 件消化モードの形（`--once` / `backoffLimit: 0` / `restartPolicy: Never` /
// `rollout.strategy: gradual` / `--soft-stop-timeout` の明示）が揃っていること。
//
// **`rollout.strategy: gradual` は書き忘れてはならない 1 行である。** 省略も
// `immediate` も、Pod テンプレートの更新で実行中の Job が消える（kind で実測。
// docs/operations.md §5 の表）。運用で踏むのはイメージのタグを上げたときで、
// 症状は「デプロイしたら録画のエンコードが飛ぶ」になる。
//
// `--once` が無いと Job は `succeeded` に到達せず、KEDA から見ると
// 「いつまでも実行中」になる（0 → 1 → 0 が成立しない）。
func TestScaledJobsRunOnceWorkers(t *testing.T) {
	for _, w := range scaledJobs(t) {
		args := argsOf(soleContainer(t, w))

		if !slices.Contains(args, "--once") {
			t.Errorf("%s does not pass --once; the Job would never finish and KEDA's 0 -> 1 -> 0 never completes", w.id())
		}
		roles, _ := flagValue(args, "roles")
		if roles != "worker" {
			t.Errorf("%s passes --roles %q, want exactly \"worker\" (once mode refuses to share a process with long-lived roles)", w.id(), roles)
		}
		// **`--queues` はちょうど 1 つ。** 1 件消化モードが要求する
		// （複数キューだと「1 Job = 1 アイテム」の算術が崩れる）。
		if q, ok := flagValue(args, "queues"); ok && strings.Contains(q, ",") {
			t.Errorf("%s passes --queues %q; once mode requires exactly one queue", w.id(), q)
		}
		if _, ok := flagValue(args, "soft-stop-timeout"); !ok {
			t.Errorf("%s does not pass --soft-stop-timeout; the default (5s) is chosen so that "+
				"deployments which wrote nothing are not SIGKILLed, not because it fits this queue", w.id())
		}

		if got := strAt(w.doc, "spec", "rollout", "strategy"); got != "gradual" {
			t.Errorf("%s rollout.strategy = %q, want %q (anything else drops running jobs when the pod template changes)",
				w.id(), got, "gradual")
		}
		// **River がリトライを持つので k8s 側では再試行しない。** `--once` は
		// 成功・失敗を問わず exit 0 なので、`backoffLimit > 0` にしても
		// Job が作り直されるのは Pod が異常終了したときだけだが、そのときは
		// 二重にリトライすることになる。
		if got := fmt.Sprint(mapAt(w.doc, "spec", "jobTargetRef")["backoffLimit"]); got != "0" {
			t.Errorf("%s jobTargetRef.backoffLimit = %s, want 0 (River owns the retries)", w.id(), got)
		}
		if got := strAt(podTemplate(w.object), "spec", "restartPolicy"); got != "Never" {
			t.Errorf("%s restartPolicy = %q, want Never", w.id(), got)
		}
		if got := strAt(w.doc, "spec", "scalingStrategy", "strategy"); got != "accurate" {
			t.Errorf("%s scalingStrategy.strategy = %q, want %q (1 Job = 1 item, so the backlog count is the wanted job count)",
				w.id(), got, "accurate")
		}
	}
}

// softStopSeconds は `--soft-stop-timeout` に書いた値を秒で返す。
//
// **cobra が読むのと同じパーサを使う**（`time.ParseDuration`）。独自の書式に
// 狭めると、**このテストは通るのにプロセスが起動しない値**を見逃しうる。
func softStopSeconds(t *testing.T, s string) int {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("--soft-stop-timeout %q: %v", s, err)
	}
	return int(d.Seconds())
}

// worker ロールの Pod の `terminationGracePeriodSeconds` が drain を包むこと。
//
//	grace > preStop の sleep + 10s + --soft-stop-timeout + 10s
//
// 内訳は docs/operations.md §5「Deployment 併用時」。**等号にしない** ---
// 右辺はプロセスが消えるまでの最悪値そのものなので、等号だと SIGKILL と同着に
// なる。同着になるのは予算を使い切ったケース、つまりここで避けようとしている
// 「行が `running` のまま残る」経路そのものである。**そのとき回収する
// `JobRescuer` はリーダーだけが動かす保守サービスなので、ロール分割構成では
// 誰も回収しない。**
//
// api（worker ロールを持たない）側の足し算は
// manifests_test.go の TestTerminationBudgetCoversPreStop。
func TestWorkerGraceCoversTheSoftStop(t *testing.T) {
	checked := 0
	for _, w := range scaledJobs(t) {
		c := soleContainer(t, w)
		args := argsOf(c)
		soft, ok := flagValue(args, "soft-stop-timeout")
		if !ok {
			continue // TestScaledJobsRunOnceWorkers が報告済み
		}
		checked++

		spec := mapAt(podTemplate(w.object), "spec")
		grace, err := strconv.Atoi(fmt.Sprint(spec["terminationGracePeriodSeconds"]))
		if err != nil {
			t.Errorf("%s: parsing terminationGracePeriodSeconds %v: %v", w.id(), spec["terminationGracePeriodSeconds"], err)
			continue
		}

		preStop := 0
		for _, a := range sliceAt(c, "lifecycle", "preStop", "exec", "command") {
			if n, err := strconv.Atoi(fmt.Sprint(a)); err == nil {
				preStop = n
			}
		}

		// 10 は cmd/rokuban/server.go の `httpShutdownTimeout`（停止待ち）と、
		// 猶予が切れたあと畳み終えるぶん。**実装の定数を参照せずリテラルで書く**。
		const stopWait, teardown = 10, 10
		want := preStop + stopWait + softStopSeconds(t, soft) + teardown
		if grace <= want {
			t.Errorf("%s terminationGracePeriodSeconds = %d, want > %d "+
				"(preStop %ds + stop wait %ds + --soft-stop-timeout %s + teardown %ds)",
				w.id(), grace, want, preStop, stopWait, soft, teardown)
		}
	}
	if checked == 0 {
		t.Error("no worker pod was checked (the --soft-stop-timeout flag disappeared from every ScaledJob)")
	}
}

// --- CronJob ----------------------------------------------------------------

// enqueueJobSpec は `rokuban enqueue` が受け付けるジョブ 1 件。
type enqueueJobSpec struct {
	Name         string
	RequiresSite bool
}

// enqueueJobsFromSource は cmd/rokuban/enqueue.go の `enqueueJobs` を読む。
//
// **一覧をこのテストに書き写さない。** 書き写すと、ジョブを 1 つ足した日に
// マニフェストとテストの両方が黙ったままになり、`worker.periodic_jobs: false`
// の構成（k8s の出荷値）ではそのパスが一度も走らない。`rokuban enqueue --help`
// が権威だと docs が言っているのは、まさにこの表のことである。
//
// 実行せずに AST で読むのは、テストがビルドとネットワークに依存しないようにする
// ため（`go run ./cmd/rokuban enqueue --help` でも同じことはできる）。
func enqueueJobsFromSource(t *testing.T) []enqueueJobSpec {
	t.Helper()
	var lit *ast.CompositeLit
	ast.Inspect(parseGoFiles(t, enqueueSourcePath)[0], func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "enqueueJobs" || len(vs.Values) != 1 {
			return true
		}
		lit, _ = vs.Values[0].(*ast.CompositeLit)
		return false
	})
	if lit == nil {
		t.Fatalf("%s: could not find the enqueueJobs map literal (the shape changed; this check is now blind)", enqueueSourcePath)
	}

	var out []enqueueJobSpec
	for _, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			continue
		}
		name, err := strconv.Unquote(key.Value)
		if err != nil {
			t.Fatalf("%s: unquoting job name %s: %v", enqueueSourcePath, key.Value, err)
		}
		spec := enqueueJobSpec{Name: name}
		if body, ok := kv.Value.(*ast.CompositeLit); ok {
			for _, fe := range body.Elts {
				fkv, ok := fe.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := fkv.Key.(*ast.Ident); ok && id.Name == "RequiresSite" {
					if v, ok := fkv.Value.(*ast.Ident); ok {
						spec.RequiresSite = v.Name == "true"
					}
				}
			}
		}
		out = append(out, spec)
	}
	if len(out) == 0 {
		t.Fatalf("%s: enqueueJobs parsed as empty (the shape changed)", enqueueSourcePath)
	}
	return out
}

func cronJobs(t *testing.T) []workload {
	t.Helper()
	var out []workload
	for _, w := range loadWorkloads(t) {
		if w.kind() == "CronJob" {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		t.Fatal("no CronJob found (nothing would insert the periodic jobs)")
	}
	return out
}

// `rokuban enqueue` の全ジョブに CronJob があり、site 束縛のものだけが
// `--site` を取ること。
//
// **これが無いと「そのパスが一度も走らない構成」が黙って出来上がる。**
// `worker.periodic_jobs: false` で出荷しているので、投入経路は CronJob だけである。
func TestCronJobsCoverEveryEnqueueJob(t *testing.T) {
	byJob := map[string][]workload{}
	for _, w := range cronJobs(t) {
		args := argsOf(soleContainer(t, w))
		if len(args) < 2 || args[0] != "enqueue" {
			t.Errorf("%s argv = %v, want `enqueue <job> ...` as flat elements", w.id(), args)
			continue
		}
		byJob[args[1]] = append(byJob[args[1]], w)
	}

	known := map[string]bool{}
	for _, spec := range enqueueJobsFromSource(t) {
		known[spec.Name] = true
		got := byJob[spec.Name]
		if len(got) != 1 {
			t.Errorf("`rokuban enqueue %s` has %d CronJob(s), want exactly 1 "+
				"(worker.periodic_jobs is false, so nothing else inserts it)", spec.Name, len(got))
			continue
		}
		w := got[0]
		args := argsOf(soleContainer(t, w))
		site, hasSite := flagValue(args, "site")

		wantDir := baseDir
		if spec.RequiresSite {
			wantDir = siteDir
		}
		if w.dir != wantDir {
			t.Errorf("%s enqueues %q (RequiresSite=%v) but lives in %s/, want %s/",
				w.id(), spec.Name, spec.RequiresSite, w.dir, wantDir)
		}
		switch {
		case spec.RequiresSite && (!hasSite || site != baseSiteName):
			t.Errorf("%s enqueues the site-bound job %q with --site=%q, want %q "+
				"(a registry with two entries makes --site mandatory)", w.id(), spec.Name, site, baseSiteName)
		case !spec.RequiresSite && hasSite:
			t.Errorf("%s enqueues the site-independent job %q with --site; "+
				"`rokuban enqueue` rejects that instead of ignoring it", w.id(), spec.Name)
		}
	}

	for job, ws := range byJob {
		if !known[job] {
			t.Errorf("%s enqueues %q, which `rokuban enqueue` does not accept", ws[0].id(), job)
		}
	}
}

// CronJob の argv がシェルを挟まない平たい要素であること。
//
// **`sh -c "rokuban enqueue ..."` でくるむと 2 つ壊れる。** 受け入れ判定
// ハーネスが投入側の CronJob を見つけられなくなり（探索は argv の要素として
// `enqueue` と ジョブ名 と `--site <site>` を見る。deploy/k8s/e2e/lib/kube.sh）、
// `rokuban` の終了コードがシェルのものに化ける。
func TestCronJobArgsAreFlatElements(t *testing.T) {
	for _, w := range cronJobs(t) {
		c := soleContainer(t, w)
		for _, a := range argsOf(c) {
			if strings.ContainsAny(a, " \t\n") {
				t.Errorf("%s argv element %q contains whitespace; write the arguments as separate elements "+
					"(a shell wrapper hides both the job name and the exit code)", w.id(), a)
			}
		}
		if cmd := sliceAt(c, "command"); len(cmd) > 0 {
			t.Errorf("%s overrides command (%v); the image's entrypoint is rokuban itself", w.id(), cmd)
		}
	}
}

// --- schedule ---------------------------------------------------------------

// productionSchedules は base が出荷する schedule。**リテラルで書く**
// （internal/worker の既定値を参照すると、両方を同時に変えたときに何も主張
// しなくなる）。値の根拠は各 CronJob のコメント（in-process の既定間隔）。
var productionSchedules = map[string]string{
	"rokuban-enqueue-epg-sync":         "*/10 * * * *",
	"rokuban-enqueue-tuner-sync":       "*/10 * * * *",
	"rokuban-enqueue-ruler-pass":       "*/10 * * * *",
	"rokuban-enqueue-reconcile-pass":   "* * * * *",
	"rokuban-enqueue-record-sweep":     "*/5 * * * *",
	"rokuban-enqueue-catalog-export":   "0 4 * * *",
	"rokuban-enqueue-delete-reconcile": "*/15 * * * *",
	"rokuban-enqueue-encode-reconcile": "*/15 * * * *",
	"rokuban-enqueue-storage-sync":     "*/5 * * * *",
}

// base / site の schedule が実運用の間隔であること。
//
// **受け入れ判定は 180 秒以内の自然発火を要求する**が、そのために base を
// 短くしない。判定用の間隔は `overlays/e2e` が patch で当てる
// （TestE2EOverlayShortensTheCronScheduleItMeasures と対で意味を持つ）。
// 対で見ていないと、「判定が緑になるから」で base の epg-sync が毎分になり、
// **出荷されたデプロイが 10 分ぶんの EPG 同期を毎分打つ**。
func TestCronSchedulesAreProductionValues(t *testing.T) {
	seen := map[string]bool{}
	for _, w := range cronJobs(t) {
		want, ok := productionSchedules[w.name()]
		if !ok {
			t.Errorf("%s has no expected schedule in productionSchedules; add it with the in-process default it mirrors", w.id())
			continue
		}
		seen[w.name()] = true
		if got := strAt(w.doc, "spec", "schedule"); got != want {
			t.Errorf("%s schedule = %q, want %q", w.id(), got, want)
		}
	}
	for name := range productionSchedules {
		if !seen[name] {
			t.Errorf("productionSchedules lists %q, which no CronJob defines", name)
		}
	}
}

// e2e overlay が「判定 2 が測る CronJob」だけを毎分に落としていること。
//
// 判定 2.3 は CronJob が**自分の schedule で** 180 秒以内に発火することを見る
// （`kubectl create job --from=cronjob` で代用すると、schedule が止まっていても
// 緑になるため）。base の 10 分では届かないので overlay で毎分にする。
//
// **したがって判定 2 が測るのは出荷される schedule ではない。**
// その旨は deploy/k8s/e2e/README.md の「0 が保証しないもの」にある。
func TestE2EOverlayShortensTheCronScheduleItMeasures(t *testing.T) {
	const target = "rokuban-enqueue-epg-sync"

	k := loadOverlayPatches(t, filepath.Join(overlaysDir, "e2e"))
	found := false
	for _, p := range k.Patches {
		if p.Target.Kind != "CronJob" || p.Target.Name != target {
			continue
		}
		for _, op := range parseJSONPatch(t, p.Patch) {
			if op.Path != "/spec/schedule" {
				continue
			}
			found = true
			if got := fmt.Sprint(op.Value); got != "* * * * *" {
				t.Errorf("overlays/e2e patches %s schedule to %q, want every minute (check 2.3 waits 180s)", target, got)
			}
		}
	}
	if !found {
		t.Errorf("overlays/e2e does not patch %s's schedule; check 2.3 would wait 180s for a 10-minute schedule and FAIL", target)
	}
}

// --- site overlay の patch --------------------------------------------------

type patchTarget struct {
	Kind          string `yaml:"kind"`
	Name          string `yaml:"name"`
	LabelSelector string `yaml:"labelSelector"`
}

type patchEntry struct {
	Target patchTarget `yaml:"target"`
	Patch  string      `yaml:"patch"`
	Path   string      `yaml:"path"`
}

type overlayPatches struct {
	Resources  []string     `yaml:"resources"`
	NameSuffix string       `yaml:"nameSuffix"`
	Patches    []patchEntry `yaml:"patches"`
}

type jsonPatchOp struct {
	Op    string `yaml:"op"`
	Path  string `yaml:"path"`
	Value any    `yaml:"value"`
}

func loadOverlayPatches(t *testing.T, dir string) overlayPatches {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading %s/kustomization.yaml: %v", dir, err)
	}
	var k overlayPatches
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("decoding %s/kustomization.yaml: %v", dir, err)
	}
	return k
}

func parseJSONPatch(t *testing.T, body string) []jsonPatchOp {
	t.Helper()
	var ops []jsonPatchOp
	if err := yaml.Unmarshal([]byte(body), &ops); err != nil {
		t.Fatalf("decoding the inline patch %q: %v", body, err)
	}
	return ops
}

// resolvePointer は JSON Pointer（`/a/0/b`）を map/slice の木の上で辿る。
func resolvePointer(root any, pointer string) (any, bool) {
	cur := root
	for _, seg := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// siteOverlayDirs は overlays/*/sites/* を列挙する。
func siteOverlayDirs(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(overlaysDir, "*", "sites", "*", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("globbing site overlays: %v", err)
	}
	var dirs []string
	for _, m := range matches {
		dirs = append(dirs, filepath.Dir(m))
	}
	return dirs
}

// 多サイト overlay の patch が、**実際に site 名の位置を指している**こと。
//
// patch の path は `deploy/k8s/site` の argv の添字である。引数を並べ替えると、
// overlay は黙って別の値を書き換える --- 症状は「site を増やしたら
// `--config` が `sitea` になって起動しない」や、もっと悪くて
// 「`--soft-stop-timeout` が site 名になって起動しない」になる。
//
// **`kustomize build` は何も言わない**（JSON6902 の replace は、その位置に値が
// あれば通る）。ここで「その位置に今 `default` が居ること」を固定しておくと、
// 並べ替えは overlay ではなくテストで落ちる。
func TestSitePatchesTargetTheSiteName(t *testing.T) {
	dirs := siteOverlayDirs(t)
	if len(dirs) == 0 {
		t.Fatalf("no site overlay found under %s/*/sites/*; TestSitePatchesTargetTheSiteName checks nothing", overlaysDir)
	}

	site := loadSite(t)
	for _, dir := range dirs {
		t.Run(dir, func(t *testing.T) {
			k := loadOverlayPatches(t, dir)
			siteName := strings.TrimPrefix(k.NameSuffix, "-")
			if siteName == "" {
				t.Fatalf("%s has no nameSuffix; the second site's objects would collide with the first's", dir)
			}
			if len(k.Patches) == 0 {
				t.Fatalf("%s has no patches; every object would keep the site name %q", dir, baseSiteName)
			}

			for _, p := range k.Patches {
				matched := 0
				for _, o := range site {
					if p.Target.Kind != "" && o.kind() != p.Target.Kind {
						continue
					}
					if p.Target.Name != "" && o.name() != p.Target.Name {
						continue
					}
					if sel := p.Target.LabelSelector; sel != "" {
						key, value, _ := strings.Cut(sel, "=")
						if fmt.Sprint(mapAt(o.doc, "metadata", "labels")[key]) != value {
							continue
						}
					}
					matched++

					for _, op := range parseJSONPatch(t, p.Patch) {
						if op.Op != "replace" {
							t.Errorf("%s: patch op %q on %s is not a replace; this check only understands replace", dir, op.Op, o.id())
							continue
						}
						cur, ok := resolvePointer(o.doc, op.Path)
						if !ok {
							t.Errorf("%s: patch path %s does not resolve in %s (kustomize would fail at build time, but only for this overlay)",
								dir, op.Path, o.id())
							continue
						}
						// argv の添字を指す patch は、いま site 名が居る位置で
						// なければならない。
						if strings.Contains(op.Path, "/args/") {
							if fmt.Sprint(cur) != baseSiteName {
								t.Errorf("%s: patch path %s points at %q in %s, want the site name %q "+
									"(the arguments were reordered; this patch now rewrites something else)",
									dir, op.Path, fmt.Sprint(cur), o.id(), baseSiteName)
							}
							if fmt.Sprint(op.Value) != siteName {
								t.Errorf("%s: patch writes %q at %s, want %q (the site this overlay is for)",
									dir, fmt.Sprint(op.Value), op.Path, siteName)
							}
						}
						// トリガのクエリは「base のクエリの site 名だけを
						// 差し替えたもの」であること。
						if strings.HasSuffix(op.Path, "/metadata/query") {
							want := strings.ReplaceAll(fmt.Sprint(cur), "_"+baseSiteName+"'", "_"+siteName+"'")
							got := strings.Join(strings.Fields(fmt.Sprint(op.Value)), " ")
							if got != strings.Join(strings.Fields(want), " ") {
								t.Errorf("%s: patched trigger query for %s\n got: %s\nwant: %s", dir, o.id(), got, want)
							}
						}
					}
				}
				if matched == 0 {
					t.Errorf("%s: patch target %+v matches no object in %s/; it is silently doing nothing",
						dir, p.Target, siteDir)
				}
			}
		})
	}
}

// --- Prometheus --------------------------------------------------------------

// 常駐する役（Deployment）の Pod テンプレートが scrape の annotation を持つこと。
//
// **ロールに関わらず全プロセスが `/metrics` を出す**（HTTP リスナーはロールに
// 関わらず 1 本立つ。docs/operations.md §1）。にもかかわらず annotation を
// 書き忘れると、**その役でしか進まないメトリクスだけが誰にも scrape されない**
// --- 出力は「ダッシュボードにその系列が無い」で、Pod は正常に見える。
// 新しい役を足すときに落ちる形にしておく。
//
// **ScaledJob が起こす Job の Pod は対象外。** 数秒で消えるので scrape が
// 間に合わない（ジョブ側の観測は DB を引くゲージで行う。同 §1）。
func TestLongLivedPodsAreScrapable(t *testing.T) {
	want := map[string]string{
		"prometheus.io/scrape": "true",
		// config.yml の `server.listen` と同じポート。**リテラルで書く**
		// （TestConfigListenPortMatchesContainerPort が config 側と
		// containerPort の一致を見ているので、ここは 3 つ目の写しになる）。
		"prometheus.io/port": "40773",
		"prometheus.io/path": "/metrics",
	}
	checked := 0
	for _, o := range loadAll(t) {
		if o.kind() != "Deployment" {
			continue
		}
		checked++
		annotations := mapAt(podTemplate(o), "metadata", "annotations")
		for k, v := range want {
			if got := fmt.Sprint(annotations[k]); got != v {
				t.Errorf("%s pod template annotation %s = %q, want %q "+
					"(every long-lived role serves /metrics; a missing annotation silently drops that role's series)",
					o.id(), k, got, v)
			}
		}
	}
	if checked == 0 {
		t.Error("no Deployment was checked")
	}
}

// --- CRD の中の名前参照 ------------------------------------------------------

// podTemplatePath は podTemplate() が掘る場所を kustomize の fieldSpec の
// 書き方（`/` 区切り）で返す。**podTemplate() の switch と対で保つ。**
func podTemplatePath(kind string) string {
	switch kind {
	case "Deployment", "Job", "StatefulSet", "DaemonSet", "ReplicaSet":
		return "spec/template"
	case "CronJob":
		return "spec/jobTemplate/spec/template"
	case "ScaledJob":
		return "spec/jobTargetRef/template"
	default:
		return ""
	}
}

// builtinWorkloadKinds は kustomize が名前参照の書き換え方を最初から知っている kind。
var builtinWorkloadKinds = []string{"Deployment", "Job", "CronJob", "StatefulSet", "DaemonSet", "ReplicaSet"}

// nameReferenceConfig は base/kustomizeconfig.yaml の形。
type nameReferenceConfig struct {
	NameReference []struct {
		Kind       string `yaml:"kind"`
		FieldSpecs []struct {
			Path string `yaml:"path"`
			Kind string `yaml:"kind"`
		} `yaml:"fieldSpecs"`
	} `yaml:"nameReference"`
}

// CRD の Pod テンプレートが持つ ConfigMap / Secret 参照が、すべて
// `configurations:` で kustomize に教えてあること。
//
// **これが無いと、generator が付けたハッシュ名に追随するのは kustomize が
// 知っている kind だけになる。** CRD の中身は kustomize にとって不透明なので、
// `ScaledJob` の Pod テンプレートに書いた `rokuban-config` はそのまま残り、
// 実際の ConfigMap は `rokuban-config-<hash>` になる。
//
// **症状は「apply は成功、Job の Pod が永久に ContainerCreating」**である
// （実測: `MountVolume.SetUp failed for volume "config" : configmap
// "rokuban-config" not found`）。`kustomize build` も kubeconform も、そして
// このファイルの参照解決の検査（TestManifestReferencesResolve）も緑のまま通る
// --- あちらは参照先を **generator の宣言名**で解決するので、ハッシュの付き方は
// 見ていない。**kind に載せるまで分からない類そのもの**なので、形で押さえる。
func TestCRDNameReferencesAreDeclared(t *testing.T) {
	k := loadKustomization(t)
	if len(k.Configurations) == 0 {
		t.Fatal("base/kustomization.yaml has no configurations:; CRD name references would not follow the generator hash")
	}

	// 宣言されている (kind, path) の集合。
	declared := map[string]bool{}
	for _, path := range k.Configurations {
		raw, err := os.ReadFile(filepath.Join(baseDir, path))
		if err != nil {
			t.Fatalf("reading %s (listed in configurations:): %v", path, err)
		}
		var cfg nameReferenceConfig
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		for _, ref := range cfg.NameReference {
			for _, fs := range ref.FieldSpecs {
				declared[fs.Kind+" "+ref.Kind+" "+fs.Path] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("configurations: declares no nameReference field specs (the file shape changed)")
	}

	checked := 0
	for _, o := range loadAll(t) {
		kind := o.kind()
		if slices.Contains(builtinWorkloadKinds, kind) {
			continue
		}
		prefix := podTemplatePath(kind)
		if prefix == "" {
			continue
		}
		spec := mapAt(podTemplate(o), "spec")

		// 実際に使っている参照の書き方だけを要求する（使っていない形まで
		// 宣言を強制すると、宣言が増える一方で誰も確かめなくなる）。
		want := func(refKind, path string) {
			checked++
			key := kind + " " + refKind + " " + prefix + "/" + path
			if !declared[key] {
				t.Errorf("%s references a %s at %s/%s, which base/kustomization.yaml's configurations: does not declare; "+
					"the generator hash would not be applied there (the pod stays in ContainerCreating)",
					o.id(), refKind, prefix, path)
			}
		}
		for _, v := range sliceAt(spec, "volumes") {
			vol, _ := v.(map[string]any)
			if mapAt(vol, "configMap") != nil {
				want("ConfigMap", "spec/volumes/configMap/name")
			}
			if mapAt(vol, "secret") != nil {
				want("Secret", "spec/volumes/secret/secretName")
			}
		}
		for _, c := range containers(spec) {
			for _, ef := range sliceAt(c, "envFrom") {
				src, _ := ef.(map[string]any)
				if mapAt(src, "configMapRef") != nil {
					want("ConfigMap", "spec/containers/envFrom/configMapRef/name")
				}
				if mapAt(src, "secretRef") != nil {
					want("Secret", "spec/containers/envFrom/secretRef/name")
				}
			}
			for _, e := range sliceAt(c, "env") {
				env, _ := e.(map[string]any)
				from := mapAt(env, "valueFrom")
				if mapAt(from, "configMapKeyRef") != nil {
					want("ConfigMap", "spec/containers/env/valueFrom/configMapKeyRef/name")
				}
				if mapAt(from, "secretKeyRef") != nil {
					want("Secret", "spec/containers/env/valueFrom/secretKeyRef/name")
				}
			}
		}
	}
	if checked == 0 {
		t.Error("no CRD workload reference was checked (podTemplatePath no longer reaches any custom kind)")
	}
}

// --- base が単一サイトのままであること --------------------------------------

// base が Pod に site を束縛しておらず、レジストリも 1 サイトのままであること。
//
// **これは受け入れ基準そのものである**（「多サイト overlay が『レジストリに
// 2 要素目 + Pod セット 1 組』の差分で書けている。単一サイトの base はレジストリ
// 1 要素のまま」）。base のレジストリが 2 要素以上に膨らむと、サイトを増やす
// 差分が「base を書き換える」に化けて、単一サイト構成の見た目が保てなくなる。
//
// **`mirakcs:` が site 名を書かない形は無い**（R-10 で `mirakc:` 単一オブジェクト
// の糖衣を廃止したため）。そのため base/config.yml も `mirakcs:` 1 要素として
// site 名 `default`（baseSiteName）を明示する。base が「site を一言も知らない」
// わけではなく、「複数サイトを知らない・Pod に site を束縛しない」ことがここでの
// 保証。
func TestBaseIsSiteIndependent(t *testing.T) {
	for _, o := range loadBase(t) {
		spec := mapAt(podTemplate(o), "spec")
		if spec == nil {
			continue
		}
		for _, c := range containers(spec) {
			args := argsOf(c)
			if site, ok := flagValue(args, "sites"); ok && site != "" {
				t.Errorf("%s container %q passes --sites %q; site-bound workloads belong in %s/",
					o.id(), strAt(c, "name"), site, siteDir)
			}
			if site, ok := flagValue(args, "site"); ok {
				t.Errorf("%s container %q passes --site %q; site-bound enqueue jobs belong in %s/",
					o.id(), strAt(c, "name"), site, siteDir)
			}
		}
	}

	// config 側も見る。base は `mirakcs:` を 1 要素だけ持つ --- 2 要素目を
	// 足すのは overlay の仕事である。
	raw, err := os.ReadFile(filepath.Join(baseDir, configFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", configFileName, err)
	}
	var doc struct {
		Mirakcs []map[string]any `yaml:"mirakcs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding %s: %v", configFileName, err)
	}
	if got := len(doc.Mirakcs); got != 1 {
		t.Errorf("%s/%s declares %d mirakcs site(s), want exactly 1; adding a second site "+
			"is overlay's job, not base's", baseDir, configFileName, got)
	}
}
