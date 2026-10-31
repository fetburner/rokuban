// 置き場（`deploy/k8s/store`）の検査。site 側の TestBaseIsSiteIndependent /
// TestSitePatchesTargetTheSiteName に対応する。
//
// ここで見ているものは、どれも `kustomize build` にも kubeconform にも見えない:
//
//   - base が置き場を知っている（PVC を挿す・置き場名を argv に書く）
//   - 置き場 overlay の patch の path が、置き場名やマウント先ではない何かを指している
//   - 置き場が 2 つの構成で、置き場束縛キューの ScaledJob が置き場ごとに無い、別の
//     置き場の物理キューを数える、別の置き場の root をマウントする
//
// **overlay を kustomize で描画しない**（CI に kustomize が無くても回る）。置き場
// overlay の patch は JSON6902 の replace だけなので、store/ のオブジェクトへ
// 同じ replace を当てて描画の代わりにする（setPointer）。nameSuffix は名前に足す。
// labels と nameReference（PVC 名の追随）は描画しない --- 前者は
// TestStorePatchesTargetTheStoreName が宣言を、後者は TestCRDNameReferencesAreDeclared が
// 宣言を見る。
package k8s

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/fetburner/rokuban/internal/jobs"
)

// storeKustomization は置き場 overlay の kustomization.yaml のうち、検査が読む部分。
type storeKustomization struct {
	Resources  []string     `yaml:"resources"`
	NameSuffix string       `yaml:"nameSuffix"`
	Patches    []patchEntry `yaml:"patches"`
	Labels     []struct {
		Pairs            map[string]string `yaml:"pairs"`
		IncludeSelectors bool              `yaml:"includeSelectors"`
		IncludeTemplates bool              `yaml:"includeTemplates"`
	} `yaml:"labels"`
}

// storeOverlayDirs は overlays/*/stores/* を列挙する。
func storeOverlayDirs(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(overlaysDir, "*", "stores", "*", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("globbing store overlays: %v", err)
	}
	var dirs []string
	for _, m := range matches {
		dirs = append(dirs, filepath.Dir(m))
	}
	return dirs
}

func loadStoreKustomization(t *testing.T, dir string) storeKustomization {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading %s/kustomization.yaml: %v", dir, err)
	}
	var k storeKustomization
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("decoding %s/kustomization.yaml: %v", dir, err)
	}
	return k
}

// patchTargets は patch の target が o に当たるかを返す。
func patchTargets(p patchEntry, o object) bool {
	if p.Target.Kind != "" && o.kind() != p.Target.Kind {
		return false
	}
	if p.Target.Name != "" && o.name() != p.Target.Name {
		return false
	}
	if sel := p.Target.LabelSelector; sel != "" {
		key, value, _ := strings.Cut(sel, "=")
		if fmt.Sprint(mapAt(o.doc, "metadata", "labels")[key]) != value {
			return false
		}
	}
	return true
}

// setPointer は JSON Pointer の指す既存の要素を value に置き換える（replace）。
func setPointer(root any, pointer string, value any) bool {
	segs := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	parent, ok := resolvePointer(root, "/"+strings.Join(segs[:len(segs)-1], "/"))
	if len(segs) == 1 {
		parent, ok = root, true
	}
	if !ok {
		return false
	}
	last := segs[len(segs)-1]
	switch node := parent.(type) {
	case map[string]any:
		if _, exists := node[last]; !exists {
			return false
		}
		node[last] = value
		return true
	case []any:
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i >= len(node) {
			return false
		}
		node[i] = value
		return true
	}
	return false
}

// renderStoreOverlay は置き場 overlay（overlays/*/stores/<置き場>）の出力の代わりに、
// store/ のオブジェクトへ patch の replace を当て、名前に nameSuffix を足して返す。
func renderStoreOverlay(t *testing.T, dir string) []object {
	t.Helper()
	k := loadStoreKustomization(t, dir)
	objs := loadStore(t)
	for _, p := range k.Patches {
		for _, o := range objs {
			if !patchTargets(p, o) {
				continue
			}
			for _, op := range parseJSONPatch(t, p.Patch) {
				if op.Op != "replace" {
					t.Fatalf("%s: patch op %q on %s is not a replace; renderStoreOverlay only understands replace", dir, op.Op, o.id())
				}
				if !setPointer(o.doc, op.Path, op.Value) {
					t.Fatalf("%s: patch path %s does not resolve in %s", dir, op.Path, o.id())
				}
			}
		}
	}
	for _, o := range objs {
		meta := mapAt(o.doc, "metadata")
		meta["name"] = strAt(meta, "name") + k.NameSuffix
	}
	return objs
}

