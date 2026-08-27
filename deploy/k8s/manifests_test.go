// Package k8s はテストだけを持つ。`deploy/k8s/base/` のマニフェストが「クラスタに
// 載せる前に分かる形で壊れていないか」を `go test ./...` で検査するためのもの。
//
// **`kustomize build` と kubeconform では足りない**（CI の manifests ジョブ）。
// 前者は YAML として組めるかを見るだけで、後者は 1 つのオブジェクトが k8s の
// スキーマに合うかを見るだけ。したがって次はどれもクラスタに載せるまで分からない
// （すべて実測で両者が緑になることを確認した）:
//
//   - 存在しない ConfigMap 名を envFrom / volume に書く（Pod は
//     CreateContainerConfigError で上がらない）
//   - Service の selector がどの Pod にも当たらない（Endpoints が空のまま
//     「正常」に存在する）
//   - ConfigMap に入れる config.yml のキーを typo する（`internal/config` は
//     strict なので全 Pod が CrashLoopBackOff）
//   - config.yml が参照する `${VAR}` を誰も供給していない（required 検証で落ちる）
//
// オブジェクト間の参照と config.yml の中身はここで見る。
package k8s

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/fetburner/rokuban/internal/config"
)

const (
	baseDir        = "base"
	overlaysDir    = "overlays"
	configFileName = "config.yml"
	apiDeployment  = "Deployment/rokuban-api"
)

// object は 1 つの k8s オブジェクト（YAML ドキュメント 1 つ）。
type object struct {
	doc map[string]any
}

func (o object) kind() string { return strAt(o.doc, "kind") }
func (o object) name() string { return strAt(o.doc, "metadata", "name") }
func (o object) id() string   { return o.kind() + "/" + o.name() }

// generator は kustomization.yaml の configMapGenerator / secretGenerator 1 件。
type generator struct {
	Name     string           `yaml:"name"`
	Files    []string         `yaml:"files"`
	Literals []string         `yaml:"literals"`
	Envs     []string         `yaml:"envs"`
	Options  generatorOptions `yaml:"options"`
}

type generatorOptions struct {
	DisableNameSuffixHash bool `yaml:"disableNameSuffixHash"`
}

type image struct {
	Name    string `yaml:"name"`
	NewName string `yaml:"newName"`
	NewTag  string `yaml:"newTag"`
}

type kustomization struct {
	Resources             []string         `yaml:"resources"`
	ConfigMapGenerator    []generator      `yaml:"configMapGenerator"`
	SecretGenerator       []generator      `yaml:"secretGenerator"`
	GeneratorOptions      generatorOptions `yaml:"generatorOptions"`
	Images                []image          `yaml:"images"`
	Patches               []any            `yaml:"patches"`
	PatchesStrategicMerge []string         `yaml:"patchesStrategicMerge"`
}

// externalSecrets は base が出荷せず、運用者が外で作る（または overlay の
// generator が作る）Secret と、その「形」を書いた参考ファイルの対応。
//
// **ここに足すのは「base に置かない」と決めたときだけ。** 参照解決の検査
// （TestManifestReferencesResolve）はこの表を見て、置き忘れと外部供給を区別する。
// キー名の権威は参考ファイル側なので、api.yaml と参考ファイルの間で名前がずれれば
// 供給の検査（TestConfigVarsAreSupplied）が落ちる。
var externalSecrets = map[string]string{
	"rokuban-secrets": "secret.example.yaml",
}

func loadKustomization(t *testing.T) kustomization {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(baseDir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading kustomization.yaml: %v", err)
	}
	var k kustomization
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("decoding kustomization.yaml: %v", err)
	}
	if len(k.Resources) == 0 {
		t.Fatal("kustomization.yaml lists no resources")
	}
	return k
}

// loadBase は kustomization.yaml の resources に挙がっているファイルを読む。
//
// **ディレクトリを走査して拡張子で拾う形にはしない。** 走査側だけが知っている
// ファイル（`.yml` で置いた / resources に書き忘れた）を検査対象に入れると、
// 「テストは見ているが kustomize は出力していない」というズレが起きる。
// resources を権威にして、ファイルの取りこぼしは
// TestKustomizationCoversEveryFile が別に見る。
func loadBase(t *testing.T) []object {
	t.Helper()
	var objs []object
	for _, name := range loadKustomization(t).Resources {
		f, err := os.Open(filepath.Join(baseDir, name))
		if err != nil {
			t.Fatalf("opening %s (listed in kustomization.yaml): %v", name, err)
		}
		dec := yaml.NewDecoder(f)
		for {
			var doc map[string]any
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("decoding %s: %v", name, err)
			}
			if len(doc) == 0 {
				continue
			}
			objs = append(objs, object{doc: doc})
		}
		_ = f.Close()
	}
	if len(objs) == 0 {
		t.Fatal("no manifests found via kustomization.yaml resources")
	}
	return objs
}

