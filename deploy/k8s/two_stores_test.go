package k8s

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// 置き場が 2 つの overlay の派生物は S3 であり、その endpoint・bucket・イメージは overlay に同梱した
// MinIO と一致すること。
//
// **派生物を FS の置き場にすると、派生物を書く worker（encode / thumbnail）は全部その置き場を
// マウントしなければならない**（cmd/rokuban/stores.go の起動検査が拒む）。置き場が 2 つの構成で
// 派生物を 1 つの FS に寄せると、もう一方の置き場の worker が起動できない。
//
// 判定 6（deploy/k8s/e2e/checks/06-two-stores.sh）の派生物は MinIO を通るので、ここで
// config と overlay の 3 か所（endpoint の Service 名・bucket の作成・イメージ名）が
// ずれていないことを固定する。ずれると、判定 6.2 が「派生物の書き込みに失敗」で赤くなり、
// 原因が config か manifest か読めなくなる。
func TestTwoStoreOverlayDerivedIsS3OnTheMinIOService(t *testing.T) {
	const overlayDir = "overlays/e2e-two-stores"

	raw, err := os.ReadFile(filepath.Join(overlayDir, configFileName))
	if err != nil {
		t.Fatalf("reading %s/%s: %v", overlayDir, configFileName, err)
	}
	var cfg struct {
		Storage struct {
			Derived string `yaml:"derived"`
			Stores  map[string]struct {
				Kind     string `yaml:"kind"`
				Endpoint string `yaml:"endpoint"`
				Bucket   string `yaml:"bucket"`
			} `yaml:"stores"`
		} `yaml:"storage"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decoding %s/%s: %v", overlayDir, configFileName, err)
	}
	derived, ok := cfg.Storage.Stores[cfg.Storage.Derived]
	if !ok || derived.Kind != "s3" {
		t.Fatalf("storage.derived = %q, want an s3 store (two fs stores need the derived store mounted by every derived-writing worker otherwise); stores = %+v",
			cfg.Storage.Derived, cfg.Storage.Stores)
	}
	if derived.Bucket == "" {
		t.Errorf("storage.stores.%s has no bucket", cfg.Storage.Derived)
	}

	// endpoint の host は overlay の Service と同じ名前・ポートを指す。
	var svc struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Ports []struct {
				Port int `yaml:"port"`
			} `yaml:"ports"`
		} `yaml:"spec"`
	}
	readYAMLFile(t, filepath.Join(overlayDir, "minio", "service.yaml"), &svc)
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("minio Service has %d ports, want 1", len(svc.Spec.Ports))
	}
	wantEndpoint := fmt.Sprintf("http://%s:%d", svc.Metadata.Name, svc.Spec.Ports[0].Port)
	if derived.Endpoint != wantEndpoint {
		t.Errorf("storage.stores.%s.endpoint = %q, want %q (the minio Service)", cfg.Storage.Derived, derived.Endpoint, wantEndpoint)
	}

	// bucket は bucket-job が作る bucket と同じ。
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Args []string `yaml:"args"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	readYAMLFile(t, filepath.Join(overlayDir, "minio", "bucket-job.yaml"), &job)
	// bucket の作成は curl の SigV4 PUT（script の中の URL）。URL の bucket 名が config と同じこと。
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("minio/bucket-job.yaml has %d containers, want 1", len(job.Spec.Template.Spec.Containers))
	}
	script := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	if !strings.Contains(script, "http://minio:9000/"+derived.Bucket+"\"") {
		t.Errorf("minio/bucket-job.yaml does not PUT bucket %q on the minio Service (script = %q)", derived.Bucket, script)
	}

	// MinIO のイメージ名は判定ハーネスが kind に載せる名前と同じ（lib/env.sh の E2E_MINIO_IMAGE）。
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `yaml:"image"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	readYAMLFile(t, filepath.Join(overlayDir, "minio", "deployment.yaml"), &dep)
	envImage := envShellValue(t, "E2E_MINIO_IMAGE")
	if len(dep.Spec.Template.Spec.Containers) != 1 || dep.Spec.Template.Spec.Containers[0].Image != envImage {
		t.Errorf("minio/deployment.yaml image = %+v, want %q (lib/env.sh E2E_MINIO_IMAGE; lib/cluster.sh builds that name)",
			dep.Spec.Template.Spec.Containers, envImage)
	}

	// overlay がその 3 つを読んでいる（書いても組まれなければ判定 6 は S3 を測れない）。
	var kust struct {
		Resources []string `yaml:"resources"`
	}
	readYAMLFile(t, filepath.Join(overlayDir, "kustomization.yaml"), &kust)
	if !slices.Contains(kust.Resources, "minio") {
		t.Errorf("%s/kustomization.yaml does not include the minio directory (resources = %v)", overlayDir, kust.Resources)
	}
}

// readYAMLFile は YAML を v に読む。読めなければテストを止める。
func readYAMLFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := yaml.Unmarshal(raw, v); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

// envShellValue は e2e/lib/env.sh の `NAME="value"` の value を返す。
// 判定ハーネスの値はシェルの中にしかないので、Go テストはそれを行単位で読む
// （TestE2EStoreQueuesMatchJobs と同じ方法）。
func envShellValue(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("e2e/lib/env.sh")
	if err != nil {
		t.Fatalf("reading e2e/lib/env.sh: %v", err)
	}
	prefix := name + `="`
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSuffix(v, `"`)
		}
	}
	t.Fatalf("e2e/lib/env.sh does not set %s", name)
	return ""
}
