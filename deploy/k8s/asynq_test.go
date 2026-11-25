package k8s

// asynq 構成（`asynq/` component と `overlays/asynq`・`overlays/e2e-asynq`）の検査。
// River 構成の検査（workloads_test.go）は base / site / store だけを読むので、
// asynq で差し替えたものを別に見張らないと、トリガの取り違えが緑のまま通る。
//
// **kustomize は patch の target が 1 つも一致しなくても黙って無視する。** だから
// ここでは「patch が当たったか」ではなく「当たるべき ScaledJob の集合と、patch の
// 集合が一致するか」を見る。命名（nameSuffix）と物理キュー名は overlay ごとに違う。

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fetburner/rokuban/internal/config"
)

const (
	asynqComponentDir = "asynq"
	asynqOverlayDir   = "overlays/asynq"
	asynqE2EDir       = "overlays/e2e-asynq"
	e2eOverlayDir     = "overlays/e2e"
)

// asynqOverlay は、トリガを当てる overlay の名前付けを表す。値は overlay の
// kustomization.yaml（namespace・resources・nameSuffix）から読む。手書きにすると、
// overlay を変えてもテストが通ったまま、トリガだけが空振りする。
type asynqOverlay struct {
	dir       string
	sites     []string // site 束縛キューを複製する site 名（overlay の resources から読む）
	suffixed  bool     // site の ScaledJob 名に nameSuffix（`-<site>`）が付くか
	namespace string   // notifier の FQDN の名前空間（overlay の namespace: から読む。無ければ default）
	stores    []string // 置き場名（nameSuffix は付かない）
}

// kustomizationFields は overlay の kustomization.yaml のうち名前付けに効く欄。
type kustomizationFields struct {
	Namespace  string   `yaml:"namespace"`
	NameSuffix string   `yaml:"nameSuffix"`
	Resources  []string `yaml:"resources"`
}

func readKustomizationFields(t *testing.T, dir string) kustomizationFields {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading %s/kustomization.yaml: %v", dir, err)
	}
	var k kustomizationFields
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("decoding %s/kustomization.yaml: %v", dir, err)
	}
	return k
}

// asynqOverlayOf は overlay dir の名前付けを、kustomization の resources を辿って読む。
// site/ を直接含む resource はサイト 1 つ（既定名・suffix 無し）、site/ を含む
// サブ overlay（`sites/<name>`）はサイト 1 つ（名前はディレクトリ名、suffix は nameSuffix）、
// store/ を含む resource は置き場 1 つ、それ以外の overlay は再帰して取り込む。
// 名前は overlays/e2e の `sites/<name>` のディレクトリ名と argv の site 名が一致する前提である
// （同じ前提は deploy/k8s/overlays/e2e/sites/ の kustomization に書いてある）。
func asynqOverlayOf(t *testing.T, dir string) asynqOverlay {
	t.Helper()
	k := readKustomizationFields(t, dir)
	o := asynqOverlay{dir: dir, namespace: "default"}
	if k.Namespace != "" {
		o.namespace = k.Namespace
	}
	for _, r := range k.Resources {
		target := filepath.Clean(filepath.Join(dir, r))
		switch {
		case target == siteDir:
			o.sites = append(o.sites, baseSiteName)
		case target == storeDir:
			o.stores = append(o.stores, baseStoreName)
		case isSiteOverlay(t, target):
			tk := readKustomizationFields(t, target)
			o.sites = append(o.sites, filepath.Base(target))
			if tk.NameSuffix != "" {
				o.suffixed = true
			}
		case !hasKustomization(target):
			// ファイルの resource（base の manifest）は overlay ではない。
		default:
			child := asynqOverlayOf(t, target)
			o.sites = append(o.sites, child.sites...)
			o.stores = append(o.stores, child.stores...)
			o.suffixed = o.suffixed || child.suffixed
		}
	}
	return o
}

// hasKustomization は dir が kustomization.yaml を持つ（overlay / base）かを返す。
func hasKustomization(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "kustomization.yaml"))
	return err == nil
}

// isSiteOverlay は target が site/ を resources に持つ（サイト 1 組ぶんの中間 overlay）かを返す。
func isSiteOverlay(t *testing.T, target string) bool {
	t.Helper()
	if _, err := os.Stat(filepath.Join(target, "kustomization.yaml")); err != nil {
		return false
	}
	for _, r := range readKustomizationFields(t, target).Resources {
		if filepath.Clean(filepath.Join(target, r)) == siteDir {
			return true
		}
	}
	return false
}

var asynqOverlayDirs = []string{asynqOverlayDir, asynqE2EDir}