// overlayStoreRoots は overlay の config.yml の `storage.stores` から、FS 置き場の root を返す。
func overlayStoreRoots(t *testing.T, overlayDir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(overlayDir, configFileName))
	if err != nil {
		t.Fatalf("reading %s/%s: %v", overlayDir, configFileName, err)
	}
	var cfg struct {
		Storage struct {
			Stores map[string]struct {
				Kind string `yaml:"kind"`
				Root string `yaml:"root"`
			} `yaml:"stores"`
		} `yaml:"storage"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decoding %s/%s: %v", overlayDir, configFileName, err)
	}
	roots := map[string]string{}
	for name, s := range cfg.Storage.Stores {
		if s.Kind == "fs" {
			roots[name] = s.Root
		}
	}
	return roots
}

// base が置き場を知らないこと。置き場束縛のワークロード（PVC / 録画配信 / 置き場で
// 修飾するキューの ScaledJob / 置き場束縛ジョブの CronJob / adopt）は store/ にあり、
// overlay が置き場の数だけ生やす。
//
// **base が PVC を 1 つでも挿すと、置き場を増やす差分が「base を書き換える」に化ける。**
// base の config.yml は置き場を 1 つだけ、`baseStoreName` で宣言する（`mirakcs:` が
// 1 要素で `default` なのと同じ形。store/ は `main` を書いたまま patch 無しで使える）。
func TestBaseIsStoreIndependent(t *testing.T) {
	for _, o := range loadBase(t) {
		if spec := mapAt(podTemplate(o), "spec"); spec != nil {
			for _, c := range containers(spec) {
				args := argsOf(c)
				if v, ok := flagValue(args, "stores"); ok && v != "" {
					t.Errorf("%s container %q passes --stores %q; store-bound workloads belong in %s/",
						o.id(), strAt(c, "name"), v, storeDir)
				}
				if v, ok := flagValue(args, "store"); ok {
					t.Errorf("%s container %q passes --store %q; store-bound enqueue jobs belong in %s/",
						o.id(), strAt(c, "name"), v, storeDir)
				}
				if slices.Contains(args, "adopt") {
					t.Errorf("%s container %q runs storage adopt; it needs a store's PVC and belongs in %s/",
						o.id(), strAt(c, "name"), storeDir)
				}
				for _, q := range strings.Split(strings.Join(flagValues(args, "queues"), ","), ",") {
					if q != "" && jobs.IsStoreBound(q) {
						t.Errorf("%s subscribes the store-bound queue %q; it belongs in %s/", o.id(), q, storeDir)
					}
				}
			}
			for _, v := range sliceAt(spec, "volumes") {
				vol, _ := v.(map[string]any)
				if mapAt(vol, "persistentVolumeClaim") != nil {
					t.Errorf("%s declares the PVC volume %q; PVCs belong to a store (%s/)", o.id(), strAt(vol, "name"), storeDir)
				}
			}
		}
		if o.kind() == "PersistentVolumeClaim" {
			t.Errorf("%s is a PVC in %s/; a PVC belongs to a store (%s/)", o.id(), baseDir, storeDir)
		}
		if o.kind() == "Ingress" {
			for _, rule := range sliceAt(o.doc, "spec", "rules") {
				for _, p := range sliceAt(mapAt(rule, "http"), "paths") {
					if path := strAt(p, "path"); strings.HasPrefix(path, "/api/media/stores/") {
						t.Errorf("%s has the store route %q; per-store routes are added by the overlay", o.id(), path)
					}
				}
			}
		}
	}

	roots := overlayStoreRoots(t, baseDir)
	if len(roots) != 1 {
		t.Fatalf("%s/%s declares %d fs store(s), want exactly 1; adding a second store is overlay's job", baseDir, configFileName, len(roots))
	}
	if _, ok := roots[baseStoreName]; !ok {
		t.Errorf("%s/%s fs stores = %v, want the single store named %q (deploy/k8s/store binds --stores %s)",
			baseDir, configFileName, roots, baseStoreName, baseStoreName)
	}
}

// flagValues は `--name value` / `--name=value` の全出現の値を返す。
func flagValues(args []string, name string) []string {
	var out []string
	for i, a := range args {
		if a == "--"+name && i+1 < len(args) {
			out = append(out, args[i+1])
		} else if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

// 置き場 overlay の patch が、**実際に置き場名とマウント先の位置を指している**こと。
//
// patch の path は `deploy/k8s/store` の argv の添字と volumeMounts の添字である。
// 並べ替えると overlay は黙って別の値を書き換える（`--soft-stop-timeout` が置き場名に
// なって起動しない、config のマウント先に PVC が刺さる、等）。`kustomize build` は何も
// 言わない。ここで「その位置に今 `main` / `/mnt/media` が居ること」を固定する。
//
// マウント先は、その overlay の config.yml の `storage.stores.<置き場>.root` と一致すること
// （違うと worker は別の root を開き、置き場の目印が無くて FS 操作をすべて見送る）。
// labels は selector とテンプレートに入れること（落とすと置き場が 2 つの構成で 2 つの
// 配信 Deployment の selector が 1 バイトも違わなくなる。site 側と同じ理由）。
func TestStorePatchesTargetTheStoreName(t *testing.T) {
	dirs := storeOverlayDirs(t)
	if len(dirs) == 0 {
		t.Fatalf("no store overlay found under %s/*/stores/*; this test checks nothing", overlaysDir)
	}
	store := loadStore(t)
	for _, dir := range dirs {
		t.Run(dir, func(t *testing.T) {
			name := filepath.Base(dir)
			k := loadStoreKustomization(t, dir)
			if k.NameSuffix != "-"+name {
				t.Fatalf("%s nameSuffix = %q, want %q (objects of two stores would collide)", dir, k.NameSuffix, "-"+name)
			}
			if len(k.Patches) == 0 {
				t.Fatalf("%s has no patches; every object would keep the store name %q", dir, baseStoreName)
			}
			okLabel := false
			for _, l := range k.Labels {
				if l.Pairs["rokuban.fetburner.net/store"] == name && l.IncludeSelectors && l.IncludeTemplates {
					okLabel = true
				}
			}
			if !okLabel {
				t.Errorf("%s does not label rokuban.fetburner.net/store=%s with includeSelectors and includeTemplates", dir, name)
			}
			root := overlayStoreRoots(t, filepath.Dir(filepath.Dir(dir)))[name]
			if root == "" {
				t.Fatalf("%s: the overlay config.yml has no fs store %q with a root", dir, name)
			}

			for _, p := range k.Patches {
				matched := 0
				for _, o := range store {
					if !patchTargets(p, o) {
						continue
					}
					matched++
					for _, op := range parseJSONPatch(t, p.Patch) {
						if op.Op != "replace" {
							t.Errorf("%s: patch op %q on %s is not a replace; this check only understands replace", dir, op.Op, o.id())
							continue
						}
						cur, ok := resolvePointer(o.doc, op.Path)
						if !ok {
							t.Errorf("%s: patch path %s does not resolve in %s", dir, op.Path, o.id())
							continue
						}
						switch {
						case strings.Contains(op.Path, "/args/"):
							if fmt.Sprint(cur) != baseStoreName {
								t.Errorf("%s: patch path %s points at %q in %s, want the store name %q "+
									"(the arguments were reordered; this patch now rewrites something else)",
									dir, op.Path, fmt.Sprint(cur), o.id(), baseStoreName)
							}
							if fmt.Sprint(op.Value) != name {
								t.Errorf("%s: patch writes %q at %s, want %q", dir, fmt.Sprint(op.Value), op.Path, name)
							}
						case strings.HasSuffix(op.Path, "/mountPath"):
							if !strings.Contains(op.Path, "/volumeMounts/1/") {
								t.Errorf("%s: mountPath patch %s is not volumeMounts[1] (media is index 1 in every store workload)", dir, op.Path)
							}
							if fmt.Sprint(cur) != "/mnt/media" {
								t.Errorf("%s: patch path %s points at %q in %s, want the media mount /mnt/media "+
									"(volumeMounts were reordered)", dir, op.Path, fmt.Sprint(cur), o.id())
							}
							if fmt.Sprint(op.Value) != root {
								t.Errorf("%s: patch mounts %s at %q, want the root %q of store %s in the overlay config.yml",
									dir, o.id(), fmt.Sprint(op.Value), root, name)
							}
						case strings.HasSuffix(op.Path, "/metadata/query"):
							want := strings.ReplaceAll(fmt.Sprint(cur), "_"+baseStoreName+"'", "_"+name+"'")
							got := strings.Join(strings.Fields(fmt.Sprint(op.Value)), " ")
							if got != strings.Join(strings.Fields(want), " ") {
								t.Errorf("%s: patched trigger query for %s\n got: %s\nwant: %s", dir, o.id(), got, want)
							}
						default:
							t.Errorf("%s: patch path %s on %s is none of args / mountPath / query; "+
								"extend this check before adding a new kind of patch", dir, op.Path, o.id())
						}
					}
				}
				if matched == 0 {
					t.Errorf("%s: patch target %+v matches no object in %s/; it is silently doing nothing", dir, p.Target, storeDir)
				}
			}
		})
	}
}

// storeBoundQueueLiterals は置き場束縛の論理キュー。**リテラルで書く**
// （jobs.IsStoreBound から導くと、キューを足したときテストも黙って追随する）。
// 下の TestStoreBoundQueueLiteralsMatchJobs が、この表と jobs の集合の一致を固定する。
var storeBoundQueueLiterals = []string{"cm_detect", "encode", "original", "storage", "thumbnail", "transfer", "ts_scan"}

// 表と jobs の置き場束縛キューが同じ集合であること（片方だけ増えたら落ちる）。
func TestStoreBoundQueueLiteralsMatchJobs(t *testing.T) {
	var fromJobs []string
	for _, q := range jobs.AllQueueNames() {
		if jobs.IsStoreBound(q) {
			fromJobs = append(fromJobs, q)
		}
	}
	if !slices.Equal(fromJobs, storeBoundQueueLiterals) {
		t.Errorf("store-bound queues in internal/jobs = %v, this test's literal list = %v", fromJobs, storeBoundQueueLiterals)
	}
}

// e2e ハーネス（判定 6）の置き場束縛キューの一覧と置き場名が、`internal/jobs` と
// 置き場 2 つの overlay の config.yml と同じであること。bash 側の一覧は書き写しなので、
// キューを足したときに判定 6 だけが古い一覧のまま緑になるのを止める。
func TestE2EStoreQueuesMatchJobs(t *testing.T) {
	raw, err := os.ReadFile("e2e/lib/env.sh")
	if err != nil {
		t.Fatalf("reading e2e/lib/env.sh: %v", err)
	}
	var queues, storeA, storeB string
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, `E2E_STORE_QUEUES="`); ok {
			queues = strings.TrimSuffix(v, `"`)
		}
		if v, ok := strings.CutPrefix(line, `E2E_STORE_A="`); ok {
			storeA = strings.TrimSuffix(v, `"`)
		}
		if v, ok := strings.CutPrefix(line, `E2E_STORE_B="`); ok {
			storeB = strings.TrimSuffix(v, `"`)
		}
	}
	var fromJobs []string
	for _, q := range jobs.AllQueueNames() {
		if jobs.IsStoreBound(q) {
			fromJobs = append(fromJobs, q)
		}
	}
	if got := strings.Fields(queues); !slices.Equal(got, fromJobs) {
		t.Errorf("e2e/lib/env.sh E2E_STORE_QUEUES = %v, internal/jobs store-bound queues = %v", got, fromJobs)
	}
	roots := overlayStoreRoots(t, "overlays/e2e-two-stores")
	for _, name := range []string{storeA, storeB} {
		if _, ok := roots[name]; !ok || name == "" {
			t.Errorf("e2e/lib/env.sh store %q is not an fs store in overlays/e2e-two-stores/config.yml (%v)", name, roots)
		}
	}
	if len(roots) != 2 {
		t.Errorf("overlays/e2e-two-stores/config.yml has %d fs stores, e2e/lib/env.sh names 2", len(roots))
	}
}

