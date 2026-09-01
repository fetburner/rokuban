//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mirakcImage は判定対象の mirakc の版。
//
// **タグではなく digest で固定する。** rokuban の ingest / streamer が依存する
// `/api/recording/records/*`（records.rs）は、最新の番号付きリリース（3.4.83.
// `docker.io/mirakc/mirakc:3.4.83-debian` として配布）にはまだ存在せず、
// `main` ブランチにしかない（2026-09-02 時点で確認。upstream の週次リリースに
// records.rs が入ったら、そちらの番号付きタグへ pin を差し替える。手順は
// docs/runbook/testing.md 参照）。`main`（`main-debian`）はビルドのたびに
// 中身が変わる可動タグなので、そのままでは版上げの回帰検知にならない
// --- pull した digest をここに固定することで再現性を保つ。
const mirakcImage = "docker.io/mirakc/mirakc@sha256:3fd884b3bb7c5c33f6d9241abf36db81b6fe25e42a869b8f14ecddd482a41c93"

// mirakcVersion は mirakcImage が実際に埋め込んでいる版（GetVersion で照合する。
// pin の上げ忘れで別物を判定していないことの検査 --- 受け入れ項目 5）。
const mirakcVersion = "4.0.0-dev.0"

// dockerArch は docker デーモンが実行するコンテナの CPU アーキテクチャを Go の GOARCH 名で返す。
// フィクスチャの偽チューナーはコンテナに bind mount するバイナリなので、ホストではなく
// デーモン側のアーキテクチャでクロスビルドする必要がある（同一ホストでも Docker Desktop の
// 仮想化は既定でネイティブアーキテクチャの一致を保証しない構成があるため、`docker version` から
// 実際の値を読む）。
func dockerArch(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	arch := strings.TrimSpace(string(out))
	if arch == "" {
		t.Fatalf("docker version returned an empty arch")
	}
	return arch
}

// buildFixtureTuner は internal/mirakc/conformance/cmd/fixturetuner を mirakc コンテナ向けに
// クロスビルドし、そのバイナリのホスト側パスを返す。dir は Docker と共有されているホストの
// ディレクトリ（bind mount できる場所）でなければならない --- macOS の Docker Desktop は
// 既定で /tmp 配下のすべてを共有しているわけではなく、リポジトリ配下（このパッケージの
// ディレクトリ）は確実に共有される（実測で確認した。t.TempDir() の既定の置き場所
// （os.TempDir() 配下）はホストによっては共有されないことがあるので使わない）。
func buildFixtureTuner(t *testing.T, dir string) string {
	t.Helper()
	arch := dockerArch(t)
	out := filepath.Join(dir, "fixturetuner")
	cmd := exec.Command("go", "build", "-tags", "conformance", "-o", out,
		"github.com/fetburner/rokuban/internal/mirakc/conformance/cmd/fixturetuner")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	cmd.Dir = repoRoot(t)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building fixturetuner (GOARCH=%s): %v\n%s", arch, err, stderr.String())
	}
	if err := os.Chmod(out, 0o755); err != nil {
		t.Fatalf("chmod fixturetuner: %v", err)
	}
	return out
}

