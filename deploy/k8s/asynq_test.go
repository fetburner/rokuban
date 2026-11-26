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

// urlPatches は overlay の patches のうち ScaledJob のトリガ URL（`/spec/triggers/0/metadata/url`）
// を置き換えるものを、target 名 → URL で返す。同じ名前を 2 回当てると落とす。
// 他の ScaledJob patch（argv や pollingInterval）は対象外。
func urlPatches(t *testing.T, dir string) map[string]string {
	t.Helper()
	k := loadOverlayPatches(t, dir)
	out := map[string]string{}
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
		for _, op := range ops {
			if op.Path != "/spec/triggers/0/metadata/url" {
				continue
			}
			if op.Op != "replace" {
				t.Fatalf("%s: patch for %s: url op is %q, want replace", dir, p.Target.Name, op.Op)
			}
			if _, dup := out[p.Target.Name]; dup {
				t.Errorf("%s: ScaledJob %s is patched twice", dir, p.Target.Name)
			}
			out[p.Target.Name], _ = op.Value.(string)
		}
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

// TestE2EOverlayRetargetsEveryScaledJobExactly は、e2e overlay が触る ScaledJob の集合と
// URL を差し替える patch の集合が一致し、各 URL が自分の物理キューの件数を、その overlay の
// notifier（FQDN）から読むことを固定する。
//
// 両構成（River も asynq も）の件数の口は同じなので、e2e-asynq は差し替えを持たず
// この overlay を継承する（TestAsynqOverlaysInheritTheTriggersOfE2E）。
//
// 落ちる典型: ScaledJob を 1 本足して patch を書き忘れる（残った既定ホストの URL は
// 別の namespace の notifier を見て 0 のまま）。論理名で引く。短い Service 名で書く。
// e2e の nameSuffix と patch の名前がずれる（黙って無視される）。
func TestE2EOverlayRetargetsEveryScaledJobExactly(t *testing.T) {
	o := asynqOverlayOf(t, e2eOverlayDir)
	patched := urlPatches(t, o.dir)
	want := expectedAsynqTargets(t, o)
	for name := range want {
		if _, ok := patched[name]; !ok {
			t.Errorf("%s: ScaledJob %s has no trigger url patch; it keeps the default notifier host", o.dir, name)
		}
	}
	for name := range patched {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: patch targets ScaledJob %s, which the overlay does not produce (kustomize ignores it silently)", o.dir, name)
		}
	}
	for name, queue := range want {
		url, ok := patched[name]
		if !ok {
			continue
		}
		if wantURL := "http://rokuban-notifier." + o.namespace + ".svc.cluster.local:40773/internal/queues/" + queue + "/deliverable-count"; url != wantURL {
			t.Errorf("%s: ScaledJob %s: url = %q, want %q", o.dir, name, url, wantURL)
		}
	}
}

// TestAsynqOverlaysInheritTheTriggersOfE2E は、asynq の 2 overlay が ScaledJob の
// トリガを差し替えないことを固定する。overlays/asynq は既定の名前空間の notifier を
// base のまま使い、overlays/e2e-asynq は overlays/e2e の差し替えを継承する。
// asynq 用に URL を書き直す patch が復活すると、件数の口が 2 種類になる。
func TestAsynqOverlaysInheritTheTriggersOfE2E(t *testing.T) {
	for _, dir := range []string{asynqOverlayDir, asynqE2EDir} {
		if got := urlPatches(t, dir); len(got) != 0 {
			t.Errorf("%s: patches the trigger url of %d ScaledJobs; the trigger is shared across engines and must not be patched per engine", dir, len(got))
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

// TestAsynqNotifierKeepsItsResidentRulerWorker は、asynq 構成でも notifier が
// `--roles notifier,worker --queues ruler` の常駐 worker を持つことを固定する。
// PUT の ruler ヒントは常駐が即座に引く（River と同じ経路）。ここを外すと、反映が
// ScaledJob の pollingInterval と Job の起動に縛られ、判定 1.8 が閾値を超える。
// 実測: 外したとき 9〜11 秒、残したとき River と同じ 0〜1 秒。
// asynq の component は notifier の args に触れない（base の形のまま）。
func TestAsynqNotifierKeepsItsResidentRulerWorker(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(asynqComponentDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k overlayPatches
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatal(err)
	}
	for _, p := range k.Patches {
		if p.Target.Kind == "Deployment" && p.Target.Name == "rokuban-notifier" {
			t.Errorf("the asynq component patches the notifier; the resident ruler worker must stay (base args)")
		}
	}
	var notifier []string
	for _, o := range loadBase(t) {
		if o.kind() == "Deployment" && o.name() == "rokuban-notifier" {
			c := soleContainer(t, workload{object: o, dir: baseDir})
			notifier = argsOf(c)
		}
	}
	if notifier == nil {
		t.Fatal("base has no notifier Deployment")
	}
	if !argsContainPair(notifier, "--roles", "notifier,worker") {
		t.Errorf("base notifier --roles = %v, want notifier,worker (the resident worker)", notifier)
	}
	if !argsContainPair(notifier, "--queues", "ruler") {
		t.Errorf("base notifier --queues = %v, want ruler (the resident ruler consumer)", notifier)
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