// 置き場が 2 つの overlay で、置き場束縛キューごと・置き場ごとに ScaledJob が 1 本あり、
// その置き場の物理キュー（リテラル）だけを数え、その置き場の root だけをマウントすること。
// CronJob / `storage adopt` / 配信 Deployment も置き場ごとに 1 組ある。
//
// **置き場の数は overlay の config.yml（`storage.stores`）を権威にする。** config に置き場を
// 足して overlay の Pod セットを足し忘れると、その置き場の転送・エンコード・削除が誰にも
// 消化されない（症状は「ジョブが積まれたまま」）。
func TestTwoStoreOverlayHasAScaledJobPerStoreAndQueue(t *testing.T) {
	const overlayDir = "overlays/e2e-two-stores"
	wantPhysical := map[string][]string{
		"takamatsu": {"cm_detect_takamatsu", "encode_takamatsu", "original_takamatsu", "storage_takamatsu", "thumbnail_takamatsu", "transfer_takamatsu", "ts_scan_takamatsu"},
		"tokyo":     {"cm_detect_tokyo", "encode_tokyo", "original_tokyo", "storage_tokyo", "thumbnail_tokyo", "transfer_tokyo", "ts_scan_tokyo"},
	}

	roots := overlayStoreRoots(t, overlayDir)
	var configured []string
	for name := range roots {
		configured = append(configured, name)
	}
	slices.Sort(configured)
	if !slices.Equal(configured, []string{"takamatsu", "tokyo"}) {
		t.Fatalf("%s/config.yml fs stores = %v, want [takamatsu tokyo]", overlayDir, configured)
	}
	if roots["takamatsu"] == roots["tokyo"] || roots["takamatsu"] == "" || roots["tokyo"] == "" {
		t.Errorf("%s/config.yml store roots = %v, want two distinct non-empty roots (a process mounting both refuses equal roots)", overlayDir, roots)
	}

	k := loadOverlayPatches(t, overlayDir)
	var overlayStores []string
	for _, r := range k.Resources {
		if name, ok := strings.CutPrefix(r, "stores/"); ok {
			overlayStores = append(overlayStores, name)
		}
	}
	slices.Sort(overlayStores)
	if !slices.Equal(overlayStores, configured) {
		t.Fatalf("%s resources stores/* = %v, config.yml stores = %v; each configured store needs one Pod set", overlayDir, overlayStores, configured)
	}

	physicalSeen := map[string]string{} // 物理キュー -> ScaledJob 名
	for _, name := range overlayStores {
		objs := renderStoreOverlay(t, filepath.Join(overlayDir, "stores", name))
		byID := map[string]object{}
		for _, o := range objs {
			byID[o.id()] = o
		}

		var gotPhysical []string
		for _, o := range objs {
			if o.kind() != "ScaledJob" {
				continue
			}
			w := workload{object: o, dir: storeDir}
			c := soleContainer(t, w)
			args := argsOf(c)
			q, _ := flagValue(args, "queues")
			gotStores, _ := flagValue(args, "stores")
			if gotStores != name {
				t.Errorf("%s (store %s) passes --stores=%q, want %q", o.id(), name, gotStores, name)
			}
			query := triggerQuery(t, w)
			wantQuery := fmt.Sprintf("SELECT count(*) FROM river_job WHERE queue = '%s_%s' AND state IN ('available','retryable')", q, name) //nolint:gocritic // KEDA の scaler が読む文字列の照合
			if query != wantQuery {
				t.Errorf("%s trigger query\n got: %s\nwant: %s", o.id(), query, wantQuery)
			}
			gotPhysical = append(gotPhysical, q+"_"+name)
			if prev, dup := physicalSeen[q+"_"+name]; dup {
				t.Errorf("physical queue %s is counted by both %s and %s; KEDA would double-count the backlog", q+"_"+name, prev, o.id())
			}
			physicalSeen[q+"_"+name] = o.name()

			mounts := sliceAt(c, "volumeMounts")
			if len(mounts) < 2 || strAt(mounts[1], "name") != "media" || strAt(mounts[1], "mountPath") != roots[name] {
				t.Errorf("%s volumeMounts[1] = %v, want the media mount at %q (the root of store %s)", o.id(), mounts, roots[name], name)
			}
		}
		slices.Sort(gotPhysical)
		if !slices.Equal(gotPhysical, wantPhysical[name]) {
			t.Errorf("store %s ScaledJobs count the physical queues %v, want %v", name, gotPhysical, wantPhysical[name])
		}

		// CronJob: 置き場束縛の 3 ジョブが、その置き場を `--store` で指す。
		wantCron := map[string]string{
			"CronJob/rokuban-enqueue-catalog-export-" + name:   "catalog-export",
			"CronJob/rokuban-enqueue-delete-reconcile-" + name: "delete-reconcile",
			"CronJob/rokuban-enqueue-storage-sync-" + name:     "storage-sync",
		}
		for id, job := range wantCron {
			o, ok := byID[id]
			if !ok {
				t.Errorf("store %s has no %s", name, id)
				continue
			}
			args := argsOf(soleContainer(t, workload{object: o, dir: storeDir}))
			if len(args) < 2 || args[0] != "enqueue" || args[1] != job {
				t.Errorf("%s argv = %v, want `enqueue %s ...`", id, args, job)
			}
			if got, _ := flagValue(args, "store"); got != name {
				t.Errorf("%s passes --store=%q, want %q", id, got, name)
			}
		}

		// 配信 Deployment と Service: 1 置き場だけを配り、その root だけをマウントする。
		dep, ok := byID["Deployment/rokuban-recording-hls-"+name]
		if !ok {
			t.Fatalf("store %s has no recording HLS Deployment", name)
		}
		if _, ok := byID["Service/rokuban-recording-hls-"+name]; !ok {
			t.Errorf("store %s has no recording HLS Service", name)
		}
		hls := soleContainer(t, workload{object: dep, dir: storeDir})
		hlsArgs := argsOf(hls)
		if got := flagValues(hlsArgs, "stores"); !slices.Equal(got, []string{name}) {
			t.Errorf("%s --stores = %v, want exactly [%s] (recording HLS serves one store per process)", dep.id(), got, name)
		}
		if slices.Contains(hlsArgs, "--no-recording-hls") {
			t.Errorf("%s opts out of the recording HLS it exists to serve", dep.id())
		}
		if mounts := sliceAt(hls, "volumeMounts"); len(mounts) < 2 || strAt(mounts[1], "mountPath") != roots[name] {
			t.Errorf("%s volumeMounts[1] = %v, want the media mount at %q", dep.id(), mounts, roots[name])
		}

		// `storage adopt` はその置き場の名前を取り、その root に目印を書く。
		adopt, ok := byID["Job/rokuban-storage-adopt-"+name]
		if !ok {
			t.Fatalf("store %s has no storage adopt Job", name)
		}
		adoptC := soleContainer(t, workload{object: adopt, dir: storeDir})
		adoptArgs := argsOf(adoptC)
		if len(adoptArgs) < 3 || strings.Join(adoptArgs[:3], " ") != "storage adopt "+name {
			t.Errorf("%s argv = %v, want `storage adopt %s` first", adopt.id(), adoptArgs, name)
		}
		if mounts := sliceAt(adoptC, "volumeMounts"); len(mounts) < 2 || strAt(mounts[1], "mountPath") != roots[name] {
			t.Errorf("%s volumeMounts[1] = %v, want the media mount at %q", adopt.id(), mounts, roots[name])
		}
		if _, ok := byID["PersistentVolumeClaim/rokuban-media-"+name]; !ok {
			t.Errorf("store %s has no PVC rokuban-media-%s", name, name)
		}
	}
	if len(physicalSeen) != len(storeBoundQueueLiterals)*len(overlayStores) {
		t.Errorf("%d physical queues counted, want %d (7 store-bound queues x %d stores)",
			len(physicalSeen), len(storeBoundQueueLiterals)*len(overlayStores), len(overlayStores))
	}
}