// asynqTriggerPatches は overlay の patches のうち ScaledJob に当たるものを、
// target 名 → 置き換えたトリガ列で返す。同じ名前を 2 回当てると落とす。
func asynqTriggerPatches(t *testing.T, dir string) map[string][]map[string]any {
	t.Helper()
	k := loadOverlayPatches(t, dir)
	out := map[string][]map[string]any{}
	for _, p := range k.Patches {
		if p.Target.Kind != "ScaledJob" {
			continue
		}
		var ops []struct {
			Op    string `yaml:"op"`
			Path  string `yaml:"path"`
			Value any    `yaml:"value"`
		}
		if err := yaml.Unmarshal([]byte(p.Patch), &ops); err != nil {
			t.Fatalf("%s: decoding the patch for %s: %v", dir, p.Target.Name, err)
		}
		if len(ops) != 1 || ops[0].Op != "replace" || ops[0].Path != "/spec/triggers" {
			t.Fatalf("%s: patch for %s: want exactly one `replace /spec/triggers`, got %+v", dir, p.Target.Name, ops)
		}
		raw, err := yaml.Marshal(ops[0].Value)
		if err != nil {
			t.Fatal(err)
		}
		var triggers []map[string]any
		if err := yaml.Unmarshal(raw, &triggers); err != nil {
			t.Fatalf("%s: patch for %s: triggers are not a list of maps: %v", dir, p.Target.Name, err)
		}
		if _, dup := out[p.Target.Name]; dup {
			t.Errorf("%s: ScaledJob %s is patched twice", dir, p.Target.Name)
		}
		out[p.Target.Name] = triggers
	}
	return out
}

// expectedAsynqTargets は、overlay で複製された ScaledJob ごとに、名前と物理キュー名を返す。
func expectedAsynqTargets(t *testing.T, o asynqOverlay) map[string]string {
	t.Helper()
	want := map[string]string{}
	for _, w := range scaledJobs(t) {
		logical, ok := flagValue(argsOf(soleContainer(t, w)), "queues")
		if !ok {
			t.Fatalf("%s has no --queues", w.id())
		}
		switch w.dir {
		case baseDir:
			want[w.name()] = logical
		case siteDir:
			for _, site := range o.sites {
				name := w.name()
				if o.suffixed {
					name += "-" + site
				}
				want[name] = logical + "_" + site
			}
		case storeDir:
			for _, store := range o.stores {
				want[w.name()] = logical + "_" + store
			}
		}
	}
	return want
}

// TestAsynqOverlaysRetargetEveryScaledJobExactly は、asynq overlay ごとに
// 「複製された ScaledJob の集合」と「トリガを差し替える patch の集合」が一致し、
// 各 URL が自分の物理キューの件数を、その overlay の notifier（FQDN）から読むことを固定する。
//
// 落ちる典型: ScaledJob を 1 本足して patch を書き忘れる（残った postgresql トリガは
// asynq 構成では river_job を見ない = 永久に 0）。論理名で引く。短い Service 名で書く。
// e2e の nameSuffix と patch の名前がずれる（黙って無視される）。
func TestAsynqOverlaysRetargetEveryScaledJobExactly(t *testing.T) {
	for _, dir := range asynqOverlayDirs {
		o := asynqOverlayOf(t, dir)
		t.Run(o.dir, func(t *testing.T) {
			patched := asynqTriggerPatches(t, o.dir)
			want := expectedAsynqTargets(t, o)
			for name := range want {
				if _, ok := patched[name]; !ok {
					t.Errorf("%s: ScaledJob %s has no trigger patch; it keeps the postgresql trigger, which counts river_job and never sees asynq tasks", o.dir, name)
				}
			}
			for name := range patched {
				if _, ok := want[name]; !ok {
					t.Errorf("%s: patch targets ScaledJob %s, which the overlay does not produce (kustomize ignores it silently)", o.dir, name)
				}
			}
			host := "rokuban-notifier." + o.namespace + ".svc.cluster.local:40773"
			for name, queue := range want {
				triggers, ok := patched[name]
				if !ok {
					continue
				}
				if len(triggers) != 1 {
					t.Errorf("%s: ScaledJob %s: %d triggers, want exactly 1 metrics-api trigger", o.dir, name, len(triggers))
					continue
				}
				tr := triggers[0]
				if tr["type"] != "metrics-api" {
					t.Errorf("%s: ScaledJob %s: trigger type = %v, want metrics-api", o.dir, name, tr["type"])
				}
				md, _ := tr["metadata"].(map[string]any)
				url, _ := md["url"].(string)
				if want := "http://" + host + "/internal/queues/" + queue + "/deliverable-count"; url != want {
					t.Errorf("%s: ScaledJob %s: url = %q, want %q", o.dir, name, url, want)
				}
				if md["valueLocation"] != "value" {
					t.Errorf("%s: ScaledJob %s: valueLocation = %v, want \"value\"", o.dir, name, md["valueLocation"])
				}
				if md["targetValue"] != "1" && md["targetValue"] != 1 {
					t.Errorf("%s: ScaledJob %s: targetValue = %v, want 1", o.dir, name, md["targetValue"])
				}
			}
		})
	}
}

