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
// RustFS と一致すること。
//
// **派生物を FS の置き場にすると、派生物を書く worker（encode / thumbnail）は全部その置き場を
// マウントしなければならない**（cmd/rokuban/stores.go の起動検査が拒む）。置き場が 2 つの構成で
// 派生物を 1 つの FS に寄せると、もう一方の置き場の worker が起動できない。
//
// 判定 6（deploy/k8s/e2e/checks/06-two-stores.sh）の派生物は RustFS を通るので、config の
// endpoint が RustFS Service と、bucket が作成 Job と一致することを固定する。
// ずれると、判定 6.2 が「派生物の書き込みに失敗」で赤くなり、原因が読めなくなる。
func TestTwoStoreOverlayDerivedIsS3OnTheRustFSService(t *testing.T) {
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
	readYAMLFile(t, filepath.Join(overlayDir, "rustfs", "service.yaml"), &svc)
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("rustfs Service has %d ports, want 1", len(svc.Spec.Ports))
	}
	wantEndpoint := fmt.Sprintf("http://%s:%d", svc.Metadata.Name, svc.Spec.Ports[0].Port)
	if derived.Endpoint != wantEndpoint {
		t.Errorf("storage.stores.%s.endpoint = %q, want %q (the rustfs Service)", cfg.Storage.Derived, derived.Endpoint, wantEndpoint)
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
	readYAMLFile(t, filepath.Join(overlayDir, "rustfs", "bucket-job.yaml"), &job)
	// bucket の作成は curl の SigV4 PUT（script の中の URL）。URL の bucket 名が config と同じこと。
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("rustfs/bucket-job.yaml has %d containers, want 1", len(job.Spec.Template.Spec.Containers))
	}
	script := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	if !strings.Contains(script, "http://rustfs:9000/"+derived.Bucket+"\"") {
		t.Errorf("rustfs/bucket-job.yaml does not PUT bucket %q on the rustfs Service (script = %q)", derived.Bucket, script)
	}
}

// TestTwoStoreRustFSDeploymentUsesPinnedImage は RustFS のイメージ ref が CI と判定ハーネスで
// 同じダイジェストを使い、設定・ボリュームがイメージの実行契約と一致することを確かめる。
func TestTwoStoreRustFSDeploymentUsesPinnedImage(t *testing.T) {
	const overlayDir = "overlays/e2e-two-stores"

	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string   `yaml:"image"`
						Args  []string `yaml:"args"`
						Env   []struct {
							Name      string `yaml:"name"`
							Value     string `yaml:"value"`
							ValueFrom struct {
								SecretKeyRef struct {
									Name string `yaml:"name"`
									Key  string `yaml:"key"`
								} `yaml:"secretKeyRef"`
							} `yaml:"valueFrom"`
						} `yaml:"env"`
						VolumeMounts []struct {
							Name      string `yaml:"name"`
							MountPath string `yaml:"mountPath"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
					Volumes []struct {
						Name     string    `yaml:"name"`
						EmptyDir *struct{} `yaml:"emptyDir"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	readYAMLFile(t, filepath.Join(overlayDir, "rustfs", "deployment.yaml"), &dep)
	envImage := envShellValue(t, "E2E_RUSTFS_IMAGE")
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("reading CI workflow: %v", err)
	}
	var ci struct {
		Env struct {
			RustFSImage string `yaml:"RUSTFS_IMAGE"`
		} `yaml:"env"`
		Jobs map[string]struct {
			Services map[string]struct {
				Image string `yaml:"image"`
			} `yaml:"services"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &ci); err != nil {
		t.Fatalf("decoding CI workflow: %v", err)
	}
	ciImage := ci.Env.RustFSImage
	if ciImage == "" || ciImage != envImage {
		t.Errorf("CI RUSTFS_IMAGE = %q, want lib/env.sh E2E_RUSTFS_IMAGE %q", ciImage, envImage)
	}
	for _, jobName := range []string{"browser-s3-302", "s3-conformance"} {
		job, ok := ci.Jobs[jobName]
		if !ok {
			t.Errorf("CI job %q is missing", jobName)
			continue
		}
		service, ok := job.Services["rustfs"]
		if !ok || service.Image != ciImage {
			t.Errorf("CI job %q RustFS image = %q, want shared RUSTFS_IMAGE %q", jobName, service.Image, ciImage)
		}
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("rustfs/deployment.yaml has %d containers, want 1", len(dep.Spec.Template.Spec.Containers))
	}
	container := dep.Spec.Template.Spec.Containers[0]
	if container.Image != envImage || !strings.Contains(container.Image, "@sha256:") {
		t.Errorf("rustfs/deployment.yaml image = %q, want the digest-pinned lib/env.sh E2E_RUSTFS_IMAGE %q", container.Image, envImage)
	}
	if !slices.Contains(container.Args, "/data") {
		t.Errorf("rustfs container args = %v, want /data", container.Args)
	}
	envValues := make(map[string]string, len(container.Env))
	envSecrets := make(map[string]string, len(container.Env))
	for _, env := range container.Env {
		envValues[env.Name] = env.Value
		if env.ValueFrom.SecretKeyRef.Name != "" {
			envSecrets[env.Name] = env.ValueFrom.SecretKeyRef.Name + "/" + env.ValueFrom.SecretKeyRef.Key
		}
	}
	if envValues["RUSTFS_VOLUMES"] != "/data" || envValues["RUSTFS_OBS_LOG_DIRECTORY"] != "/logs" {
		t.Errorf("RustFS data/log directories = %q/%q, want /data and /logs", envValues["RUSTFS_VOLUMES"], envValues["RUSTFS_OBS_LOG_DIRECTORY"])
	}
	if envSecrets["RUSTFS_ACCESS_KEY"] != "rokuban-secrets/AWS_ACCESS_KEY_ID" || envSecrets["RUSTFS_SECRET_KEY"] != "rokuban-secrets/AWS_SECRET_ACCESS_KEY" {
		t.Errorf("RustFS credential sources = %v, want the shared rokuban-secrets AWS keys", envSecrets)
	}
	mountPaths := make(map[string]string, len(container.VolumeMounts))
	for _, mount := range container.VolumeMounts {
		mountPaths[mount.Name] = mount.MountPath
	}
	if mountPaths["data"] != "/data" || mountPaths["logs"] != "/logs" {
		t.Errorf("RustFS volume mounts = %v, want data at /data and logs at /logs", mountPaths)
	}
	emptyDirs := make(map[string]bool, len(dep.Spec.Template.Spec.Volumes))
	for _, volume := range dep.Spec.Template.Spec.Volumes {
		emptyDirs[volume.Name] = volume.EmptyDir != nil
	}
	if !emptyDirs["data"] || !emptyDirs["logs"] {
		t.Errorf("RustFS emptyDir volumes = %v, want data and logs", emptyDirs)
	}
}

// TestTwoStoreOverlayIncludesRustFS は kustomization が RustFS のリソースを含めることを確かめる。
func TestTwoStoreOverlayIncludesRustFS(t *testing.T) {
	const overlayDir = "overlays/e2e-two-stores"

	var kust struct {
		Resources []string `yaml:"resources"`
	}
	readYAMLFile(t, filepath.Join(overlayDir, "kustomization.yaml"), &kust)
	if !slices.Contains(kust.Resources, "rustfs") {
		t.Errorf("%s/kustomization.yaml does not include the rustfs directory (resources = %v)", overlayDir, kust.Resources)
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