// 置き場が 2 つの overlay の Ingress が、`/api/media/stores/<置き場>` を**その置き場の**
// 配信 Service へ送ること。Service 名は置き場 overlay の描画結果（nameSuffix 後）と突き合わせる。
// TestIngressOverlaysAddConcreteLiveRoutes は文字列を固定するだけで、Service が実在する
// 名前かは見ない。
func TestStoreIngressRowsPointAtTheStoreService(t *testing.T) {
	const overlayDir = "overlays/e2e-two-stores"
	services := map[string]string{} // 置き場 -> 配信 Service の名前
	for _, dir := range storeOverlayDirs(t) {
		if !strings.HasPrefix(dir, overlayDir+string(filepath.Separator)) {
			continue
		}
		for _, o := range renderStoreOverlay(t, dir) {
			if o.kind() == "Service" && strAt(o.doc, "spec", "selector", "app.kubernetes.io/component") == "recording-hls" {
				services[filepath.Base(dir)] = o.name()
			}
		}
	}
	if len(services) != 2 {
		t.Fatalf("recording HLS Services per store = %v, want 2", services)
	}

	k := loadOverlayPatches(t, overlayDir)
	rows := map[string]string{}
	for _, p := range k.Patches {
		if p.Target.Kind != "Ingress" {
			continue
		}
		for _, op := range parseJSONPatch(t, p.Patch) {
			v, _ := op.Value.(map[string]any)
			if path := strAt(v, "path"); strings.HasPrefix(path, "/api/media/stores/") {
				rows[strings.TrimPrefix(path, "/api/media/stores/")] = strAt(v, "backend", "service", "name")
			}
		}
	}
	for name, svc := range services {
		if got := rows[name]; got != svc {
			t.Errorf("Ingress /api/media/stores/%s routes to %q, want the store's own Service %q", name, got, svc)
		}
	}
	if len(rows) != len(services) {
		t.Errorf("Ingress has %d store rows (%v), want %d", len(rows), rows, len(services))
	}
}

