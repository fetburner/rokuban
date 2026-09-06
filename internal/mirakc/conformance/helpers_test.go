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
// **`main` ブランチのビルドを digest で固定する。番号付きリリースには戻らない。**
// rokuban の ingest / streamer が依存する `/api/recording/records/*`（records.rs）は
// 最新の番号付きリリース（3.4.83）には存在せず `main` にしかない（2026-09-02 時点で
// `docker run mirakc/mirakc:3.4.83-debian` の config パースが `records-dir` を
// unknown field として拒否し、`/api/recording/records` が 404 になることで確認した）。
// **rokuban はどのバージョンの mirakc イメージも自身で出荷しない**（mirakc は運用者が
// 用意するもの）ので、rokuban 側に「本番の pin」は存在しない --- この conformance の
// pin こそが唯一の判定対象であり、番号付きリリースが records.rs を含むようになっても
// 差し替える理由にはならない。
//
// `main`（`main-debian`）はビルドのたびに中身が変わる可動タグなので、そのままでは
// 版上げの回帰検知にならない --- pull した digest をここに固定することで再現性を保つ。
// **この digest はいずれ Docker Hub 上で prune されうる**（`main-debian` は継続的に
// 上書きされる可動タグで、古い digest は untagged manifest として残るだけ）。
// pull 失敗はテストの回帰ではなく pin の失効なので、docs/runbook/testing.md の
// 「mirakc の版を上げる手順」の手順で最新の digest に取り直す。
const mirakcImage = "docker.io/mirakc/mirakc@sha256:3fd884b3bb7c5c33f6d9241abf36db81b6fe25e42a869b8f14ecddd482a41c93"

const mirakcImageEnv = "ROKUBAN_CONFORMANCE_MIRAKC_IMAGE"

// configuredMirakcImage returns the pinned image unless the caller supplies an image override.
func configuredMirakcImage() string {
	if image := os.Getenv(mirakcImageEnv); image != "" {
		return image
	}
	return mirakcImage
}

// TestMirakcRunArgsImage は startMirakc が実際に docker へ渡す引数列の末尾（イメージ名）を
// 見る。configuredMirakcImage() 単体のテストでは、startMirakc 内の配線
// （args = append(args, configuredMirakcImage())）を pin 定数へ直接差し替える変異を
// 検出できない。mirakcRunArgs を経由することで、その配線そのものを検査対象にする。
func TestMirakcRunArgsImage(t *testing.T) {
	lastArg := func(args []string) string { return args[len(args)-1] }

	t.Run("uses the pinned image by default", func(t *testing.T) {
		t.Setenv(mirakcImageEnv, "")
		args := mirakcRunArgs("c", "/tuner", "/config.yml", "/recordings", "")
		if got := lastArg(args); got != mirakcImage {
			t.Fatalf("mirakcRunArgs last arg = %q, want pinned image %q", got, mirakcImage)
		}
	})

	t.Run("uses the environment override", func(t *testing.T) {
		const want = "docker.io/mirakc/mirakc:main-debian"
		t.Setenv(mirakcImageEnv, want)
		args := mirakcRunArgs("c", "/tuner", "/config.yml", "/recordings", "")
		if got := lastArg(args); got != want {
			t.Fatalf("mirakcRunArgs last arg = %q, want %q", got, want)
		}
	})
}

// mirakcVersion は mirakcImage が実際に埋め込んでいる版（GetVersion で照合する。
// 受け入れ項目 5）。
//
// **この照合が捕まえるのは番号付きの版が変わったことだけ。** `main` の Cargo バージョンは
// 4.0.0 が出るまでどの main ビルドでも "4.0.0-dev.0" のままなので、mirakcImage の
// digest を差し替えても（＝別のコミットに pin し直しても）ここは変わらない ---
// `main` ビルド同士を区別する検査ではない（`docker run <image@digest>` を使っている
// 時点で起動イメージが pin と一致することはそちらで保証済みなので、この照合を
// digest 検査の代わりにする必要もない）。この定数の実質的な価値は、
// delegation.md / reconciler.md / ingest.md / troubleshooting.md が引用する
// 「mirakc 4.0.0-dev.0 相当」という文言を嘘にしないことである。
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