// TestAsynqOverlaysShareOneNotifierHost は、1 つの overlay の全トリガが同じホスト
// （FQDN）を指すことを固定する。直書きが 1 か所でも残ると、その 1 キューだけが
// 黙ってスケールしなくなる。
func TestAsynqOverlaysShareOneNotifierHost(t *testing.T) {
	for _, dir := range asynqOverlayDirs {
		o := asynqOverlayOf(t, dir)
		hosts := map[string]bool{}
		for _, triggers := range asynqTriggerPatches(t, o.dir) {
			for _, tr := range triggers {
				md, _ := tr["metadata"].(map[string]any)
				url, _ := md["url"].(string)
				rest := strings.TrimPrefix(url, "http://")
				host, _, _ := strings.Cut(rest, "/")
				hosts[host] = true
			}
		}
		if len(hosts) != 1 {
			t.Errorf("%s: triggers name %d different notifier hosts %v; want exactly one", o.dir, len(hosts), hosts)
		}
	}
}

// TestAsynqRedisIsReachableByItsServiceName は、Redis の Service が 6379 を指し、
// 永続ボリュームを持たず、永続化を切って起動することを固定する。
func TestAsynqRedisIsReachableByItsServiceName(t *testing.T) {
	objs := readObjects(t, filepath.Join(asynqComponentDir, "redis.yaml"))
	var svc, dep object
	for _, o := range objs {
		switch o.kind() {
		case "Service":
			svc = o
		case "Deployment":
			dep = o
		case "PersistentVolumeClaim":
			t.Errorf("redis.yaml declares PersistentVolumeClaim %s; the asynq queue must not be durable (docs/operations/k8s.md)", o.name())
		}
	}
	if svc.doc == nil || dep.doc == nil {
		t.Fatal("redis.yaml must declare a Service and a Deployment")
	}
	if vols := sliceAt(podTemplate(dep), "spec", "volumes"); len(vols) != 0 {
		t.Errorf("Redis Deployment mounts %d volumes; the asynq queue has no persistent volume", len(vols))
	}
	cs := containers(mapAt(podTemplate(dep), "spec"))
	if len(cs) != 1 {
		t.Fatalf("Redis Deployment has %d containers, want 1", len(cs))
	}
	args := argsOf(cs[0])
	if !argsContainPair(args, "--appendonly", "no") {
		t.Errorf("Redis is not started with --appendonly no (args %v)", args)
	}
	if !argsContainPair(args, "--save", "") {
		t.Errorf("Redis is not started with --save \"\" (args %v)", args)
	}
	ports := sliceAt(svc.doc, "spec", "ports")
	if len(ports) != 1 {
		t.Fatalf("Redis Service has %d ports, want 1", len(ports))
	}
	if p := ports[0].(map[string]any)["port"]; p != 6379 {
		t.Errorf("Redis Service port = %v, want 6379", p)
	}
}

// TestAsynqNotifierDoesNotConsumeQueues は、asynq の component が notifier の
// 常駐 worker を外すことを固定する。base の notifier は River の保守のために
// `worker` ロールと `--queues ruler` を持つ。asynq でそれが残ると、常駐 consumer
// が ruler のタスクを ScaledJob の Job より先に取り、KEDA が起こす先がなくなる
// （実測: 件数は 0 のまま `asynq:{ruler}:processed` だけが進んだ）。
func TestAsynqNotifierDoesNotConsumeQueues(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(asynqComponentDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k overlayPatches
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatal(err)
	}
	var args []string
	found := false
	for _, p := range k.Patches {
		if p.Target.Kind != "Deployment" || p.Target.Name != "rokuban-notifier" {
			continue
		}
		var ops []jsonPatchOp
		if err := yaml.Unmarshal([]byte(p.Patch), &ops); err != nil {
			t.Fatalf("decoding the notifier patch: %v", err)
		}
		if len(ops) != 1 || ops[0].Path != "/spec/template/spec/containers/0/args" {
			t.Fatalf("notifier patch must replace only the container args, got %+v", ops)
		}
		raw, _ := yaml.Marshal(ops[0].Value)
		if err := yaml.Unmarshal(raw, &args); err != nil {
			t.Fatal(err)
		}
		found = true
	}
	if !found {
		t.Fatal("the asynq component does not patch the notifier args (base runs notifier,worker --queues ruler)")
	}
	if !argsContainPair(args, "--roles", "notifier") {
		t.Errorf("asynq notifier --roles = %v, want exactly notifier", args)
	}
	for _, a := range args {
		if a == "--queues" {
			t.Errorf("asynq notifier passes --queues (%v); a resident consumer would take ScaledJob work before the Job starts", args)
		}
		if a == "worker" || strings.Contains(a, "worker") {
			t.Errorf("asynq notifier args %v still include the worker role", args)
		}
	}
}