// dataKeys は ConfigMap / Secret（マニフェスト直書きと generator の両方）が
// 持つキー集合を id ごとに返す。
func dataKeys(t *testing.T, objs []object) map[string]map[string]bool {
	t.Helper()
	keysOf := map[string]map[string]bool{}
	for _, o := range objs {
		if o.kind() != "ConfigMap" && o.kind() != "Secret" {
			continue
		}
		keys := map[string]bool{}
		for _, field := range []string{"data", "stringData", "binaryData"} {
			for k := range mapAt(o.doc, field) {
				keys[k] = true
			}
		}
		keysOf[o.id()] = keys
	}

	// base が出荷しない Secret のキーは、参考ファイル（apply されない）から読む。
	for name, example := range externalSecrets {
		raw, err := os.ReadFile(filepath.Join(baseDir, example))
		if err != nil {
			t.Fatalf("reading %s (externalSecrets[%q]): %v", example, name, err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decoding %s: %v", example, err)
		}
		keys := map[string]bool{}
		for _, field := range []string{"data", "stringData"} {
			for k := range mapAt(doc, field) {
				keys[k] = true
			}
		}
		keysOf["Secret/"+name] = keys
	}

	k := loadKustomization(t)
	add := func(kind string, gens []generator) {
		for _, g := range gens {
			keys := map[string]bool{}
			for _, f := range g.Files {
				// `key=path` 形式も許されるので key 側だけ取る。
				key := f
				if i := strings.Index(f, "="); i >= 0 {
					key = f[:i]
				}
				keys[filepath.Base(key)] = true
			}
			for _, l := range g.Literals {
				if i := strings.Index(l, "="); i > 0 {
					keys[l[:i]] = true
				}
			}
			// `envs:`（KEY=VALUE のファイル）。手元にしか置かない運用もあるので、
			// 無ければ黙って飛ばす --- 供給の検査はそのキーを「未供給」と見る
			// （fail-closed）。
			for _, e := range g.Envs {
				raw, err := os.ReadFile(filepath.Join(baseDir, e))
				if err != nil {
					continue
				}
				for _, line := range strings.Split(string(raw), "\n") {
					if i := strings.Index(line, "="); i > 0 {
						keys[strings.TrimSpace(line[:i])] = true
					}
				}
			}
			keysOf[kind+"/"+g.Name] = keys
		}
	}
	add("ConfigMap", k.ConfigMapGenerator)
	add("Secret", k.SecretGenerator)
	return keysOf
}

// podTemplate は Pod テンプレート（`spec.template` 相当）を返す。無ければ nil。
//
// **ここに無い kind のワークロードは、このファイルの検査すべてを素通りする。**
// 実測: KEDA の `ScaledJob`（テンプレートは `spec.jobTargetRef.template`）を
// 存在しない Secret の envFrom・宣言していない volume の mount・securityContext
// なしの全部盛りで足しても、全テストが緑のまま通った（参照件数の下限も api +
// migrate だけで満たされるので鳴らない）。M4-6c で足すのがまさに ScaledJob 群
// なので、取りこぼしは TestEveryWorkloadIsInspected が落とす。
func podTemplate(o object) map[string]any {
	switch o.kind() {
	case "Deployment", "Job", "StatefulSet", "DaemonSet", "ReplicaSet":
		return mapAt(o.doc, "spec", "template")
	case "CronJob":
		return mapAt(o.doc, "spec", "jobTemplate", "spec", "template")
	case "ScaledJob": // KEDA
		return mapAt(o.doc, "spec", "jobTargetRef", "template")
	default:
		return nil
	}
}

// findPodTemplates は `template.spec.containers` を持つ入れ物を再帰的に探す。
// podTemplate の switch から漏れた kind を見つけるためだけに使う。
func findPodTemplates(v any) []map[string]any {
	var out []map[string]any
	switch t := v.(type) {
	case map[string]any:
		if tmpl, ok := t["template"].(map[string]any); ok {
			if spec, ok := tmpl["spec"].(map[string]any); ok {
				if _, ok := spec["containers"]; ok {
					out = append(out, tmpl)
				}
			}
		}
		for _, e := range t {
			out = append(out, findPodTemplates(e)...)
		}
	case []any:
		for _, e := range t {
			out = append(out, findPodTemplates(e)...)
		}
	}
	return out
}

// Pod テンプレートを持つオブジェクトが、すべて podTemplate に拾われていること。
//
// 拾われていない kind は参照解決・名前解決・締め方・`${VAR}` の供給の**全部**を
// 素通りする（実測）。**新しい kind を足す人に必要なのは、テストが落ちて
// podTemplate に 1 行足すこと**であって、静かに検査対象ゼロで緑になることではない。
func TestEveryWorkloadIsInspected(t *testing.T) {
	for _, o := range loadBase(t) {
		found := findPodTemplates(o.doc)
		if len(found) == 0 {
			continue
		}
		if podTemplate(o) == nil {
			t.Errorf("%s has a pod template that podTemplate() does not reach; add its kind there "+
				"(every check in this file would silently skip this workload)", o.id())
		}
	}
}

// podSpecs は各オブジェクトの Pod テンプレートの spec を返す。
func podSpecs(objs []object) map[string]map[string]any {
	specs := make(map[string]map[string]any)
	for _, o := range objs {
		if spec := mapAt(podTemplate(o), "spec"); spec != nil {
			specs[o.id()] = spec
		}
	}
	return specs
}

func containers(podSpec map[string]any) []map[string]any {
	var out []map[string]any
	for _, key := range []string{"initContainers", "containers"} {
		for _, c := range sliceAt(podSpec, key) {
			if m, ok := c.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// container は Pod テンプレート内の名前付きコンテナを返す。無ければテストを失敗
// させる（「該当コンテナが無いのでループが 0 回まわって PASS」を防ぐ。実際に
// この形の空虚な成功を踏んだ）。
func container(t *testing.T, specs map[string]map[string]any, workload, name string) map[string]any {
	t.Helper()
	spec, ok := specs[workload]
	if !ok {
		t.Fatalf("%s not found in %s/", workload, baseDir)
	}
	for _, c := range containers(spec) {
		if strAt(c, "name") == name {
			return c
		}
	}
	t.Fatalf("container %q not found in %s", name, workload)
	return nil
}

// ConfigMap / Secret への参照（envFrom / env.valueFrom / volumes）が、
// base に実在するオブジェクト（generator 由来を含む）を指していること。
func TestManifestReferencesResolve(t *testing.T) {
	objs := loadBase(t)
	keysOf := dataKeys(t, objs)

	// 検査した参照の数。**下限を主張する。** ループの入口が空（`spec.template` の
	// typo など）だと検査 0 件で PASS してしまう（実測でこの穴を踏んだ）。
	checked := 0
	check := func(where, kind, name, key string) {
		checked++
		id := kind + "/" + name
		keys, ok := keysOf[id]
		if !ok {
			t.Errorf("%s references %s, which no manifest or generator in %s/ defines", where, id, baseDir)
			return
		}
		if key != "" && !keys[key] {
			t.Errorf("%s references key %q of %s, which it does not define", where, key, id)
		}
	}

	for id, podSpec := range podSpecs(objs) {
		volumeNames := map[string]bool{}
		for _, v := range sliceAt(podSpec, "volumes") {
			vol, ok := v.(map[string]any)
			if !ok {
				continue
			}
			volumeNames[strAt(vol, "name")] = true
			where := fmt.Sprintf("%s volume %q", id, strAt(vol, "name"))
			if cm := mapAt(vol, "configMap"); cm != nil {
				check(where, "ConfigMap", strAt(cm, "name"), "")
				for _, it := range sliceAt(cm, "items") {
					if item, ok := it.(map[string]any); ok {
						check(where, "ConfigMap", strAt(cm, "name"), strAt(item, "key"))
					}
				}
			}
			if s := mapAt(vol, "secret"); s != nil {
				check(where, "Secret", strAt(s, "secretName"), "")
			}
		}

		for _, c := range containers(podSpec) {
			where := fmt.Sprintf("%s container %q", id, strAt(c, "name"))
			for _, ef := range sliceAt(c, "envFrom") {
				src, ok := ef.(map[string]any)
				if !ok {
					continue
				}
				if r := mapAt(src, "configMapRef"); r != nil {
					check(where+" envFrom", "ConfigMap", strAt(r, "name"), "")
				}
				if r := mapAt(src, "secretRef"); r != nil {
					check(where+" envFrom", "Secret", strAt(r, "name"), "")
				}
			}
			for _, e := range sliceAt(c, "env") {
				env, ok := e.(map[string]any)
				if !ok {
					continue
				}
				from := mapAt(env, "valueFrom")
				if r := mapAt(from, "configMapKeyRef"); r != nil {
					check(where+" env "+strAt(env, "name"), "ConfigMap", strAt(r, "name"), strAt(r, "key"))
				}
				if r := mapAt(from, "secretKeyRef"); r != nil {
					check(where+" env "+strAt(env, "name"), "Secret", strAt(r, "name"), strAt(r, "key"))
				}
			}
			for _, vm := range sliceAt(c, "volumeMounts") {
				m, ok := vm.(map[string]any)
				if !ok {
					continue
				}
				if n := strAt(m, "name"); !volumeNames[n] {
					t.Errorf("%s mounts volume %q, which the pod spec does not declare", where, n)
				}
			}
		}
	}

	// 現状 4 件（api / migrate それぞれの config volume と envFrom）。
	// ワークロードを足すなら増える一方なので、下回ったら検査が空回りしている。
	const wantAtLeast = 4
	if checked < wantAtLeast {
		t.Errorf("only %d references were checked, want >= %d (a reference was removed, or the traversal is not reaching the pod specs)",
			checked, wantAtLeast)
	}
}

// ConfigMap に入れる config.yml が `internal/config` で実際にロードできること。
//
// **kubeconform は ConfigMap の中身を不透明な文字列として扱う。** config.yml の
// キーを 1 文字 typo すると、strict モードの `config.Load` が `unknown field` で
// 落ちて **api も migrate も全 Pod が CrashLoopBackOff** になるが、
// `kustomize build` / kubeconform / 他のテストはどれも緑のままになる（実測）。
//
// パスワードに YAML の構文（`*abc` / `a: b`）が来るケースも一緒に見る。展開は
// パースの前に走るので、参照側のクォートが落ちると Secret の中身次第で
// 全 Pod が起動しなくなる。
func TestConfigMapConfigLoads(t *testing.T) {
	path := filepath.Join(baseDir, configFileName)
	// カナリアだけを置く。**どの囲み方がどの値に耐えるかの一般論は
	// `internal/config` 側のテスト**（TestSecretExpansionQuotingForms）にある ---
	// パーサの意味論なので、赤が出たときそちらに帰属できる方がよい。ここでは
	// 「この config.yml がその形で書かれている」ことだけを見る。
	for _, password := range []string{"s3cret", "a: b", "pa'ss", `pa"ss`, "*abc"} {
		t.Run(password, func(t *testing.T) {
			t.Setenv("POSTGRES_PASSWORD", password)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("config.Load(%s) with POSTGRES_PASSWORD=%q: %v", path, password, err)
			}
			if cfg.DB.Password != password {
				t.Errorf("db.password = %q, want %q", cfg.DB.Password, password)
			}
		})
	}
}

// **前後の空白は落ちる。** 折り畳みブロックスカラーの仕様で、これがこの形の
// 唯一の制約である（引用符で囲む形の「特定の引用符で全 Pod が起動しない」との
// トレードオフでこちらを選んだ）。制約が制約のままであることを固定する ---
// 黙って別の形に変わると、空白入りパスワードで DB 認証だけが失敗する。
func TestConfigMapPasswordWhitespaceIsTrimmed(t *testing.T) {
	path := filepath.Join(baseDir, configFileName)
	for _, password := range []string{" leading", "trailing "} {
		t.Setenv("POSTGRES_PASSWORD", password)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("config.Load with POSTGRES_PASSWORD=%q: %v", password, err)
		}
		if cfg.DB.Password != strings.TrimSpace(password) {
			t.Errorf("db.password = %q, want %q (前後の空白が落ちる形のまま)",
				cfg.DB.Password, strings.TrimSpace(password))
		}
	}
}

// **改行を含むパスワードは値として運べない。** 展開は YAML パースの前の生テキスト
// 置換なので、値の中の改行がブロックスカラーを終端する。実測（実バイナリ）:
//   - `abc\ndef` → `parsing config: [56:1] non-map value is specified`
//   - `abc\nlive:\n  enabled: true` → **live 節として読まれる**（config.yml に
//     無い既知のトップレベルキーは strict でも弾かれない）
//
// どちらも「パスワードとして読まれない」= 運べないので、そこを固定する。
// 構造的な解決（password を YAML に展開せず env から直接読む）は internal/config
// 側の判断なのでこの差分の外。
func TestConfigMapPasswordWithNewlineIsRejected(t *testing.T) {
	path := filepath.Join(baseDir, configFileName)
	for _, password := range []string{"abc\ndef", "abc\nlive:\n  enabled: true"} {
		t.Run(strings.ReplaceAll(password, "\n", "\\n"), func(t *testing.T) {
			t.Setenv("POSTGRES_PASSWORD", password)
			cfg, err := config.Load(path)
			if err != nil {
				return // パースで落ちる = 運べないことが分かる
			}
			if cfg.DB.Password == password {
				t.Errorf("db.password = %q: 改行入りの値がそのまま通ってしまった（制約の記述が古い）", cfg.DB.Password)
			}
		})
	}
}

// base の `allowed_hosts` が空でないこと。
//
// アプリ内に認証は無いので、Host 検証は唯一の防壁である（docs/api.md §認証）。
// **base を空で出荷すると、Ingress を足す overlay 側でここを patch する義理が
// 無いまま外に露出する。** 起動時 WARN は出るがログ 1 行では止まらない。
// 締めるのは base、緩めるのは overlay の向きにしておく。
func TestConfigAllowedHostsIsNotEmpty(t *testing.T) {
	t.Setenv("POSTGRES_PASSWORD", "s3cret")
	cfg, err := config.Load(filepath.Join(baseDir, configFileName))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.Server.AllowedHosts) == 0 {
		t.Error("server.allowed_hosts is empty; DNS rebinding protection would be off in the reference deployment")
	}
}

// パスワードが空（base の Secret のプレースホルダそのまま）なら、config のロードが
// 失敗すること = 起動時に fail-fast すること。
//
// **プレースホルダを `changeme` のような非空の値にすると、この fail-fast が消える**
// ---「Pod は起動し、DB 認証に失敗し、`/readyz` が 503 を返し続ける」という、
// ログを掘るまで原因が分からない壊れ方になる。空であることに意味があるので、
// Secret のプレースホルダと対にして押さえる。
func TestConfigMapConfigFailsWithoutPassword(t *testing.T) {
	t.Setenv("POSTGRES_PASSWORD", "")
	if _, err := config.Load(filepath.Join(baseDir, configFileName)); err == nil {
		t.Fatal("config.Load succeeded with an empty POSTGRES_PASSWORD, want a required-field error")
	}
}

// overlay が自前の config.yml を持つ場合、それも base と同じ主張の対象にする。
//
// **overlay の config.yml は `kustomize build` にも kubeconform にも見えない**
// （ConfigMap の中身は不透明な文字列として運ばれる）。上の config 系テストは
// どれも baseDir 決め打ちだったので、`behavior: replace` で base を丸ごと
// 置き換える overlay は**どの検査も通らないまま**だった。そこを 1 文字 typo
// すると strict な config.Load が unknown field で落ち、api も migration Job も
// 全 Pod が CrashLoopBackOff になる --- このファイル冒頭が「クラスタに載せる
// まで分からないから Go テストで見る」と書いている壊れ方そのもの。
func TestOverlayConfigsLoad(t *testing.T) {
	entries, err := os.ReadDir(overlaysDir)
	if err != nil {
		t.Fatalf("reading %s: %v", overlaysDir, err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(overlaysDir, e.Name(), configFileName)
		if _, err := os.Stat(path); err != nil {
			// config.yml を持たない overlay（base のものをそのまま使う）は対象外。
			continue
		}
		checked++
		t.Run(e.Name(), func(t *testing.T) {
			t.Setenv("POSTGRES_PASSWORD", "s3cret")
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("config.Load(%s): %v", path, err)
			}
			if cfg.DB.Password != "s3cret" {
				t.Errorf("db.password = %q, want the expanded secret", cfg.DB.Password)
			}
			if len(cfg.Server.AllowedHosts) == 0 {
				t.Errorf("%s: server.allowed_hosts is empty; DNS rebinding protection would be off", path)
			}

			t.Setenv("POSTGRES_PASSWORD", "")
			if _, err := config.Load(path); err == nil {
				t.Errorf("%s: config.Load succeeded with an empty POSTGRES_PASSWORD, want fail-fast", path)
			}
		})
	}
	if checked == 0 {
		t.Errorf("no overlay under %s ships its own %s (nothing was checked)", overlaysDir, configFileName)
	}
}

// e2e overlay は受け入れ判定ハーネスの前提を 2 つ config に載せている。
// **その 2 つは、載っていることを誰かが見ていないと黙って外れる。**
//
//   - 2 サイト: 判定 5（サイト B の滞留でサイト A の Job が起きない）は
//     1 サイトでは原理的に測れない。減ると判定 5 は FAIL ではなく TODO に化ける
//   - worker.periodic_jobs = false: true に戻ると、判定 2 が「worker が自分で
//     投入して自分で消化した」だけでも緑になりうる（in-process の PeriodicJobs が
//     投入してしまうので、CronJob が止まっていても待ち行列が埋まる）
//
// どちらも「判定の有効性が config の 1 行に依存している」形なので、上の
// 汎用チェックとは別に名指しで固定する。
func TestE2EOverlayConfigKeepsTheHarnessPremises(t *testing.T) {
	t.Setenv("POSTGRES_PASSWORD", "s3cret")
	path := filepath.Join(overlaysDir, "e2e", configFileName)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", path, err)
	}
	if got := len(cfg.Mirakcs); got != 2 {
		t.Errorf("%s declares %d mirakc site(s), want 2 (check 5 cannot be measured with one)", path, got)
	}
	if cfg.Worker.PeriodicJobs {
		t.Errorf("%s has worker.periodic_jobs = true; check 2 would pass on in-process periodic jobs", path)
	}
}

