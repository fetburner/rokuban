package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/role"
	"github.com/fetburner/rokuban/internal/testutil"
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

// 既存の `mirakc: {url, site}` だけの config は 1 文字も変えずに動き、--sites
// 省略でそのサイトに束縛される（issue #183 の受け入れ基準 1）。
func TestServerSiteBinding_ExistingSingleMirakcConfig_Unchanged(t *testing.T) {
	path := writeServerTestConfig(t, `
db:
  host: localhost
  user: rokuban
  password: secret
  database: rokuban
mirakc:
  url: http://mirakc.local:40772
  site: home
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

// mirakc: と mirakcs: の同時指定は Load 自体が起動エラーにする
// （issue #183 の受け入れ基準 4）。
func TestServerSiteBinding_MirakcAndMirakcsBothSet_LoadFails(t *testing.T) {
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
func writeOnceModeConfig(t *testing.T) string {
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
mirakc:
  url: http://127.0.0.1:1
  site: home
storage:
  media_dir: %q
worker:
  periodic_jobs: false
`, dbCfg.Host, dbCfg.Port, user, password, dbCfg.Database, dbCfg.SSLMode, t.TempDir()))
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
		args := append(append([]string{}, onceArgs...), "--once-idle-timeout="+idle.String())
		elapsed, err := runServerCmdBounded(t, 30*time.Second, path, args...)
		if err != nil {
			t.Fatalf("server --once: %v", err)
		}
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
		args := append(append([]string{}, onceArgs...), "--once-idle-timeout=60s")
		if _, err := runServerCmdBounded(t, 30*time.Second, path, args...); err != nil {
			t.Fatalf("server --once: %v", err)
		}

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

// **未登録 kind のジョブを掴んでも Job が終わること。** River の executor は
// 登録されていない kind を WorkUnit == nil で早期 return し、その時点では
// WorkerMiddleware のチェーンをまだ組み立てていない（worker.SubscribeOnceEvents）。
// middleware だけを見ていると、Job は「1 件も claim していない」と誤認したまま
// idleTimeout の間そのキューを掴み続け、試行回数を潰す。
//
// 版ずれ（新しいイメージの CronJob / api が、古い worker の知らない kind を
// 投入する）で起きうるほか、**受け入れ判定ハーネスの `insert_probe_job` が
// 入れる `e2e_probe` がまさにこの形**である（deploy/k8s/e2e/lib/kube.sh）。
//
// 判定は 2 つ。**どちらも「戻った」だけでは足りない** --- 壊れていても
// idleTimeout 後には戻るので、(a) idleTimeout より十分早く戻ること、
// (b) 試行を 1 回しか潰していないこと（壊れていると窓の中で何度も掴み直す）。
func TestServerCmd_OnceModeExitsOnUnhandledJobKind(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()
	path := writeOnceModeConfig(t)

	// 製品の投入経路には無い kind なので DB へ直接入れる（ハーネスと同じ形）。
	if _, err := pool.Exec(ctx,
		`INSERT INTO river_job (state, queue, kind, args, max_attempts, priority, scheduled_at)
		 VALUES ('available', 'cleanup', 'e2e_probe', '{}'::jsonb, 25, 1, now())`,
	); err != nil {
		t.Fatalf("inserting unhandled-kind job: %v", err)
	}

	elapsed, err := runServerCmdBounded(t, 20*time.Second, path,
		"--roles", "worker", "--sites=", "--queues=cleanup", "--once", "--once-idle-timeout=60s")
	if err != nil {
		t.Fatalf("server --once: %v", err)
	}
	if elapsed > 20*time.Second {
		t.Errorf("elapsed = %s: idle timeout を待ってから終わっている（イベントで終われていない）", elapsed)
	}

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