// mirakcLiveReleaseConfigYAML はライブストリームのチューナー解放遅延を測るための構成。
// 3 つの異なる channel / service を 2 本の tuner で受ける。録画 A とライブ B がそれぞれ
// tuner を占有した状態でライブ C を要求し、B の Close 後に C が通るかを測る。
func mirakcLiveReleaseConfigYAML(tunerCommand string) string {
	return fmt.Sprintf(`server:
  addrs:
    - http: '0.0.0.0:40772'

channels:
  - name: conformance-a
    type: GR
    channel: '1'
  - name: conformance-b
    type: GR
    channel: '2'
  - name: conformance-c
    type: GR
    channel: '3'

tuners:
  - name: fixture-1
    types: [GR]
    command: %s
    decoded: true
  - name: fixture-2
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
`, tunerCommand, tunerCommand)
}

// mirakcContainer は起動済みの mirakc コンテナへのハンドルである。
type mirakcContainer struct {
	name    string
	baseURL string
}

// mirakcRunArgs は startMirakc が docker に渡す `run` 引数列を組み立てる。
func mirakcRunArgs(name, tunerBin, configPath, recBase, fixtureCase string) []string {
	args := []string{
		// --rm は起動直後に mirakc が終了した場合の診断ログまで消してしまう。
		// 成功時も失敗時も t.Cleanup の rm -f で回収する。
		"run", "-d",
		"--name", name,
		"-p", "127.0.0.1:0:40772",
		"-v", tunerBin + ":/fixtures/fixturetuner:ro",
		"-v", configPath + ":/etc/mirakc/config.yml:ro",
		"-v", recBase + ":/recordings",
	}
	if fixtureCase != "" {
		// tuners[].command は mirakc 自身が有効なコマンドか検証するため、
		// "VAR=value command" 形式を置けない。ケース切り替えはコンテナ環境へ渡す。
		args = append(args, "-e", "ROKUBAN_FIXTURE_CASE="+fixtureCase)
	}
	return append(args, configuredMirakcImage())
}

// startMirakc は mirakc 実物のコンテナを起こし、/api/version が応答するまで待つ。
// t.Cleanup でコンテナの停止・削除を登録する。
func startMirakc(t *testing.T, hostDir string, tunerBin string, fixtureCase string) *mirakcContainer {
	return startMirakcWithConfig(t, hostDir, tunerBin, fixtureCase,
		mirakcConfigYAML("/fixtures/fixturetuner"))
}

func startMirakcWithConfig(t *testing.T, hostDir string, tunerBin string, fixtureCase string, configYAML string) *mirakcContainer {
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
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config.yml: %v", err)
	}

	// t.Name() を混ぜて呼び出しごとに一意にする --- TestBroadcastPathologies は 1 バイナリ
	// から 4 回 startMirakc を呼ぶ（+ TestConformance の 1 回）。--rm を使わないので
	// （下記コメント参照）名前が衝突すると `docker rm -f` が別サブテストのコンテナを
	// 巻き込んで消す。PID だけが一意性の根拠だとサブテストが直列だから成り立っているに
	// すぎず、将来 t.Parallel() を足すと壊れる。
	name := fmt.Sprintf("rokuban-conformance-%s-%d", sanitizeContainerName(t.Name()), os.Getpid())
	// 既存の同名コンテナが残っていたら先に消す（前回異常終了時の掃除）。
	_ = exec.Command("docker", "rm", "-f", name).Run()

	cmd := exec.Command("docker", mirakcRunArgs(name, tunerBin, configPath, recBase, fixtureCase)...)
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

// sanitizeContainerName は t.Name() を docker のコンテナ名に使える文字列にする
// （t.Run のサブテスト名は "/" 区切りになる）。
func sanitizeContainerName(name string) string {
	return strings.NewReplacer("/", "-", " ", "-").Replace(name)
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
