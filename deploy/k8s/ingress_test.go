package k8s

import (
	"fmt"
	"strings"
	"testing"
)

// base の入口は site 名を知らない共通 3 経路だけを持つ。具体的なライブ経路は
// overlay が site ごとに追加するため、単一ホストの routing table が method / query /
// header / regex に依存しないことをここで固定する。
func TestBaseIngressUsesOnlyFixedCommonRoutes(t *testing.T) {
	var ingress object
	for _, o := range loadBase(t) {
		if o.kind() == "Ingress" {
			if ingress.kind() != "" {
				t.Fatalf("multiple Ingress objects found in %s/: %s and %s", baseDir, ingress.id(), o.id())
			}
			ingress = o
		}
	}
	if ingress.kind() == "" {
		t.Fatalf("no Ingress found in %s/", baseDir)
	}
	rules := sliceAt(ingress.doc, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("%s has %d rules, want exactly 1 single-host rule", ingress.id(), len(rules))
	}
	rule, ok := rules[0].(map[string]any)
	if !ok {
		t.Fatalf("%s spec.rules[0] is not a map", ingress.id())
	}
	if got := strAt(rule, "host"); got != "rokuban.local" {
		t.Errorf("%s host = %q, want %q", ingress.id(), got, "rokuban.local")
	}

	want := map[string]struct {
		pathType string
		service  string
	}{
		"/api/events":           {pathType: "Exact", service: "rokuban-notifier"},
		"/api/media/recordings": {pathType: "Prefix", service: "rokuban-streamer"},
		"/":                     {pathType: "Prefix", service: "rokuban-api"},
	}
	paths := sliceAt(mapAt(rule, "http"), "paths")
	if len(paths) != len(want) {
		t.Fatalf("%s has %d common paths, want %d", ingress.id(), len(paths), len(want))
	}
	for _, raw := range paths {
		path, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("%s contains a non-map path entry", ingress.id())
			continue
		}
		name := strAt(path, "path")
		expect, ok := want[name]
		if !ok {
			t.Errorf("%s contains unexpected common path %q", ingress.id(), name)
			continue
		}
		if got := strAt(path, "pathType"); got != expect.pathType {
			t.Errorf("%s path %q has pathType %q, want %q", ingress.id(), name, got, expect.pathType)
		}
		if got := strAt(path, "backend", "service", "name"); got != expect.service {
			t.Errorf("%s path %q backend = %q, want %q", ingress.id(), name, got, expect.service)
		}
		if got := strAt(path, "backend", "service", "port", "name"); got != "http" {
			t.Errorf("%s path %q backend port = %q, want %q", ingress.id(), name, got, "http")
		}
	}
}

// overlay の live route patch は、具体的な site 名と同じ site Service を指す。
// JSON patch の value は kubeconform には見えるが、Service 名の typo は見えない。
func TestIngressOverlaysAddConcreteLiveRoutes(t *testing.T) {
	cases := []struct {
		dir  string
		want map[string]string
	}{
		{
			dir:  "overlays/kind",
			want: map[string]string{"/api/sites/default/networks": "rokuban-live-streamer"},
		},
		{
			dir: "overlays/e2e",
			want: map[string]string{
				"/api/sites/sitea/networks": "rokuban-live-streamer-sitea",
				"/api/sites/siteb/networks": "rokuban-live-streamer-siteb",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			k := loadOverlayPatches(t, tc.dir)
			seen := map[string]bool{}
			for _, p := range k.Patches {
				if p.Target.Kind != "Ingress" || p.Target.Name != "rokuban-ingress" {
					continue
				}
				for _, op := range parseJSONPatch(t, p.Patch) {
					if op.Op != "add" {
						t.Errorf("Ingress patch op = %q, want add", op.Op)
						continue
					}
					value, ok := op.Value.(map[string]any)
					if !ok {
						t.Errorf("Ingress patch value = %#v, want a path map", op.Value)
						continue
					}
					path := strAt(value, "path")
					service := strAt(value, "backend", "service", "name")
					wantService, ok := tc.want[path]
					if !ok {
						t.Errorf("Ingress patch adds unexpected path %q", path)
						continue
					}
					if service != wantService {
						t.Errorf("Ingress path %q backend = %q, want %q", path, service, wantService)
					}
					if got := strAt(value, "pathType"); got != "Prefix" {
						t.Errorf("Ingress path %q pathType = %q, want Prefix", path, got)
					}
					if got := strAt(value, "backend", "service", "port", "name"); got != "http" {
						t.Errorf("Ingress path %q backend port = %q, want http", path, got)
					}
					seen[path] = true
				}
			}
			for path := range tc.want {
				if !seen[path] {
					t.Errorf("%s does not add live route %q", tc.dir, path)
				}
			}
			if len(seen) != len(tc.want) {
				t.Errorf("%s adds %d live routes, want %d", tc.dir, len(seen), len(tc.want))
			}
		})
	}
}

// Service / Ingress に affinity を書くと、VOD の Range 読みもライブの URL hash も
// 壊れる。未指定（Service の既定 None）だけを許す。
func TestServicesAndIngressDoNotConfigureStickyAffinity(t *testing.T) {
	for _, o := range loadAll(t) {
		if o.kind() == "Service" {
			if got, ok := mapAt(o.doc, "spec")["sessionAffinity"]; ok && fmt.Sprint(got) != "None" {
				t.Errorf("%s sets sessionAffinity=%v; sticky session affinity is forbidden", o.id(), got)
			}
		}
		if o.kind() == "Ingress" {
			for key := range mapAt(o.doc, "metadata", "annotations") {
				if strings.Contains(strings.ToLower(key), "affinity") {
					t.Errorf("%s annotation %q configures affinity; sticky sessions are forbidden", o.id(), key)
				}
			}
		}
	}
}