// base が Secret を出荷しないこと（参考ファイルは apply されないこと）。
//
// **プレースホルダの Secret を出荷すると、`kustomize build base | kubectl apply`
// が外で作った本物のパスワードを上書きする。** 上書きした瞬間は動いている Pod が
// 死なない（env は起動時に読まれている）ので気付かず、次の rollout やノード退避で
// 初めて全 Pod が起動不能になる --- そのときパスワードはクラスタからも消えている。
// 参考ファイルはキー名の権威として残す（externalSecrets）。
func TestBaseShipsNoSecret(t *testing.T) {
	for _, o := range loadBase(t) {
		if o.kind() == "Secret" {
			t.Errorf("%s is shipped by base; keep secrets out of resources (see secret.example.yaml)", o.id())
		}
	}
	for name, example := range externalSecrets {
		path := filepath.Join(baseDir, example)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("externalSecrets[%q] points at %s, which does not exist: %v", name, example, err)
			continue
		}
		if slices.Contains(loadKustomization(t).Resources, example) {
			t.Errorf("%s is listed in resources; the example must not be applied", example)
		}

		// **参考ファイル自身の同定を見る。** これを見ないと、キー名の権威だと
		// 言っているファイルが別の名前・別の kind を主張していても通る
		// （実測で name / kind / 値の 3 つとも無検査だった）。
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Errorf("decoding %s: %v", example, err)
			continue
		}
		if got := strAt(doc, "kind"); got != "Secret" {
			t.Errorf("%s kind = %q, want Secret", example, got)
		}
		if got := strAt(doc, "metadata", "name"); got != name {
			t.Errorf("%s metadata.name = %q, want %q (the name base references)", example, got, name)
		}
		// 値は空であること。ダミー値を置くと fail-fast（`db.password is required`）が
		// 消えて、「Pod は起動して DB 認証に失敗し 503 を返し続ける」形になる
		// （TestConfigMapConfigFailsWithoutPassword と対で意味を持つ）。
		for _, field := range []string{"stringData", "data"} {
			for k, v := range mapAt(doc, field) {
				if fmt.Sprint(v) != "" {
					t.Errorf("%s %s[%q] = %q, want an empty placeholder", example, field, k, fmt.Sprint(v))
				}
			}
		}
	}
}

