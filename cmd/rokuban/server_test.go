package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/role"
	"github.com/fetburner/rokuban/internal/testutil"
	"github.com/fetburner/rokuban/internal/worker"
)

func writeServerTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// mirakcs: 1 要素の config は --sites 省略でそのサイトに束縛される
// （issue #183 の受け入れ基準 1。issue #444 で `mirakc:`（単数）糖衣を廃止したため、
// 単一サイト構成も `mirakcs:` 配列 1 要素で書く）。
func TestServerSiteBinding_SingleElementMirakcsRegistry_SitesOmitted(t *testing.T) {
	path := writeServerTestConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: home
    url: http://mirakc.local:40772
storage:
  media_dir: /mnt/media
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cmd := newSitesTestCmd(t)
	bound, err := resolveSiteBinding(cmd, cfg.Registry())
	if err != nil {
		t.Fatalf("resolveSiteBinding: %v", err)
	}
	if len(bound) != 1 || bound[0].Site != "home" || bound[0].URL != "http://mirakc.local:40772" {
		t.Errorf("bound = %+v, want [{home http://mirakc.local:40772}]", bound)
	}
}

// mirakcs: 2 要素 + --sites tokyo は tokyo だけに束縛される
// （issue #183 の受け入れ基準 2）。
func TestServerSiteBinding_MirakcsRegistry_BindsToNamedSite(t *testing.T) {
	path := writeServerTestConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
  - site: takamatsu
    url: http://mirakc-takamatsu:40772
storage:
  media_dir: /mnt/media
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cmd := newSitesTestCmd(t)
	if err := cmd.Flags().Set(siteFlagName, "tokyo"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	bound, err := resolveSiteBinding(cmd, cfg.Registry())
	if err != nil {
		t.Fatalf("resolveSiteBinding: %v", err)
	}
	if len(bound) != 1 || bound[0].Site != "tokyo" || bound[0].URL != "http://mirakc-tokyo:40772" {
		t.Errorf("bound = %+v, want [{tokyo http://mirakc-tokyo:40772}]", bound)
	}
}

// mirakcs: 2 要素 + --sites 省略は起動エラーになる（issue #183 の受け入れ基準 3）。
func TestServerSiteBinding_MirakcsRegistry_UnspecifiedSitesIsError(t *testing.T) {
	path := writeServerTestConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
  - site: takamatsu
    url: http://mirakc-takamatsu:40772
storage:
  media_dir: /mnt/media
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cmd := newSitesTestCmd(t)
	if _, err := resolveSiteBinding(cmd, cfg.Registry()); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// `mirakc:`（単数）は issue #444 で廃止した糖衣。旧キーを書いた config は struct に
// 対応するフィールドが無いため、strict パースの未知キー検出で Load 自体が
// 起動エラーにする（issue #183 の受け入れ基準 4 の後継。当時の理由は相互排他
// だったが、糖衣の廃止後は unknown field が理由になる）。
func TestServerSiteBinding_LegacyMirakcKey_LoadFails(t *testing.T) {
	path := writeServerTestConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
storage:
  media_dir: /mnt/media
`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// runServerCmdForTest は server サブコマンドの RunE を実際に走らせる。
//
// **単体の resolve* / validate* を直接呼ぶテストでは RunE の配線が検証できない**
// （解決した値を渡し忘れて config を直接見ていても、resolveWorkerQueues の
// 単体テストは通り続ける。CLAUDE.md「壊す場所を、実際に壊れる経路の上に置く」）。
// DB を到達不能にしてあるので、起動検査を通った場合は
// 「connecting to database」で落ちる --- どこまで進んだかがエラーの種類で分かる。
func runServerCmdForTest(t *testing.T, configPath string, args ...string) error {
	t.Helper()
	return runServerCmdWithContext(t, context.Background(), configPath, args...)
}

// runServerCmdWithContext は ctx を RunE に渡して server サブコマンドを走らせる。
// ctx を cancel するとサーバーは SIGTERM と同じ経路で畳む。
func runServerCmdWithContext(t *testing.T, ctx context.Context, configPath string, args ...string) error {
	t.Helper()
	cmd := newServerCmd()
	// 本番では root の PersistentFlags から来る（cmd/rokuban/main.go）。
	cmd.Flags().String("config", configPath, "")
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd.ExecuteContext(ctx)
}

// 到達不能な DB（localhost:1）+ 2 サイトのレジストリ。
const serverCmdTestConfig = `
db:
  host: 127.0.0.1
  port: 1
  user: rokuban
  password: secret
  database: rokuban
mirakcs:
  - site: tokyo
    url: http://mirakc-tokyo:40772
  - site: takamatsu
    url: http://mirakc-takamatsu:40772
storage:
  media_dir: /mnt/media
`

// **--queues が RunE の配線に載っていること。** 中央（0 サイト束縛）の worker は
// site 非依存キューに絞ったときだけ起動できる（validateSiteBinding）。
// その判定が config だけを見ていると、`--queues=encode` を渡しても
// 「全キュー購読の中央 worker」と見なされて起動できない --- KEDA の
// encode ScaledJob（`--sites=`）が起動すらしないという形で現れる。
//
// 両方向を見る: --queues=encode なら DB まで到達し、--queues 無しなら
// site 束縛の検査で（DB に触る前に）落ちる。
func TestServerCmd_QueuesFlagUnblocksCentralEncodeWorker(t *testing.T) {
	path := writeServerTestConfig(t, serverCmdTestConfig)

	err := runServerCmdForTest(t, path, "--roles", "worker", "--sites=", "--queues=encode")
	if err == nil {
		t.Fatal("到達不能な DB を指しているので error を期待したが nil だった")
	}
	if !strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v, want to fail at the DB (= 起動検査を通ったこと)", err)
	}

	// 反対方向: --queues 無しの中央 worker は従来どおり起動エラー。
	err = runServerCmdForTest(t, path, "--roles", "worker", "--sites=")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--sites") {
		t.Errorf("err = %v, want the site-binding error (DB に触る前に落ちること)", err)
	}
	if strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v: DB まで進んでいる（site 束縛の検査が効いていない）", err)
	}
}

// **--soft-stop-timeout の検査が RunE の配線に載っていること**（ロール検査が
// DB より前に効く）。
//
// `resolveSoftStopTimeout` の単体テスト（queues_test.go）は「呼べば弾く」しか
// 主張しない。**戻り値のエラーを握り潰す変異はそれでも緑になる**（実測: RunE を
// `softStopTimeout, _ := resolveSoftStopTimeout(...)` にする変異は、この
// テストを足す前は cmd/rokuban 全体が緑のままだった）。そのとき
// `--roles watcher --soft-stop-timeout 5m` は黙って無視され、drain するつもりの
// Pod が drain しない。`--queues` / `--once` が同じ形のテストを持っているのに、
// このフラグだけ持っていなかった。
func TestServerCmd_SoftStopTimeoutRequiresWorkerRoleInRunE(t *testing.T) {
	path := writeServerTestConfig(t, serverCmdTestConfig)

	err := runServerCmdForTest(t, path,
		"--roles", "watcher", "--sites", "tokyo", "--soft-stop-timeout", "5m")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), softStopTimeoutFlagName) {
		t.Errorf("err = %v, want the --%s role error", err, softStopTimeoutFlagName)
	}
	if strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v: DB まで進んでいる（検査が効いていない）", err)
	}

	// 反対方向: worker ロールなら検査を通り、DB まで到達する。
	err = runServerCmdForTest(t, path,
		"--roles", "worker", "--sites", "tokyo", "--soft-stop-timeout", "5m")
	if err == nil {
		t.Fatal("到達不能な DB を指しているので error を期待したが nil だった")
	}
	if !strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v, want to fail at the DB (= 起動検査を通ったこと)", err)
	}
}

// **--once が RunE の配線に載っていること**（ロール検査が DB より前に効く）。
func TestServerCmd_OnceRejectsExtraRoles(t *testing.T) {
	path := writeServerTestConfig(t, serverCmdTestConfig)

	err := runServerCmdForTest(t, path, "--roles", "worker,api", "--sites", "tokyo", "--once")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--once") {
		t.Errorf("err = %v, want the --once role error", err)
	}
	if strings.Contains(err.Error(), "connecting to database") {
		t.Errorf("err = %v: DB まで進んでいる（--once のロール検査が効いていない）", err)
	}
}

// writeOnceModeConfig は 1 件消化モードのテスト用 config を書く（実 DB を指す）。
//
// user / password が空のときにダミーを埋めるのは、ローカルの
// ROKUBAN_TEST_DATABASE_URL が資格情報を持たない（trust 認証）ことがある一方で
// config.DBConfig が両方を required にしているため。CI の URL は
// `postgres://rokuban:rokuban@...` なのでこの分岐は通らない。
//
// **log.format はここで上書きしない。** 運用者が実際に読むのは既定の json
// （config.example.yml / k8s の overlay がどちらも format: json）。ここで
// text に固定すると、assertOnceOutcome の契約検査が製品が出さない符号化に
// 対してしか成立しなくなる。
func writeOnceModeConfig(t *testing.T, extra ...string) string {
	t.Helper()
	// 到達不能な mirakc（127.0.0.1:1）。1 件消化モードのテストは mirakc に
	// 触らないジョブを使うか、触って失敗することを主張するかのどちらかである。
	return writeWorkerTestConfig(t, "http://127.0.0.1:1", extra...)
}

// writeWorkerTestConfig は worker ロールのテスト用 config を書く（実 DB を指す）。
// mirakcURL を差し替えられるので、mirakc に触るジョブを実際に完走させられる。
func writeWorkerTestConfig(t *testing.T, mirakcURL string, extra ...string) string {
	t.Helper()
	dbCfg := testutil.DatabaseConfig(t)
	user := dbCfg.User
	if user == "" {
		user = os.Getenv("USER")
	}
	password := dbCfg.Password
	if password == "" {
		password = "unused"
	}
	return writeServerTestConfig(t, fmt.Sprintf(`
server:
  listen: "127.0.0.1:0"
  allowed_hosts: []
db:
  host: %q
  port: %d
  user: %q
  password: %q
  database: %q
  sslmode: %q
mirakcs:
  - site: home
    url: %q
storage:
  media_dir: %q
worker:
  periodic_jobs: false
%s`, dbCfg.Host, dbCfg.Port, user, password, dbCfg.Database, dbCfg.SSLMode, mirakcURL, t.TempDir(),
		strings.Join(extra, "\n")))
}

// syncBuffer は複数 goroutine から書かれるログを集める。サーバーは
// runServerCmdBounded の中で別 goroutine が動かすので、素の bytes.Buffer だと
// -race が拾う。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureServerLogs は os.Stderr を差し替えてサーバーのログを集める（loadConfig
// が config の log.* から slog.SetDefault するので、先回りで SetDefault しても
// runServer 側に上書きされてしまう。cmd/rokuban/stderr_capture_test.go 参照）。
//
// **outcome のラベルは docs / e2e README / runbook が名指ししている契約**
// （outcome 属性が `idle_timeout` / `job_done` / `job_unhandled` になることを
// 運用側が読み分ける）。ラベルを見ないと、「終了した」だけを見るテストは
// **でっち上げの理由で終了した**場合も緑になる --- 実際に `unsubscribe` の
// 呼び出し位置を 1 行ずらす変異では、3 本のうち 2 本が緑のまま
// `outcome=job_unhandled` を出していた。
func captureServerLogs(t *testing.T) func() string {
	t.Helper()
	return captureStderr(t, &syncBuffer{})
}

// assertOnceOutcome は 1 件消化モードの終了理由がログに出ていることを見る。
// logs は captureServerLogs が返す読み出し関数（drain してから読む。
// stderr_capture_test.go 参照）。
//
// **json（既定の log.format）の書式で見る。** writeOnceModeConfig / captureServerLogs
// の doc コメント参照 --- 運用者が実際に読む形式に対して契約を検査する。
func assertOnceOutcome(t *testing.T, logs func() string, want string) {
	t.Helper()
	if got := logs(); !strings.Contains(got, `"outcome":"`+want+`"`) {
		t.Errorf("ログに \"outcome\":\"%s\" が無い。ログ:\n%s", want, got)
	}
}

// runServerCmdBounded は server の RunE を走らせ、**有限時間で戻ること**まで見る。
//
// 1 件消化モードの主張はまさに「プロセスが終わる」ことなので、戻らない変異は
// go test 全体のタイムアウト（panic ダンプ）ではなく、このテストの失敗として
// 報告する。
func runServerCmdBounded(t *testing.T, limit time.Duration, configPath string, args ...string) (time.Duration, error) {
	t.Helper()
	// **打ち切るときはサーバーを畳んでから返る。** goroutine を残すと、
	// River クライアントが cleanup キューを購読したまま生き続け、後続のテストが
	// 入れたジョブを掴んだり testutil.SetupDB の TRUNCATE と競合したりして、
	// 1 件の失敗が連鎖して元の原因を隠す。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	start := time.Now()
	go func() { errCh <- runServerCmdWithContext(t, ctx, configPath, args...) }()
	select {
	case err := <-errCh:
		return time.Since(start), err
	case <-time.After(limit):
		cancel()
		select {
		case <-errCh:
		case <-time.After(30 * time.Second):
			t.Error("cancel してもサーバーが畳まれない（River クライアントが残る）")
		}
		t.Fatalf("server が %s 以内に終了しない（--once の Job が終わらない形）", limit)
		return 0, nil
	}
}

// **1 件消化モードのプロセスが実際に終了すること。** 判定 2.2 / 2.4 が要求する
// 「0 → 1 → 0」の 0 に戻る側そのもので、これが無いと KEDA が起こした Job は
// succeeded に到達しない。
//
// 実 DB を使って RunE を丸ごと走らせるのが要点 --- OnceGate の単体テストは
// gate の論理しか見ておらず、**gate を River に登録して待つ配線**（server.go の
// eg.Go）を検証できない。
//
// 両方向を見る:
//   - 空キュー: --once-idle-timeout で終了する
//   - 1 件入っている: そのジョブを消化して終了し、ジョブが completed になる
func TestServerCmd_OnceModeTerminates(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	path := writeOnceModeConfig(t)

	onceArgs := []string{"--roles", "worker", "--sites=", "--queues=cleanup", "--once"}

	t.Run("空キューなら idle timeout で終了する", func(t *testing.T) {
		const idle = 2 * time.Second
		logs := captureServerLogs(t)
		args := append(append([]string{}, onceArgs...), "--once-idle-timeout="+idle.String())
		elapsed, err := runServerCmdBounded(t, 30*time.Second, path, args...)
		if err != nil {
			t.Fatalf("server --once: %v", err)
		}
		assertOnceOutcome(t, logs, "idle_timeout")
		// **待った時間も見る。** 「戻った」だけを見ると、idle timeout を
		// 無視して即終了する変異（掴む前に畳む = ジョブを取りこぼす形）でも通る。
		if elapsed < idle {
			t.Errorf("elapsed = %s, want >= %s（指定した idle timeout を待っていない）", elapsed, idle)
		}
	})

	t.Run("1 件あれば消化して終了する", func(t *testing.T) {
		var out bytes.Buffer
		if err := runEnqueue(ctx, pool, "delete-reconcile", "", &out); err != nil {
			t.Fatalf("runEnqueue: %v", err)
		}

		// **idle timeout を長く取る。** 短いと「消化して終わった」と
		// 「掴めないまま時間切れで終わった」が区別できず、空虚な成功になる。
		logs := captureServerLogs(t)
		args := append(append([]string{}, onceArgs...), "--once-idle-timeout=60s")
		if _, err := runServerCmdBounded(t, 30*time.Second, path, args...); err != nil {
			t.Fatalf("server --once: %v", err)
		}
		assertOnceOutcome(t, logs, "job_done")

		var state string
		if err := pool.QueryRow(ctx,
			`SELECT state FROM river_job WHERE kind = 'delete_reconcile'`,
		).Scan(&state); err != nil {
			t.Fatalf("reading delete_reconcile state: %v", err)
		}
		if state != "completed" {
			t.Errorf("delete_reconcile state = %q, want %q（消化せずに終わっている）", state, "completed")
		}
	})
}

type e2eProbeArgs struct{}

func (e2eProbeArgs) Kind() string { return "e2e_probe" }

// TestServerCmd_OnceModeExitsOnUnhandledJobKind は**未登録 kind のジョブを
// 掴んでも Job が終わること**を確認する。River の executor は登録されていない
// kind を WorkUnit == nil で早期 return し、その時点では WorkerMiddleware の
// チェーンをまだ組み立てていない（worker.SubscribeOnceEvents）。
// middleware だけを見ていると、Job は「1 件も claim していない」と誤認したまま
// idleTimeout の間そのキューを掴み続け、試行回数を潰す。
//
// 版ずれ（新しいイメージの CronJob / api が、古い worker の知らない kind を
// 投入する）で起きうるほか、**受け入れ判定ハーネスの `insert_probe_job` が
// 使う `e2e_probe` も未登録 kind** である（deploy/k8s/e2e/lib/kube.sh）。
//
// 判定は 2 つ。**どちらも「戻った」だけでは足りない** --- 壊れていても
// idleTimeout 後には戻る。(a) idleTimeout（60s）より十分短い時間で戻ること
// （runServerCmdBounded の limit 20s がこれを担う）、(b) 試行を 1 回しか
// 潰していないこと（壊れていると窓の中で何度も掴み直す）。
type e2eProbeArgs struct{}

func (e2eProbeArgs) Kind() string { return "e2e_probe" }

func TestServerCmd_OnceModeExitsOnUnhandledJobKind(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	path := writeOnceModeConfig(t)

	// 製品の投入経路には無い kind だが、insert-only client の公開 API を使って
	// 入れる。Workers == nil の client は未登録 kind の検査をスキップする。
	// **max_attempts はハーネスの 1 ではなく 25 にする** ---
	// 1 だと最初の失敗で discarded になり、壊れた実装でも掴み直せないので
	// 下の attempt の主張が何も検出しなくなる。
	insertClient, err := worker.NewInsertOnlyClient(pool)
	if err != nil {
		t.Fatalf("creating insert-only river client: %v", err)
	}
	if _, err := insertClient.Insert(ctx, e2eProbeArgs{}, &river.InsertOpts{Queue: "cleanup", MaxAttempts: 25}); err != nil {
		t.Fatalf("inserting unhandled-kind job: %v", err)
	}

	// **idleTimeout（60s）より短い limit（20s）で打ち切ることが判定 (a) 本体。**
	// これを超えると runServerCmdBounded が Fatalf で落ちるので、ここに
	// elapsed の比較を足しても到達しない（何も主張しない 3 行になる）。
	logs := captureServerLogs(t)
	_, err = runServerCmdBounded(t, 20*time.Second, path,
		"--roles", "worker", "--sites=", "--queues=cleanup", "--once", "--once-idle-timeout=60s")
	if err != nil {
		t.Fatalf("server --once: %v", err)
	}
	assertOnceOutcome(t, logs, "job_unhandled")

	var attempt int
	var state string
	if err := pool.QueryRow(ctx,
		`SELECT attempt, state FROM river_job WHERE kind = 'e2e_probe'`,
	).Scan(&attempt, &state); err != nil {
		t.Fatalf("reading e2e_probe job: %v", err)
	}
	if attempt != 1 {
		t.Errorf("attempt = %d, want 1（同じ Job が掴み直して試行回数を潰している。state=%s）", attempt, state)
	}
}

// **ffmpeg/ffprobe の存在検査が `--queues` に従うこと。** 検査が config の
// `worker.queues` を見ていると、argv だけで絞った Pod（k8s の ScaledJob が
// まさにこの形）に対して常に ffmpeg を要求し、**公式イメージ（ffmpeg 非同梱）の
// worker が起動できなくなる**（`RequiresEncodeTools([])` は true）。
//
// 両方向を見る: `--queues=cleanup` は検査を通り、`--queues=encode` は落ちる。
func TestServerCmd_EncodeToolCheckFollowsQueuesFlag(t *testing.T) {
	testutil.SetupDB(t)
	path := writeOnceModeConfig(t, `encode:
  ffmpeg: /nonexistent/rokuban-test-ffmpeg
  ffprobe: /nonexistent/rokuban-test-ffprobe`)

	if _, err := runServerCmdBounded(t, 30*time.Second, path,
		"--roles", "worker", "--sites=", "--queues=cleanup", "--once", "--once-idle-timeout=1s"); err != nil {
		t.Fatalf("--queues=cleanup は ffmpeg を要求しないこと: %v", err)
	}

	_, err := runServerCmdBounded(t, 30*time.Second, path,
		"--roles", "worker", "--sites=", "--queues=encode", "--once", "--once-idle-timeout=1s")
	if err == nil {
		t.Fatal("--queues=encode は ffmpeg の不在で落ちること: error を期待したが nil だった")
	}
	if !strings.Contains(err.Error(), "encode.ffmpeg") {
		t.Errorf("err = %v, want the ffmpeg tool error", err)
	}
}

// **失敗するジョブでもプロセスは exit 0。** リトライは River が持っており、
// k8s 側は backoffLimit: 0 / restartPolicy: Never で起こし直さない前提なので、
// 終了コードで失敗を表すと二重にリトライする形になる。
//
// 到達不能な mirakc への epg_sync で失敗させる（RunE を丸ごと通すので、
// 「gate goroutine が nil を返す」だけでなく errgroup の畳み方も含めて見る）。
func TestServerCmd_OnceModeExitsZeroOnJobFailure(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	path := writeOnceModeConfig(t)

	var out bytes.Buffer
	if err := runEnqueue(ctx, pool, "epg-sync", "home", &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}

	// config の mirakc は 127.0.0.1:1（接続不能）なので epg_sync は必ず失敗する。
	_, err := runServerCmdBounded(t, 30*time.Second, path,
		"--roles", "worker", "--sites", "home", "--queues=epg", "--once", "--once-idle-timeout=60s")
	if err != nil {
		t.Fatalf("ジョブが失敗しても exit 0 であること: %v", err)
	}

	// ジョブは完了扱いにならず、River 側に再試行として残る。
	var state string
	var attempt int
	if err := pool.QueryRow(ctx,
		`SELECT state, attempt FROM river_job WHERE kind = 'epg_sync'`,
	).Scan(&state, &attempt); err != nil {
		t.Fatalf("reading epg_sync job: %v", err)
	}
	if state == "completed" {
		t.Error("epg_sync が completed になっている（失敗が握り潰されている）")
	}
	if attempt != 1 {
		t.Errorf("attempt = %d, want 1", attempt)
	}
}

// **プロセスが `riverClient.Stop` を待つ上限は、必ず soft stop の猶予より
// 長いこと。**
//
// 短いと、猶予の内側で完走するはずのジョブが「プロセスだけ先に抜ける」ことで
// 打ち切られる。しかもその打ち切りは ctx の cancel ですらない（プロセスが
// 終わるだけ）ので、行は `running` のまま残り、回収は `JobRescuer` に委ねる
// ことになる --- ロール分割構成ではそれを動かす常駐クライアントが無い
// （shutdownBudget のコメント）。
//
// **E2E（TestServerCmd_SigtermDrainsRunningJob）ではこれを測れない**: 猶予より
// 長く走るジョブを実際に走らせる必要があり、テストの所要が猶予そのものになる。
// 代わりにここで関係だけを固定する。期待値は実装の定数を参照せずリテラルで
// 書く（参照すると両方を同時に変えたときに何も主張しなくなる）。
//
// 実測: `return 30 * time.Second`（この PR が置き換えた固定値）に戻す変異で
// 4 行とも赤くなることを確認した。
func TestShutdownBudget_CoversTheSoftStop(t *testing.T) {
	for _, soft := range []time.Duration{time.Second, 30 * time.Second, 60 * time.Second, 6 * time.Hour} {
		if got := shutdownBudget(soft); got <= soft {
			t.Errorf("shutdownBudget(%s) = %s, want > %s（猶予の内側の drain をプロセスが先に抜けて打ち切る）",
				soft, got, soft)
		}
	}
	// 既定（--soft-stop-timeout 5s）での値。**docs/operations.md §5 の
	// `terminationGracePeriodSeconds` の足し算と deploy 側の数値がこれに
	// 依存している**ので、変えるときは同じ PR で揃える。
	if got := shutdownBudget(5 * time.Second); got != 15*time.Second {
		t.Errorf("shutdownBudget(5s) = %s, want 15s", got)
	}
}

// **HTTP の停止予算の値を固定する。**
//
// この定数はプロセスの停止予算の 1 項であり、k8s の
// `terminationGracePeriodSeconds` に書く数値の内訳に入っている
// （docs/operations.md §5「Deployment 併用時」の足し算）。**マニフェスト側の
// テストはこれを検出できない** --- `deploy/k8s/manifests_test.go` は同じ 10 を
// リテラルで持っており（実装の定数を参照すると両方を同時に変えたときに何も
// 主張しなくなるため）、こちらを 5 分にする変異は cmd/rokuban も deploy/k8s も
// 緑のままだった（実測）。そのとき api Pod は猶予 30 秒の途中で SIGKILL される。
//
// リテラルで書くのは「値が正しい」ことの主張ではなく、**この定数を変える人を
// deploy/k8s と api.yaml の猶予に立ち寄らせる**ためである。
func TestHTTPShutdownTimeout_IsPinnedToTheManifestBudget(t *testing.T) {
	if httpShutdownTimeout != 10*time.Second {
		t.Errorf("httpShutdownTimeout = %s, want 10s（変えるなら deploy/k8s/manifests_test.go の "+
			"リテラルと deploy/k8s/base/api.yaml の terminationGracePeriodSeconds、"+
			"docs/operations.md §5 の足し算も同じ PR で揃えること）", httpShutdownTimeout)
	}
}

// **`riverClient.Stop` に渡す締切が、実際に soft stop の猶予から導かれている
// こと。** shutdownBudget が正しくても、呼び出し側が固定値を渡していれば
// 何の意味も無い --- 元の実装がまさに固定の 30 秒だった。
//
// **E2E では掛からない変異である**（TestServerCmd_SigtermDrainsRunningJob は
// 猶予を 2 秒 / 60 秒で回すので、2 秒以上のどんな固定値でも緑になる）。実害が
// 出るのは docs が encode に指示している `--soft-stop-timeout=6h` のような構成で、
// そこでは固定値が drain を黙って打ち切る。だから固定値では通らない大きさ
// （6 時間）で見る。
//
// 実測: `stopRiverForShutdown` の締切を `30*time.Second` に戻す変異で赤くなる
// ことを確認した。
func TestStopRiverForShutdown_DeadlineFollowsSoftStopTimeout(t *testing.T) {
	const soft = 6 * time.Hour

	var deadline time.Time
	var hasDeadline bool
	stopRiverForShutdown(func(stopCtx context.Context) error {
		deadline, hasDeadline = stopCtx.Deadline()
		return nil
	}, soft)

	if !hasDeadline {
		t.Fatal("Stop に締切の無い ctx が渡っている（畳めないワーカーでプロセスが終わらなくなる）")
	}
	if remaining := time.Until(deadline); remaining <= soft {
		t.Errorf("Stop の締切まで %s, want > %s（猶予の内側の drain をプロセスが先に抜けて打ち切る）",
			remaining, soft)
	}
}

// blockingMirakc は「掴まれたことが分かり、いつ応答するかをテストが決められる」
// mirakc のモック。SIGTERM を受けた瞬間にジョブが**実行中**であることを保証する
// ために要る --- 時間で待つと、ジョブが既に終わっていても緑になる（空虚な成功）。
type blockingMirakc struct {
	url string
	// hit は最初の GET /api/services で閉じる。ジョブが mirakc に到達した
	// ＝ River が Work に入っていることの観測点。
	hit chan struct{}
	// release を閉じると /api/services が空リストを返して epg_sync が完走する。
	release chan struct{}
}

func newBlockingMirakc(t *testing.T) *blockingMirakc {
	t.Helper()
	m := &blockingMirakc{hit: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/services" {
			once.Do(func() { close(m.hit) })
			select {
			case <-m.release:
			case <-r.Context().Done():
				// ジョブの ctx が切れるとクライアントが接続を切る。
				// エスカレート側のテストはここを通る。
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

// workerProc は startWorkerWithRunningJob が起こした server プロセス。
type workerProc struct {
	// terminate は SIGTERM と同じ経路（signal.NotifyContext に渡した ctx の
	// cancel）でプロセスを畳む。
	terminate func()

	finished chan struct{} // RunE が戻ったら閉じる
	err      error         // finished が閉じたあとだけ読んでよい
}

// wait は RunE が戻るのを待ち、その戻り値を返す。limit を超えたら失敗させる
// （戻らない変異を go test 全体のタイムアウトではなくこのテストの失敗として
// 報告する。runServerCmdBounded と同じ方針）。
//
// **複数回呼べる形にしてある。** t.Cleanup も同じ待ちをするので、
// 「受け取ったら消える」チャネルにすると本体が受け取ったぶんだけ
// cleanup 側が待ちぼうけになる。
func (p *workerProc) wait(t *testing.T, limit time.Duration, what string) error {
	t.Helper()
	select {
	case <-p.finished:
		return p.err
	case <-time.After(limit):
		t.Fatalf("%s: server が %s 以内に終了しない", what, limit)
		return nil
	}
}

// startWorkerWithRunningJob は epg_sync を 1 件投入した worker を起動し、
// **そのジョブが実行中になるまで待ってから**戻る。
func startWorkerWithRunningJob(t *testing.T, pool *pgxpool.Pool, mock *blockingMirakc, softStop time.Duration) *workerProc {
	t.Helper()
	var out bytes.Buffer
	if err := runEnqueue(context.Background(), pool, "epg-sync", "home", &out); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}
	path := writeWorkerTestConfig(t, mock.url)

	ctx, cancelFn := context.WithCancel(context.Background())
	p := &workerProc{terminate: cancelFn, finished: make(chan struct{})}
	go func() {
		defer close(p.finished)
		p.err = runServerCmdWithContext(t, ctx, path,
			"--roles", "worker", "--sites", "home", "--queues=epg",
			"--soft-stop-timeout="+softStop.String())
	}()
	// 主張が成立しなかったときに River クライアントを残さない。残すと
	// 後続のテストが入れたジョブを掴んだり testutil.SetupDB の TRUNCATE と
	// 競合したりして、1 件の失敗が連鎖して元の原因を隠す。
	t.Cleanup(func() {
		cancelFn()
		select {
		case <-p.finished:
		case <-time.After(60 * time.Second):
			t.Error("cancel してもサーバーが畳まれない（River クライアントが残る）")
		}
	})

	select {
	case <-mock.hit:
	case <-p.finished:
		t.Fatalf("epg_sync が走り出す前に server が終了した: %v", p.err)
	case <-time.After(60 * time.Second):
		t.Fatal("epg_sync が mirakc に到達しない（ジョブが実行中にならない）")
	}
	return p
}

// **SIGTERM が drain であること（実行中のジョブを打ち切らないこと）。**
//
// これが無いと、Deployment 型 worker のローリング更新・ノード退避が実行中の
// エンコード（数時間）を打ち切る。症状は「デプロイしたらエンコードがやり直しに
// なる」。River は `SoftStopTimeout` が未設定だと work ctx を start ctx から
// 継ぐので、`signal.NotifyContext` の ctx を `Start` に渡しているこの構成では
// **SIGTERM がそのまま StopAndCancel 相当になる**（river@v0.47.0 client.go の
// workParentCtx）。
//
// RunE を丸ごと走らせるのが要点 --- `--soft-stop-timeout` → `worker.ClientConfig`
// → `river.Config` の配線は、`buildRiverConfig` を直接見るテストでは検証できない
// （フラグを ClientConfig に渡し忘れても、既定値で組まれた client がそこにある）。
//
// **`riverClient.Stop` に与える待ちの上限（shutdownBudget）はここでは測れない。**
// 猶予より長い時間走るジョブを実際に走らせることになるので、テストの所要が
// 猶予そのものになる。上限が猶予を包むことは TestShutdownBudget_CoversTheSoftStop
// が別に固定し、実バイナリでの確認は docs/runbook/ に残す。
//
// **両方向を見る。** 猶予の内側なら完走し、猶予を超えたらエスカレートする
// （＝待ちっぱなしにはならない）。
//
// **猶予切れの打ち切りを River は「ジョブのエラー」として記録しない。** 停止に
// よる cancel（cause が `rivercommon.ErrStop`）を検出すると、`AttemptError` を
// 組み立てず `JobSetStateInterrupted`（state=available、attempt は
// `max(attempt-1, 0)`）で戻す（river@v0.47.0 internal/jobexecutor/job_executor.go
// の isSoftStopCancelError と `if softStopped` の早期 return）。**v0.44 で
// 変わった**（それ以前は attempt を潰して `errors` に
// `listing services: … : stop initiated` が残ったので、このテストの
// エスカレート側はそれを読んでいた）。**この差は退行ではない**（打ち切りが
// `max_attempts` を削らなくなった）ので、テストの側を新しい帳簿に合わせる ---
// 見るのは `state=available` と `attempt=0` である。
//
// 実測（変異を注入して赤くなることを確認済み。river@v0.47.0）:
//   - `river.Config.SoftStopTimeout` を 0 に戻す（この issue 以前の形）: 完走側が
//     `state=available` / `error="… context canceled"` で赤。エスカレート側も
//     「SIGTERM から終了まで 2.1〜2.6ms」と `attempt = 1` で赤（3 回）
//   - `ClientConfig.SoftStopTimeout` の代入を消す（フラグの配線落ち）:
//     エスカレート側が 5.0 秒（上限判定 3.5 秒）で赤
func TestServerCmd_SigtermDrainsRunningJob(t *testing.T) {
	// 実行中のジョブが SIGTERM の後に終わることを主張するので、cancel の
	// あと**実際に時間を進めてから**完走させる。ハードストップ時の打ち切りは
	// 実測 2.6ms なので、この 1 秒は 2 桁以上の余裕がある。
	const drainDelay = time.Second

	t.Run("猶予の内側なら実行中のジョブが完走する", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		mock := newBlockingMirakc(t)
		p := startWorkerWithRunningJob(t, pool, mock, 60*time.Second)

		p.terminate() // = SIGTERM
		time.Sleep(drainDelay)
		close(mock.release)

		if err := p.wait(t, 60*time.Second, "SIGTERM 後"); err != nil {
			t.Fatalf("server: %v", err)
		}

		var state string
		var errs string
		if err := pool.QueryRow(context.Background(),
			`SELECT state, coalesce(errors::text, '') FROM river_job WHERE kind = 'epg_sync'`,
		).Scan(&state, &errs); err != nil {
			t.Fatalf("reading epg_sync job: %v", err)
		}
		if state != "completed" {
			t.Errorf("state = %q, want %q（SIGTERM が実行中のジョブを打ち切っている）。errors=%s",
				state, "completed", errs)
		}
	})

	t.Run("猶予を超えたらエスカレートする", func(t *testing.T) {
		pool := testutil.SetupDB(t)
		mock := newBlockingMirakc(t)
		// mock.release は閉じない。ジョブは猶予を超えても終わらない。
		const softStop = 2 * time.Second
		p := startWorkerWithRunningJob(t, pool, mock, softStop)

		start := time.Now()
		p.terminate() // = SIGTERM
		if err := p.wait(t, 60*time.Second, "猶予切れ（drain が無制限になっている）"); err != nil {
			t.Fatalf("server: %v", err)
		}
		elapsed := time.Since(start)

		// **待った側も見る。** 「終わった」だけを見ると、SIGTERM で即座に
		// 打ち切る（＝この issue が直そうとしている壊れ方そのもの）でも通る。
		if elapsed < softStop {
			t.Errorf("SIGTERM から終了まで %s、want >= %s（猶予を待たずに打ち切っている）", elapsed, softStop)
		}
		// **上限も見る。** 下限だけだと、`--soft-stop-timeout` が River に
		// 渡っておらず既定値（worker.DefaultSoftStopTimeout = 5 秒）で
		// 待っている場合も通ってしまう --- フラグの配線を落とす変異が
		// そのまま緑になる。
		//
		// **判別する 2 つの値が近いので、線の引き方に余裕が無い。** 正しい実装は
		// 猶予（2 秒）ちょうどで戻り（実測 2.0034〜2.0114 秒）、配線を落とす変異は
		// 既定値の 5 秒で戻る。線は 3.5 秒（= 2 秒 + 1.5 秒）に引いてある。
		// **既定値を変えるならここも見直すこと** --- 既定が 2 秒に近づくと、
		// この判定は何も主張しなくなる。
		if margin := 1500 * time.Millisecond; elapsed > softStop+margin {
			t.Errorf("SIGTERM から終了まで %s、want < %s（--%s が River に渡っていない可能性）",
				elapsed, softStop+margin, softStopTimeoutFlagName)
		}

		// **`attempted_at IS NOT NULL` で絞る。** `available` / `attempt=0` は
		// **一度も claim されていない行と同じ形**なので、これが無いと
		// 「ジョブが起きなかった」と区別が付かない --- 旧アサーション
		// （`errors` の中身）が担っていたのはこの区別である。claim 自体は
		// startWorkerWithRunningJob が mock への到達で待っているが、それは
		// 「この行を読んだ」ことの保証にはならない（`epg_sync` の行が 2 本に
		// なった瞬間に別の行を読んで緑になる）。`attempted_at` は claim の
		// ときだけ書かれ、`JobSetStateInterrupted` は触らないので
		// （riverdriver@v0.47.0 river_driver_interface.go の JobSetStateInterrupted が
		// AttemptedAt を設定せず、completer が撃つクエリ
		// riverpgxv5@v0.47.0 internal/dbsqlc/river_job.sql の
		// `JobSetStateIfRunningMany` は job_input にも SET 句にも
		// `attempted_at` 列を持たない --- 同区間の `cancel_attempted_at` は
		// metadata のキー名で別物）、**実際に worked された行である**ことの
		// 肯定形の主張になる。
		var state string
		var attempt int
		var errs string
		if err := pool.QueryRow(context.Background(),
			`SELECT state, attempt, coalesce(errors::text, '') FROM river_job
			 WHERE kind = 'epg_sync' AND attempted_at IS NOT NULL`,
		).Scan(&state, &attempt, &errs); errors.Is(err, pgx.ErrNoRows) {
			// 失敗メッセージ単体で読めるようにする（`no rows in result set`
			// だけだと、上のフィルタの意図を読まないと分からない）。
			t.Fatal("worked された epg_sync の行が無い（ジョブが起きなかった）")
		} else if err != nil {
			t.Fatalf("reading the worked epg_sync job: %v", err)
		}
		// **River が帳簿を書き終えてから畳んでいることも見る。** プロセスが
		// completer の flush を待たずに抜けると行は `running` のまま残り、
		// 回収は JobRescuer（既定 1 時間。ロール分割構成では動かす常駐
		// クライアントが無い）に委ねられる。
		if state != "available" {
			t.Errorf("state = %q, want %q（猶予切れの打ち切りが available に戻っていない）。attempt=%d errors=%s",
				state, "available", attempt, errs)
		}
		// **試行を消費していないことを見る。** ここは River v0.44 で変わった
		// （この関数の doc コメント）。消費する形に戻ると、猶予切れの打ち切りが
		// `max_attempts` を削り、ローリング更新のたびに再試行の余地が減る。
		if attempt != 0 {
			t.Errorf("attempt = %d, want 0（停止による打ち切りが試行を消費している）。errors=%s", attempt, errs)
		}
	})
}

// resolveRiverClientKind はロール指定が実際に実行する仕事を制約するための唯一の
// 分岐点。ここが誤ると、--roles watcher 単独のプロセスが worker.NewWorkers の
// フルのワーカー群（EncodeWorker/ThumbnailWorker を含む）を登録し、ffmpeg/ffprobe
// を検査しないまま encode/thumbnail ジョブを実行しうる（不変条件 4 違反、issue #113）。
func TestResolveRiverClientKind(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		want  riverClientKind
	}{
		{"watcher alone must not get the full worker set", []string{"watcher"}, riverClientInsertOnly},
		{"worker alone", []string{"worker"}, riverClientFull},
		{"worker and watcher together (monolith) keep the shared full client", []string{"worker", "watcher"}, riverClientFull},
		{"watcher and worker together regardless of order", []string{"watcher", "worker"}, riverClientFull},
		{"api alone needs no river client here (uses its own insert-only client)", []string{"api"}, riverClientNone},
		{"streamer alone", []string{"streamer"}, riverClientNone},
		{"notifier alone", []string{"notifier"}, riverClientNone},
		{"api and watcher: watcher still must not get the full worker set", []string{"api", "watcher"}, riverClientInsertOnly},
		{"all roles together", allRoles, riverClientFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveRiverClientKind(tt.roles); got != tt.want {
				t.Errorf("resolveRiverClientKind(%v) = %v, want %v", tt.roles, got, tt.want)
			}
		})
	}
}

// watcherLockName は site ごとに異なるキーを生成すること。site を含めないと、
// 多サイト構成で 2 つの watcher が同じ pg_advisory_lock を取り合い、負けた側の
// mirakc の SSE を誰も購読しなくなる（issue #185 M4-13「含むもの」8）。
func TestWatcherLockName_DiffersPerSite(t *testing.T) {
	tokyoLock := watcherLockName("tokyo")
	takamatsuLock := watcherLockName("takamatsu")
	if tokyoLock == takamatsuLock {
		t.Fatalf("watcherLockName(tokyo) == watcherLockName(takamatsu) = %q, want distinct names", tokyoLock)
	}
	if tokyoLock != watcherLockName("tokyo") {
		t.Errorf("watcherLockName(tokyo) is not deterministic: %q vs %q", tokyoLock, watcherLockName("tokyo"))
	}
}

// TestWatcherLockName_MultiSiteIndependence は watcherLockName が実際に
// pg_advisory_lock のキーとして機能し、2 サイト構成で両方の watcher が
// 同時にリーダーを取れることを確認する（issue #185 の受け入れ基準:
// 「2 サイト構成で watcher を 2 プロセス（サイトごとに 1）立てると両方が
// mirakc の SSE を購読する」）。
//
// 対になる確認（同一サイトで 2 プロセス立てると片方だけが動く）は
// internal/role.TestTryAcquire_Exclusive がキー文字列一般で固定済みなので、
// ここでは「site を変えると独立したロックになる」ことだけを固定する。
func TestWatcherLockName_MultiSiteIndependence(t *testing.T) {
	dbURL := testutil.DatabaseURL(t)
	ctx := context.Background()

	poolTokyo, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating poolTokyo: %v", err)
	}
	defer poolTokyo.Close()

	poolTakamatsu, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating poolTakamatsu: %v", err)
	}
	defer poolTakamatsu.Close()

	acquiredTokyo, releaseTokyo, err := role.TryAcquire(ctx, poolTokyo, watcherLockName("tokyo"))
	if err != nil {
		t.Fatalf("TryAcquire(tokyo): %v", err)
	}
	if !acquiredTokyo {
		t.Fatal("expected tokyo watcher to acquire its lock")
	}
	defer releaseTokyo()

	acquiredTakamatsu, releaseTakamatsu, err := role.TryAcquire(ctx, poolTakamatsu, watcherLockName("takamatsu"))
	if err != nil {
		t.Fatalf("TryAcquire(takamatsu): %v", err)
	}
	if !acquiredTakamatsu {
		t.Fatal("expected takamatsu watcher to also acquire its own lock while tokyo holds its lock " +
			"(site 修飾が無いと両者は同じキーを取り合い、この 2 番目の TryAcquire が失敗する)")
	}
	defer releaseTakamatsu()

	// 同一サイトの 2 プロセス目は従来どおり排他される（片方だけが動く）。
	poolTokyo2, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("creating poolTokyo2: %v", err)
	}
	defer poolTokyo2.Close()
	acquiredTokyo2, _, err := role.TryAcquire(ctx, poolTokyo2, watcherLockName("tokyo"))
	if err != nil {
		t.Fatalf("TryAcquire(tokyo) second process: %v", err)
	}
	if acquiredTokyo2 {
		t.Fatal("expected the second tokyo watcher to NOT acquire the lock (already held)")
	}
}