// TestAsynqConfigDiffersFromRiverOnlyInTheWorkerSection は、asynq の config.yml が
// River 構成の config.yml と worker 節の中しか違わないことを固定する。
// 違う節が紛れると、「River の config を触らない」という前提が静かに崩れる。
func TestAsynqConfigDiffersFromRiverOnlyInTheWorkerSection(t *testing.T) {
	cases := []struct{ asynq, river string }{
		{filepath.Join(asynqOverlayDir, configFileName), filepath.Join(baseDir, configFileName)},
		{filepath.Join(asynqE2EDir, configFileName), filepath.Join(e2eOverlayDir, configFileName)},
	}
	for _, c := range cases {
		a := readFile(t, c.asynq)
		r := readFile(t, c.river)
		if got, want := withoutWorkerSection(a), withoutWorkerSection(r); got != want {
			t.Errorf("%s differs from %s outside the worker section", c.asynq, c.river)
		}
		sec := workerSection(a)
		for _, need := range []string{"engine: asynq", `redis_url: "${REDIS_URL}"`, "periodic_jobs: false"} {
			if !strings.Contains(sec, need) {
				t.Errorf("%s: worker section lacks %q", c.asynq, need)
			}
		}
		if strings.Contains(sec, "rescue_stuck_jobs_after") {
			t.Errorf("%s: rescue_stuck_jobs_after is River-only and the asynq config refuses it at startup", c.asynq)
		}
	}
}

// TestAsynqConfigsLoad は、asynq の config.yml が実際に読めて asynq を選ぶことを固定する。
func TestAsynqConfigsLoad(t *testing.T) {
	for _, path := range []string{filepath.Join(asynqOverlayDir, configFileName), filepath.Join(asynqE2EDir, configFileName)} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("POSTGRES_PASSWORD", "s3cret")
			t.Setenv("REDIS_URL", "redis://rokuban-redis.default.svc.cluster.local:6379/0")
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("config.Load(%s): %v", path, err)
			}
			if cfg.Worker.Engine != config.JobEngineAsynq {
				t.Errorf("worker.engine = %q, want asynq", cfg.Worker.Engine)
			}
		})
	}
}

// TestAsynqEngineReachesEveryConfigPod は、config.yml を読むすべての Pod が
// rokuban-secrets を envFrom で受けることを固定する。asynq の config は `${REDIS_URL}`
// を Secret から受けるので、届かない Pod は起動時に落ちる。
func TestAsynqEngineReachesEveryConfigPod(t *testing.T) {
	checked := 0
	for _, w := range loadWorkloads(t) {
		if !mountsRokubanConfig(w) {
			continue
		}
		checked++
		if !envFromHasSecret(soleContainer(t, w), "rokuban-secrets") {
			t.Errorf("%s (%s) mounts rokuban-config but does not envFrom rokuban-secrets; REDIS_URL would not reach it", w.id(), w.dir)
		}
	}
	if checked == 0 {
		t.Fatal("no workload mounts rokuban-config (nothing would be checked)")
	}
}

// argsContainPair は args に flag と value が隣り合って並ぶかを見る（`--save ""`
// のように値が空文字の引数を含むので、文字列の結合では判定できない）。
func argsContainPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func mountsRokubanConfig(w workload) bool {
	for _, v := range sliceAt(podTemplate(w.object), "spec", "volumes") {
		vm, _ := v.(map[string]any)
		if mapAt(vm, "configMap") != nil && strAt(vm, "configMap", "name") == "rokuban-config" {
			return true
		}
	}
	return false
}

func envFromHasSecret(c map[string]any, name string) bool {
	for _, e := range sliceAt(c, "envFrom") {
		em, _ := e.(map[string]any)
		if strAt(em, "secretRef", "name") == name {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

// workerSection は config.yml の `worker:` 節（次の列 0 のキーの直前まで）を返す。
func workerSection(text string) string {
	var out []string
	inside := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "worker:") {
			inside = true
		} else if inside && l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#") {
			inside = false
		}
		if inside {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// withoutWorkerSection は worker 節を取り除いた残りを返す（節の境目の空行は比べる）。
func withoutWorkerSection(text string) string {
	sec := workerSection(text)
	return strings.Replace(text, sec, "", 1)
}

// readObjects は単一ファイルの多ドキュメント YAML を読む（loadDir と違い、
// kustomization.yaml の resources を経由しない。component の追加ファイル用）。
func readObjects(t *testing.T, path string) []object {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var objs []object
	dec := yaml.NewDecoder(f)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		if len(doc) != 0 {
			objs = append(objs, object{doc: doc})
		}
	}
	return objs
}