// generator の名前にハッシュ suffix が付く設定であること。
//
// **ハッシュは「config を変えたら rollout が起きる」の唯一の引き金**である
// （rokuban は config を起動時に 1 回しか読まない）。`disableNameSuffixHash: true`
// を足すと ConfigMap の世代は溜まらなくなるが、**設定変更が黙って効かない状態に
// 戻る**。kustomize も kubeconform もそれを止めない。
func TestGeneratorsKeepTheNameSuffixHash(t *testing.T) {
	k := loadKustomization(t)
	if k.GeneratorOptions.DisableNameSuffixHash {
		t.Error("generatorOptions.disableNameSuffixHash is true; config changes would stop triggering a rollout")
	}
	for _, gens := range [][]generator{k.ConfigMapGenerator, k.SecretGenerator} {
		for _, g := range gens {
			if g.Options.DisableNameSuffixHash {
				t.Errorf("generator %q sets options.disableNameSuffixHash; config changes would stop triggering a rollout", g.Name)
			}
		}
	}
	if len(k.ConfigMapGenerator) == 0 {
		t.Error("no configMapGenerator found (config would be a plain ConfigMap: no hash, no rollout)")
	}
}

// config.yml の `server.listen` のポートが api コンテナの containerPort と
// 一致すること。probe も Service の targetPort も名前付きポート経由でここを指すので、
// ずれると Pod は永久に Ready にならない。
func TestConfigListenPortMatchesContainerPort(t *testing.T) {
	t.Setenv("POSTGRES_PASSWORD", "s3cret")
	cfg, err := config.Load(filepath.Join(baseDir, configFileName))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	_, portStr, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil {
		t.Fatalf("splitting server.listen %q: %v", cfg.Server.Listen, err)
	}
	want, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}

	// **`http` という名前のポートだけを見る。** 全ポートと比較すると、将来 debug
	// 用のポートを 1 本足しただけで偽陽性で落ちる。
	c := container(t, podSpecs(loadBase(t)), apiDeployment, "api")
	found := false
	for _, p := range sliceAt(c, "ports") {
		port, ok := p.(map[string]any)
		if !ok || strAt(port, "name") != "http" {
			continue
		}
		found = true
		got, err := strconv.Atoi(fmt.Sprint(port["containerPort"]))
		if err != nil {
			t.Fatalf("parsing containerPort %v: %v", port["containerPort"], err)
		}
		if got != want {
			t.Errorf("containerPort = %d, but config.yml server.listen is %q", got, cfg.Server.Listen)
		}
	}
	if !found {
		t.Fatal(`api container declares no port named "http"`)
	}
}

// requiredConfigVars は config.yml が既定値なしで参照する `${VAR}` の一覧。
//
// **生テキストではなくパース後の値だけを見る。** コメントの中にも説明として
// `${VAR}` を書いてあるので、生テキストを走査すると実在しない変数を
// 「供給必須」に数えてしまう。
func requiredConfigVars(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(baseDir, configFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", configFileName, err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding %s: %v", configFileName, err)
	}
	var required []string
	for _, v := range stringValues(doc) {
		for _, m := range configVarRe.FindAllStringSubmatch(v, -1) {
			if m[2] == "" { // `:-default` を持たない = 供給が必須
				required = append(required, m[1])
			}
		}
	}
	if len(required) == 0 {
		t.Fatalf("no ${VAR} without a default found in %s (the regexp or the file changed shape)", configFileName)
	}
	return required
}

var configVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