// 置き場が 2 つの overlay で、ingest（site 束縛）の ScaledJob が**すべての置き場**を
// root にマウントし、`--stores` に全部を渡すこと。
//
// ingest は site ごとの 1 本で、原本の置き場は規則（`storage.originals`）が録画ごとに
// 決める。規則が選びうる置き場をマウントしていないと、その録画の ingest は置き場が
// 無くて失敗する（症状は特定の site だけ ingest が止まる）。**mirakc の録画を通した実測は
// していない**（この検査は patch の宣言を見るだけ）。
func TestTwoStoreOverlayIngestMountsEveryStore(t *testing.T) {
	const overlayDir = "overlays/e2e-two-stores"
	roots := overlayStoreRoots(t, overlayDir)
	wantStores := []string{"takamatsu", "tokyo"}
	wantMounts := []string{roots["takamatsu"], roots["tokyo"]}
	slices.Sort(wantMounts)

	// site/ の ingest の argv で `--stores` の値がある位置。patch の index 11 がそこを指す。
	var ingest object
	for _, o := range loadSite(t) {
		if o.id() == "ScaledJob/rokuban-worker-ingest" {
			ingest = o
		}
	}
	if ingest.kind() == "" {
		t.Fatal("ScaledJob/rokuban-worker-ingest not found in site/")
	}
	const storesValuePath = "/spec/jobTargetRef/template/spec/containers/0/args/11"
	if cur, ok := resolvePointer(ingest.doc, storesValuePath); !ok || fmt.Sprint(cur) != baseStoreName {
		t.Fatalf("site/ ingest %s = %v (resolved=%v), want %q (the arguments were reordered)", storesValuePath, cur, ok, baseStoreName)
	}

	k := loadOverlayPatches(t, overlayDir)
	for _, site := range []string{"sitea", "siteb"} {
		var mounts []string
		var stores string
		found := false
		for _, p := range k.Patches {
			if p.Target.Kind != "ScaledJob" || p.Target.Name != "rokuban-worker-ingest-"+site {
				continue
			}
			found = true
			for _, op := range parseJSONPatch(t, p.Patch) {
				v, _ := op.Value.(map[string]any)
				switch {
				case op.Path == storesValuePath:
					stores = fmt.Sprint(op.Value)
				case strings.HasSuffix(op.Path, "/volumeMounts/1/mountPath"):
					mounts = append(mounts, fmt.Sprint(op.Value))
				case strings.HasSuffix(op.Path, "/volumeMounts/-"):
					mounts = append(mounts, strAt(v, "mountPath"))
				}
			}
		}
		if !found {
			t.Errorf("%s does not patch ScaledJob/rokuban-worker-ingest-%s; it would mount only %q", overlayDir, site, baseStoreName)
			continue
		}
		got := strings.Split(stores, ",")
		slices.Sort(got)
		if !slices.Equal(got, wantStores) {
			t.Errorf("ingest-%s --stores = %q, want every configured store %v", site, stores, wantStores)
		}
		slices.Sort(mounts)
		if !slices.Equal(mounts, wantMounts) {
			t.Errorf("ingest-%s mounts %v, want the root of every store %v", site, mounts, wantMounts)
		}
	}
}