// testDir はこのテスト専用の作業ディレクトリを作る。t.TempDir() を使わないのは、その既定の
// 置き場所（os.TempDir() 配下）が Docker Desktop の bind mount 共有パスに含まれるとは限らない
// ため（実測: macOS でこのテストを書いた際、/private/tmp 配下の一時ディレクトリを bind mount
// しても、コンテナ側には空のディレクトリしか見えなかった）。リポジトリ配下は確実に共有される。
func testDir(t *testing.T) string {
	t.Helper()
	base := filepath.Join(repoRoot(t), "internal", "mirakc", "conformance", ".tmp")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	dir, err := os.MkdirTemp(base, fmt.Sprintf("%d-", os.Getpid()))
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// repoRoot はこのテストが属す go module のルートを返す（`go build` の作業ディレクトリに使う。
// GOFLAGS 等に依存せず常にモジュールルートから解決させるため）。
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

// mirakcConfigYAML は conformance テスト用の mirakc 設定を組み立てる。
//
// filters は一切書かない --- program-filter の既定コマンドは mirakc-core 組み込みの
// `mirakc-arib filter-program ...`（mirakc-core/src/config.rs
// `FiltersConfig::default_program_filter`）であり、録画パイプラインはこれを暗黙に使う。
// jobs の schedule は cron（7 フィールド、秒単位）を 3 秒間隔にして、EPG 収集
// （scan-services / sync-clocks / update-schedules）を実運用よりずっと速く回す。
func mirakcConfigYAML(tunerCommand string) string {
	return fmt.Sprintf(`server:
  addrs:
    - http: '0.0.0.0:40772'

channels:
  - name: conformance
    type: GR
    channel: '1'

tuners:
  - name: fixture
    types: [GR]
    command: %s
    decoded: true

jobs:
  scan-services:
    schedule: '*/3 * * * * * *'
  sync-clocks:
    schedule: '*/3 * * * * * *'
  update-schedules:
    schedule: '*/3 * * * * * *'

recording:
  basedir: /recordings
  records-dir: /recordings/records
`, tunerCommand)
}

// mirakcContainer は起動済みの mirakc コンテナへのハンドルである。
type mirakcContainer struct {
	name    string
	baseURL string
}

// startMirakc は mirakc 実物のコンテナを起こし、/api/version が応答するまで待つ。
// t.Cleanup でコンテナの停止・削除を登録する。
func startMirakc(t *testing.T, hostDir string, tunerBin string) *mirakcContainer {
	t.Helper()

	recBase := filepath.Join(hostDir, "recordings")
	recRecords := filepath.Join(recBase, "records")
	if err := os.MkdirAll(recRecords, 0o755); err != nil {
		t.Fatalf("mkdir recordings: %v", err)
	}

	configDir := filepath.Join(hostDir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	configPath := filepath.Join(configDir, "config.yml")
	if err := os.WriteFile(configPath, []byte(mirakcConfigYAML("/fixtures/fixturetuner")), 0o644); err != nil {
		t.Fatalf("write config.yml: %v", err)
	}

	name := fmt.Sprintf("rokuban-conformance-%d", os.Getpid())
	// 既存の同名コンテナが残っていたら先に消す（前回異常終了時の掃除）。
	_ = exec.Command("docker", "rm", "-f", name).Run()

	args := []string{
		"run", "-d", "--rm",
		"--name", name,
		"-p", "127.0.0.1:0:40772",
		"-v", tunerBin + ":/fixtures/fixturetuner:ro",
		"-v", configPath + ":/etc/mirakc/config.yml:ro",
		"-v", recBase + ":/recordings",
		mirakcImage,
	}
	cmd := exec.Command("docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, stderr.String())
	}

	c := &mirakcContainer{name: name}
	t.Cleanup(func() {
		dumpContainerLogsOnFailure(t, name)
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	port := waitForPublishedPort(t, name, 40772)
	c.baseURL = fmt.Sprintf("http://127.0.0.1:%s", port)
	waitForHTTP(t, c.baseURL+"/api/version", 30*time.Second)
	return c
}

func dumpContainerLogsOnFailure(t *testing.T, name string) {
	t.Helper()
	if !t.Failed() {
		return
	}
	out, _ := exec.Command("docker", "logs", "--tail", "200", name).CombinedOutput()
	t.Logf("mirakc container logs (tail):\n%s", out)
}

// waitForPublishedPort は docker がランダム割当てたホスト側ポートが判明するまで待つ
// （`docker run -p 127.0.0.1:0:40772` の直後は割当てにわずかに遅延がありうる）。
func waitForPublishedPort(t *testing.T, name string, containerPort int) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "port", name, fmt.Sprintf("%d/tcp", containerPort)).Output()
		if err == nil {
			line := strings.TrimSpace(string(out))
			if idx := strings.LastIndex(line, ":"); idx >= 0 {
				return line[idx+1:]
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("mirakc container %s never published port %d", name, containerPort)
	return ""
}

func waitForHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %s", resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready within %s: %v", url, timeout, lastErr)
}