// config.yml が既定値なしで参照する `${VAR}` を、その config を mount する
// すべての Pod が env として供給していること。
//
// **`envFrom` はキーを持たないので、参照解決の検査（TestManifestReferencesResolve）
// では Secret のキー名まで見られない。** `envFrom` を消しても、Secret のキーを
// `POSTGRES_PASSWORD` から改名しても、他のテストは全部緑のまま通る（実測）。
// 実際には `password:` が空に展開されて `db.password is required` で全 Pod が
// 起動しない。
func TestConfigVarsAreSupplied(t *testing.T) {
	required := requiredConfigVars(t)

	objs := loadBase(t)
	keysOf := dataKeys(t, objs)
	mounters := 0
	for id, podSpec := range podSpecs(objs) {
		mountsConfig := false
		for _, v := range sliceAt(podSpec, "volumes") {
			vol, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if cm := mapAt(vol, "configMap"); cm != nil {
				if keysOf["ConfigMap/"+strAt(cm, "name")][configFileName] {
					mountsConfig = true
				}
			}
		}
		if !mountsConfig {
			continue
		}
		mounters++

		for _, c := range containers(podSpec) {
			// **config を実際に読むコンテナだけを見る。** Pod 粒度でゲートすると、
			// 将来 migration 待ちの init container（別イメージ・env 不要）を足した
			// ときに、config を読まないコンテナへ env の供給を強要してしまう。
			var args []string
			for _, a := range sliceAt(c, "args") {
				args = append(args, fmt.Sprint(a))
			}
			if !slices.Contains(args, "--config") {
				continue
			}
			supplied := map[string]bool{}
			for _, ef := range sliceAt(c, "envFrom") {
				src, ok := ef.(map[string]any)
				if !ok {
					continue
				}
				for kind, field := range map[string]string{"ConfigMap": "configMapRef", "Secret": "secretRef"} {
					if r := mapAt(src, field); r != nil {
						for k := range keysOf[kind+"/"+strAt(r, "name")] {
							supplied[k] = true
						}
					}
				}
			}
			for _, e := range sliceAt(c, "env") {
				if env, ok := e.(map[string]any); ok {
					supplied[strAt(env, "name")] = true
				}
			}
			for _, v := range required {
				if !supplied[v] {
					t.Errorf("%s container %q mounts %s but does not supply ${%s}",
						id, strAt(c, "name"), configFileName, v)
				}
			}
		}
	}
	if mounters == 0 {
		t.Errorf("no pod mounts %s (nothing was checked)", configFileName)
	}
}

// Service の selector が実在する Pod テンプレートのラベルに当たること。
// 当たっていない Service は Endpoints が空のまま「正常に」存在するので、
// スキーマ検証では絶対に出ない。
func TestServiceSelectorsMatchAPod(t *testing.T) {
	objs := loadBase(t)

	labels := map[string]map[string]string{}
	for _, o := range objs {
		tmpl := podTemplate(o)
		if tmpl == nil {
			continue
		}
		l := map[string]string{}
		for k, v := range mapAt(tmpl, "metadata", "labels") {
			l[k] = fmt.Sprint(v)
		}
		labels[o.id()] = l
	}

	services := 0
	for _, o := range objs {
		if o.kind() != "Service" {
			continue
		}
		services++
		selector := mapAt(o.doc, "spec", "selector")
		if len(selector) == 0 {
			t.Errorf("%s has no selector", o.id())
			continue
		}
		matched := false
		for _, l := range labels {
			all := true
			for k, v := range selector {
				if l[k] != fmt.Sprint(v) {
					all = false
					break
				}
			}
			if all {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("%s selector %v matches no pod template in %s/", o.id(), selector, baseDir)
		}
	}
	if services == 0 {
		t.Error("no Service found (nothing was checked)")
	}
}

// Deployment の selector.matchLabels が自分の Pod テンプレートのラベルに一致すること
// （不一致だと apply 自体が拒否されるが、それが分かるのはクラスタに載せてから）。
func TestDeploymentSelectorMatchesOwnTemplate(t *testing.T) {
	deployments := 0
	for _, o := range loadBase(t) {
		if o.kind() != "Deployment" {
			continue
		}
		deployments++
		selector := mapAt(o.doc, "spec", "selector", "matchLabels")
		podLabels := mapAt(o.doc, "spec", "template", "metadata", "labels")
		if len(selector) == 0 {
			t.Errorf("%s has no spec.selector.matchLabels", o.id())
			continue
		}
		for k, v := range selector {
			if fmt.Sprint(podLabels[k]) != fmt.Sprint(v) {
				t.Errorf("%s selector %s=%v does not match its pod template label %v",
					o.id(), k, v, podLabels[k])
			}
		}
	}
	if deployments == 0 {
		t.Error("no Deployment found (nothing was checked)")
	}
}

// base/ の YAML ファイルが、resources か generator の入力のどちらかに必ず
// 挙がっていること。
//
// 挙げ忘れたファイルは `kustomize build` の出力に一切現れないまま CI が緑になる
// （kubeconform は build の出力しか見ない）。`.yml` / `.yaml` の両方を見るのは、
// 拡張子を変えただけで検査から消える穴を作らないため。
func TestKustomizationCoversEveryFile(t *testing.T) {
	k := loadKustomization(t)

	covered := map[string]bool{"kustomization.yaml": true}
	for _, r := range k.Resources {
		covered[r] = true
	}
	// 意図的に apply しないファイル（外部供給 Secret の形）。
	for _, example := range externalSecrets {
		covered[example] = true
	}
	for _, gens := range [][]generator{k.ConfigMapGenerator, k.SecretGenerator} {
		for _, g := range gens {
			for _, f := range g.Files {
				if i := strings.Index(f, "="); i >= 0 {
					f = f[i+1:]
				}
				covered[f] = true
			}
			for _, e := range g.Envs {
				covered[e] = true
			}
		}
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		t.Fatalf("reading %s: %v", baseDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !slices.Contains([]string{".yaml", ".yml"}, filepath.Ext(name)) {
			continue
		}
		if !covered[name] {
			t.Errorf("%s/%s exists but kustomization.yaml lists it neither in resources nor as a generator input",
				baseDir, name)
		}
	}
	for _, r := range k.Resources {
		if _, err := os.Stat(filepath.Join(baseDir, r)); err != nil {
			t.Errorf("kustomization.yaml lists resources entry %q, which does not exist: %v", r, err)
		}
	}
}

// api の probe が /readyz（readiness）と /healthz（liveness）に割り当てられていること。
//
// **この 2 つを取り違えても k8s は何も言わない**（どちらも 200 を返す実装がある）。
// liveness に /readyz を割り当てると、DB の瞬断で全 Pod が同時に再起動する
// —— readyz を足した理由そのものを裏返す事故なので、形で押さえる。
//
// readiness の `timeoutSeconds` がハンドラ側の上限（`api.readyzTimeout` = 2s）より
// 長いことも一緒に見る。同値以下だと kubelet が先に諦めるので、ハンドラが 503 を
// 返す経路（DB のハングを readiness の失敗に落とす）が一度も通らない。
func TestAPIProbesUseTheRightEndpoints(t *testing.T) {
	c := container(t, podSpecs(loadBase(t)), apiDeployment, "api")

	if got := strAt(c, "readinessProbe", "httpGet", "path"); got != "/readyz" {
		t.Errorf("readinessProbe path = %q, want %q", got, "/readyz")
	}
	if got := strAt(c, "livenessProbe", "httpGet", "path"); got != "/healthz" {
		t.Errorf("livenessProbe path = %q, want %q", got, "/healthz")
	}

	// internal/api の readyzTimeout（2s）。**実装の定数を参照せずリテラルで書く**
	// （参照すると、両方を同時に変えたときに何も主張しなくなる）。
	const handlerTimeoutSeconds = 2
	timeout, err := strconv.Atoi(fmt.Sprint(mapAt(c, "readinessProbe")["timeoutSeconds"]))
	if err != nil {
		t.Fatalf("parsing readinessProbe.timeoutSeconds: %v", err)
	}
	if timeout <= handlerTimeoutSeconds {
		t.Errorf("readinessProbe.timeoutSeconds = %d, want > %d (api.readyzTimeout)",
			timeout, handlerTimeoutSeconds)
	}
}

// probe と Service が名前で指しているポートが、コンテナに実在すること。
//
// **名前の解決は kustomize も kubeconform も見ない。** `ports[].name` を
// `http` から変えるだけで、probe は解決先を失って **Pod は永久に Ready にならず**、
// Service の Endpoints も機能しない。それでも build も schema 検証も緑になる（実測）。
func TestNamedPortsResolve(t *testing.T) {
	objs := loadBase(t)
	specs := podSpecs(objs)

	// Pod テンプレートごとの、コンテナが宣言したポート名。
	names := map[string]map[string]bool{}
	for id, spec := range specs {
		set := map[string]bool{}
		for _, c := range containers(spec) {
			for _, p := range sliceAt(c, "ports") {
				if port, ok := p.(map[string]any); ok {
					if n := strAt(port, "name"); n != "" {
						set[n] = true
					}
				}
			}
		}
		names[id] = set
	}

	checked := 0
	// probe 側。
	for id, spec := range specs {
		for _, c := range containers(spec) {
			for _, probe := range []string{"readinessProbe", "livenessProbe", "startupProbe"} {
				pr := mapAt(c, probe)
				if pr == nil {
					continue
				}
				port := mapAt(pr, "httpGet")["port"]
				name, ok := port.(string)
				if !ok { // 数値指定は解決の必要が無い
					continue
				}
				checked++
				if !names[id][name] {
					t.Errorf("%s container %q %s targets port %q, which the container does not declare",
						id, strAt(c, "name"), probe, name)
				}
			}
		}
	}

	// Service 側。selector に当たる Pod テンプレートのポート名で解決する。
	labels := map[string]map[string]string{}
	for _, o := range objs {
		if tmpl := podTemplate(o); tmpl != nil {
			l := map[string]string{}
			for k, v := range mapAt(tmpl, "metadata", "labels") {
				l[k] = fmt.Sprint(v)
			}
			labels[o.id()] = l
		}
	}
	for _, o := range objs {
		if o.kind() != "Service" {
			continue
		}
		selector := mapAt(o.doc, "spec", "selector")
		for _, p := range sliceAt(o.doc, "spec", "ports") {
			port, ok := p.(map[string]any)
			if !ok {
				continue
			}
			target, ok := port["targetPort"].(string)
			if !ok {
				continue
			}
			checked++
			resolved := false
			for id, l := range labels {
				matches := true
				for k, v := range selector {
					if l[k] != fmt.Sprint(v) {
						matches = false
						break
					}
				}
				if matches && names[id][target] {
					resolved = true
					break
				}
			}
			if !resolved {
				t.Errorf("%s targetPort %q resolves to no port name on its selected pods", o.id(), target)
			}
		}
	}

	if checked < 3 { // api の readiness / liveness と Service の targetPort
		t.Errorf("only %d named-port references were checked, want >= 3", checked)
	}
}

// `--config` の指す先が、実際に mount した ConfigMap のキーであること。
//
// mountPath を変える / `--config` のパスを変える / ConfigMap のキー名を変える、の
// どれ 1 つでも api は config を開けず CrashLoopBackOff になるが、3 つが噛み合って
// いるかを見るものが他に無い（実測: mountPath を変えても全テスト緑だった）。
func TestConfigFlagPointsAtTheMountedFile(t *testing.T) {
	objs := loadBase(t)
	keysOf := dataKeys(t, objs)

	checked := 0
	for id, spec := range podSpecs(objs) {
		// volume 名 → ConfigMap 名
		cmOf := map[string]string{}
		for _, v := range sliceAt(spec, "volumes") {
			vol, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if cm := mapAt(vol, "configMap"); cm != nil {
				cmOf[strAt(vol, "name")] = strAt(cm, "name")
			}
		}

		for _, c := range containers(spec) {
			var args []string
			for _, a := range sliceAt(c, "args") {
				args = append(args, fmt.Sprint(a))
			}
			i := slices.Index(args, "--config")
			if i < 0 || i+1 >= len(args) {
				continue
			}
			path := args[i+1]
			checked++

			// mount 先を探す。
			matched := ""
			for _, vm := range sliceAt(c, "volumeMounts") {
				m, ok := vm.(map[string]any)
				if !ok {
					continue
				}
				// ディレクトリ mount（`/etc/rokuban`）と、subPath による
				// ファイル単位 mount（`/etc/rokuban/config.yml`）の両方を許す。
				if mp := filepath.Clean(strAt(m, "mountPath")); mp != "." {
					if filepath.Dir(path) == mp || filepath.Clean(path) == mp {
						matched = cmOf[strAt(m, "name")]
					}
				}
			}
			if matched == "" {
				t.Errorf("%s container %q passes --config %s, but no volumeMount covers %s",
					id, strAt(c, "name"), path, filepath.Dir(path))
				continue
			}
			if !keysOf["ConfigMap/"+matched][filepath.Base(path)] {
				t.Errorf("%s container %q passes --config %s, but ConfigMap/%s has no key %q",
					id, strAt(c, "name"), path, matched, filepath.Base(path))
			}
		}
	}
	if checked < 2 { // api と migrate
		t.Errorf("only %d --config flags were checked, want >= 2", checked)
	}
}

// readiness が probe 1 回の失敗で Pod を Service から外さないこと。
//
// **何秒が正しいかは測っていない**（プール飽和が 503 になるという機構自体が
// `Acquire` が待つことからの帰結で、未測定）。ここで固定するのは
// 「1 回の失敗で外さない」だけ --- probe は本質的にノイズを含むので、
// これは測らなくても言える。実際の猶予（`periodSeconds` × これ）の値は
// api.yaml のコメントに根拠付きで書いてあり、#202 のハーネスが測る対象。
func TestReadinessDoesNotEvictOnASingleFailure(t *testing.T) {
	pr := mapAt(container(t, podSpecs(loadBase(t)), apiDeployment, "api"), "readinessProbe")
	if pr == nil {
		t.Fatal("api container has no readinessProbe")
	}
	threshold, err := strconv.Atoi(fmt.Sprint(pr["failureThreshold"]))
	if err != nil {
		t.Fatalf("parsing failureThreshold %v: %v", pr["failureThreshold"], err)
	}
	if threshold < 2 {
		t.Errorf("readinessProbe.failureThreshold = %d, want >= 2 (1 回の失敗で全レプリカが Endpoints から抜ける)", threshold)
	}
}

// `terminationGracePeriodSeconds` が preStop + シャットダウン予算を包むこと。
//
// grace が短いと kubelet は preStop の途中で SIGKILL するので、**SIGTERM が一度も
// 届かず `http.Server.Shutdown`（cmd/rokuban/server.go。10s 上限）が走らない** ---
// つまり進行中のリクエストが切れる。api.yaml のコメントはこの足し算を根拠として
// 書いているので、根拠と一緒に固定する（実測: grace を 3 にする変異はどのテストにも
// 掛からなかった）。
//
// **worker ロールの Pod を足すときは、同じ検査をそちらにも掛けること。**
// api は River クライアントを Start しないので足し算が 2 項で済んでいるが、
// worker ロールの Pod では drain のぶんが増える:
//
//	grace >= preStop の sleep + 10s + --soft-stop-timeout + 10s
//
// 内訳と、包まなかったときに何が起きるか（行が `running` のまま残り、ロール分割
// 構成では `JobRescuer` を動かす常駐クライアントが居ないので誰も回収しない）は
// docs/operations.md §5「Deployment 併用時」にある。ここを worker Pod へ広げる
// のは、worker の pod spec を書くタスクの担当である --- いま汎用のループを書いても
// 対象が 0 件で、通るだけのテストになる。
func TestTerminationBudgetCoversPreStop(t *testing.T) {
	specs := podSpecs(loadBase(t))
	spec, ok := specs[apiDeployment]
	if !ok {
		t.Fatalf("%s not found", apiDeployment)
	}
	grace, err := strconv.Atoi(fmt.Sprint(spec["terminationGracePeriodSeconds"]))
	if err != nil {
		t.Fatalf("parsing terminationGracePeriodSeconds %v: %v", spec["terminationGracePeriodSeconds"], err)
	}

	cmd := sliceAt(container(t, specs, apiDeployment, "api"), "lifecycle", "preStop", "exec", "command")
	if len(cmd) == 0 {
		t.Fatal("api container has no lifecycle.preStop.exec.command (Endpoints からの離脱を待たずにリスナが閉じる)")
	}
	sleep := 0
	for _, a := range cmd {
		if n, err := strconv.Atoi(fmt.Sprint(a)); err == nil {
			sleep = n
		}
	}
	if sleep == 0 {
		t.Fatalf("preStop command %v has no duration to compare against the grace period", cmd)
	}

	// 10 は cmd/rokuban/server.go の Shutdown 予算。**実装の定数を参照せず
	// リテラルで書く**（参照すると両方を同時に変えたときに何も主張しなくなる）。
	const shutdownBudgetSeconds = 10
	if grace < sleep+shutdownBudgetSeconds {
		t.Errorf("terminationGracePeriodSeconds = %d, want >= %d (preStop %ds + shutdown %ds)",
			grace, sleep+shutdownBudgetSeconds, sleep, shutdownBudgetSeconds)
	}
}

// 冗長だと主張する構成（replicas > 1）には PDB が対で要ること。
//
// PDB が無い Pod は `kubectl drain` が無条件に退去させるので、ノード 1 台の
// 退避・アップグレードで全レプリカが同時に落ちる（実測: PDB 有りだと 2 本目の
// eviction が `Cannot evict pod as it would violate the pod's disruption budget`
// で阻まれる）。
func TestAPIHasRedundancyAndAPDB(t *testing.T) {
	objs := loadBase(t)

	var apiLabels map[string]string
	replicas := 0
	for _, o := range objs {
		if o.id() != apiDeployment {
			continue
		}
		r, err := strconv.Atoi(fmt.Sprint(mapAt(o.doc, "spec")["replicas"]))
		if err != nil {
			t.Fatalf("parsing replicas: %v", err)
		}
		replicas = r
		apiLabels = map[string]string{}
		for k, v := range mapAt(o.doc, "spec", "template", "metadata", "labels") {
			apiLabels[k] = fmt.Sprint(v)
		}
	}
	if apiLabels == nil {
		t.Fatalf("%s not found", apiDeployment)
	}
	if replicas <= 1 {
		// **レプリカ数そのものは固定しない**（何本が正しいかを測る手段がまだ無く、
		// 1 本に落とすのは可逆な選択）。ただし PDB は minAvailable: 1 なので、
		// 1 本構成だと drain が永久にブロックする組み合わせになる。
		t.Errorf("%s replicas = %d: PDB(minAvailable: 1) と組むと drain がブロックし続ける。"+
			"1 本で運用するなら PDB 側も一緒に見直すこと", apiDeployment, replicas)
		return
	}

	found := false
	for _, o := range objs {
		if o.kind() != "PodDisruptionBudget" {
			continue
		}
		selector := mapAt(o.doc, "spec", "selector", "matchLabels")
		if len(selector) == 0 {
			t.Errorf("%s has no selector.matchLabels", o.id())
			continue
		}
		matches := true
		for k, v := range selector {
			if apiLabels[k] != fmt.Sprint(v) {
				matches = false
			}
		}
		if !matches {
			continue
		}
		found = true
		// minAvailable で書く。maxUnavailable だと replicas を 1 に落とした構成で
		// 「1 本しかないものを退去してよい」になり、主張がレプリカ数に依存して消える。
		if _, ok := mapAt(o.doc, "spec")["minAvailable"]; !ok {
			t.Errorf("%s does not set minAvailable", o.id())
		}
	}
	if !found {
		t.Errorf("no PodDisruptionBudget selects the api pods; a single node drain would take all replicas at once")
	}
}

// 全 Pod / 全コンテナが同じ締め方をしていること。
//
// 個々のマニフェストのコメントが根拠を書いている設定は、新しいワークロードを
// 足すときに書き忘れる形で消える。ここで一律に見る。
func TestPodsAreHardened(t *testing.T) {
	specs := podSpecs(loadBase(t))
	if len(specs) == 0 {
		t.Fatal("no pod specs found")
	}
	for id, spec := range specs {
		if spec["automountServiceAccountToken"] != false {
			t.Errorf("%s automountServiceAccountToken = %#v, want the boolean false", id, spec["automountServiceAccountToken"])
		}
		if mapAt(spec, "securityContext")["runAsNonRoot"] != true {
			t.Errorf("%s securityContext.runAsNonRoot is not true", id)
		}
		for _, c := range containers(spec) {
			sc := mapAt(c, "securityContext")
			where := fmt.Sprintf("%s container %q", id, strAt(c, "name"))
			if sc["readOnlyRootFilesystem"] != true {
				t.Errorf("%s readOnlyRootFilesystem = %#v, want the boolean true", where, sc["readOnlyRootFilesystem"])
			}
			if sc["allowPrivilegeEscalation"] != false {
				t.Errorf("%s allowPrivilegeEscalation = %#v, want the boolean false", where, sc["allowPrivilegeEscalation"])
			}
			drops := sliceAt(sc, "capabilities", "drop")
			if len(drops) != 1 || fmt.Sprint(drops[0]) != "ALL" {
				t.Errorf("%s capabilities.drop = %v, want [ALL]", where, drops)
			}
		}
	}
}

// migration Job が `migrate up` を打つこと。
//
// `up` を `down` に変えると **本番スキーマのロールバック**が 1 語差で通る
// （`rokuban migrate down` は存在するサブコマンド）。
func TestMigrateJobRunsUp(t *testing.T) {
	c := container(t, podSpecs(loadBase(t)), "Job/rokuban-migrate", "migrate")
	var args []string
	for _, a := range sliceAt(c, "args") {
		args = append(args, fmt.Sprint(a))
	}
	if !slices.Contains(args, "migrate") || !slices.Contains(args, "up") {
		t.Errorf("migrate args = %q, want `migrate up`", strings.Join(args, " "))
	}
	if slices.Contains(args, "down") {
		t.Errorf("migrate args = %q contain `down` (that rolls the schema back)", strings.Join(args, " "))
	}
}

// すべての Pod が `enableServiceLinks: false` であること。
//
// k8s は同一 namespace の Service ごとに `<SVCNAME>_PORT` /
// `<SVCNAME>_SERVICE_HOST` 等の env を全 Pod に注入する。rokuban の config は
// `${VAR}` を自分で展開するので、注入された名前と config の参照が衝突すると
// **config のパースで落ちて 1 つも起動しない** --- 実測: `postgres` Service の
// `POSTGRES_PORT=tcp://10.96.x.x:5432` が当時の `${POSTGRES_PORT:-5432}` に入り、
// api も migrate も CrashLoopBackOff になった（kind, 2026-08-26）。
//
// 既定が true なので、新しいワークロードを足すときに書き忘れる形で再発する。
func TestPodsDisableServiceLinks(t *testing.T) {
	specs := podSpecs(loadBase(t))
	if len(specs) == 0 {
		t.Fatal("no pod specs found")
	}
	for id, spec := range specs {
		v, ok := spec["enableServiceLinks"]
		if !ok {
			t.Errorf("%s does not set enableServiceLinks (default true injects Service env vars)", id)
			continue
		}
		// bool 以外（`"false"` のような文字列）も落とす。型アサートを
		// `if b, _ := v.(bool); b` と書くと、文字列は false 扱いで無言に通る。
		if v != false {
			t.Errorf("%s sets enableServiceLinks=%#v, want the boolean false", id, v)
		}
	}
}

// api Pod は「中央プロセス」（どのサイトにも束縛しない）として起動すること。
//
// `--sites=` を省くと、mirakcs レジストリに 2 サイト目を足した瞬間に
// 「--sites is required」で api が起動しなくなる（cmd/rokuban/sites.go の
// resolveSiteBinding）。単一サイト構成では省いても動くので、壊れるのは
// サイトを増やしたときだけ —— つまり気付くのが最も遅い形で壊れる。
func TestAPIArgsAreSiteIndependent(t *testing.T) {
	c := container(t, podSpecs(loadBase(t)), apiDeployment, "api")

	var args []string
	for _, a := range sliceAt(c, "args") {
		args = append(args, fmt.Sprint(a))
	}
	joined := strings.Join(args, " ")
	if !slices.Contains(args, "--sites=") {
		t.Errorf("api args = %q, want an explicit --sites= (unbound central process)", joined)
	}
	if !slices.Contains(args, "--roles") || !slices.Contains(args, "api") {
		t.Errorf("api args = %q, want --roles api", joined)
	}
}

// stringValues は YAML の値のうち文字列であるものを再帰的に集める。
func stringValues(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case map[string]any:
		var out []string
		for _, e := range t {
			out = append(out, stringValues(e)...)
		}
		return out
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, stringValues(e)...)
		}
		return out
	default:
		return nil
	}
}

// overlay も base と同じ規律で見る。
//
// **overlay のファイル挙げ忘れは base と同じく黙って無視される**（実測: overlay に
// patch ファイルを置いて resources/patches に書かないと、`kustomize build` は
// exit 0 でそれを出力に含めない）。
//
// あわせて `images:` の名前が base の image 名と一致することを見る。**一致しない
// `images:` エントリは kustomize が黙って無視する**ので、typo すると
// `ghcr.io/fetburner/rokuban:latest` のまま起動する（実測）。kind での確認は
// 手元でビルドしたイメージを測っているつもりで、実際にはリリース済みイメージを
// 測ることになる。
func TestOverlaysAreConsistent(t *testing.T) {
	baseImages := map[string]bool{}
	for _, spec := range podSpecs(loadBase(t)) {
		for _, c := range containers(spec) {
			img := strAt(c, "image")
			if i := strings.LastIndex(img, ":"); i >= 0 {
				img = img[:i]
			}
			baseImages[img] = true
		}
	}
	if len(baseImages) == 0 {
		t.Fatal("no container images found in base/")
	}

	entries, err := os.ReadDir(overlaysDir)
	if err != nil {
		t.Fatalf("reading %s: %v", overlaysDir, err)
	}
	overlays := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(overlaysDir, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
		if err != nil {
			t.Errorf("reading %s/kustomization.yaml: %v", dir, err)
			continue
		}
		var k kustomization
		if err := yaml.Unmarshal(raw, &k); err != nil {
			t.Errorf("decoding %s/kustomization.yaml: %v", dir, err)
			continue
		}
		overlays++

		for _, img := range k.Images {
			if !baseImages[img.Name] {
				t.Errorf("%s images[] targets %q, which no container in %s/ uses (kustomize ignores it silently)",
					dir, img.Name, baseDir)
			}
		}

		// **overlay の generator が実際に apply される供給元である。** base が
		// 出荷しない Secret を作っているのはここだけなので、名前もキーも
		// 検査対象にする（実測: キー名の typo・generator 名の typo は、どちらも
		// 全テスト緑のまま通っていた。クラスタでは CrashLoopBackOff か
		// CreateContainerConfigError になる）。
		//
		// **generator が「無い」ことは許す。** 外部管理（ESO / 手で作る）の
		// overlay が正当な構成なので、「Secret を供給し忘れた overlay」は
		// ここでは止まらない --- それはクラスタ側で
		// `CreateContainerConfigError` として出る（`kubectl get pods` の 1 行）。
		for _, g := range k.SecretGenerator {
			if _, ok := externalSecrets[g.Name]; !ok {
				t.Errorf("%s secretGenerator %q does not match any Secret that %s/ references",
					dir, g.Name, baseDir)
				continue
			}
			keys := map[string]bool{}
			for _, l := range g.Literals {
				if i := strings.Index(l, "="); i > 0 {
					keys[l[:i]] = true
				}
			}
			for _, e := range g.Envs {
				raw, err := os.ReadFile(filepath.Join(dir, e))
				if err != nil {
					continue // 手元にしか置かない運用（.gitignore）を許す
				}
				for _, line := range strings.Split(string(raw), "\n") {
					if i := strings.Index(line, "="); i > 0 {
						keys[strings.TrimSpace(line[:i])] = true
					}
				}
			}
			if len(g.Envs) > 0 && len(keys) == 0 {
				continue // envs だけの構成でファイルが手元に無い
			}
			for _, v := range requiredConfigVars(t) {
				if !keys[v] {
					t.Errorf("%s secretGenerator %q does not supply ${%s} (config.yml requires it)", dir, g.Name, v)
				}
			}
		}

		for _, r := range k.Resources {
			if _, err := os.Stat(filepath.Join(dir, r)); err != nil {
				t.Errorf("%s resources entry %q does not exist: %v", dir, r, err)
			}
		}

		// ファイルの挙げ忘れ。resources / patches / generator の入力を覆う。
		covered := map[string]bool{"kustomization.yaml": true}
		for _, r := range k.Resources {
			covered[r] = true
		}
		for _, r := range k.PatchesStrategicMerge {
			covered[r] = true
		}
		for _, p := range k.Patches {
			if m, ok := p.(map[string]any); ok {
				if path := strAt(m, "path"); path != "" {
					covered[path] = true
				}
			}
		}
		for _, gens := range [][]generator{k.ConfigMapGenerator, k.SecretGenerator} {
			for _, g := range gens {
				for _, f := range append(append([]string{}, g.Files...), g.Envs...) {
					if i := strings.Index(f, "="); i >= 0 {
						f = f[i+1:]
					}
					covered[f] = true
				}
			}
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("reading %s: %v", dir, err)
			continue
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !slices.Contains([]string{".yaml", ".yml"}, filepath.Ext(name)) {
				continue
			}
			if !covered[name] {
				t.Errorf("%s/%s exists but that overlay's kustomization.yaml does not reference it", dir, name)
			}
		}
	}
	if overlays == 0 {
		t.Errorf("no overlay found under %s (nothing was checked)", overlaysDir)
	}
}

// --- YAML のたどり方（map[string]any を掘るだけの薄いヘルパ）---

func mapAt(v any, keys ...string) map[string]any {
	cur := v
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	m, _ := cur.(map[string]any)
	return m
}

func sliceAt(v any, keys ...string) []any {
	cur := v
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	s, _ := cur.([]any)
	return s
}

func strAt(v any, keys ...string) string {
	cur := v
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[k]
	}
	s, _ := cur.(string)
	return s
}
