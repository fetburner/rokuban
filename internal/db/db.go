package db

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/config"
)

// defaultAPIStatementTimeout は cfg.APIStatementTimeout が未指定（0）のときに
// api ロールを含むプロセスへ適用する statement_timeout（issue #90）。
// 世帯スケールの通常クエリを十分に上回りつつ、インデックス漏れ等による
// 暴走クエリを打ち切れる値として選んである。
const defaultAPIStatementTimeout = 30 * time.Second

// roleConnBudget はロールごとにこのプロセスが必要とする最大コネクション数の目安
// （**束縛サイトが 1 つの場合の値**。2 サイト以上では perSiteConnBudget が
// 上乗せする。issue #532: 1 プロセスが N site を束縛できるようになったため）。
// db.max_conns が未指定のとき、プロセスが担う roles の合計（+ 上乗せ分）を
// pgxpool.Config.MaxConns の既定値として使う（issue #90、docs/operations.md §3）。
//
// 根拠（世帯スケール。数値は保守的な上限であり、実測に基づくチューニングは運用開始後に行う）:
//   - api (10): HTTP リクエストの同時実行に応じる。SSE 配送は notifier が別に持つため
//     api 自身が保持し続める接続はなく、ブラウザの複数タブ・同時操作を吸収する余裕を見た値
//   - worker (8): River の内部機構が LISTEN 用に 1 本を長時間保持する（`river.Client` は
//     `notifier.New` で 1 個の Listener だけを作り、leadership の elector もそれを
//     共有する。`github.com/riverqueue/river@v0.40.0/client.go` で確認済み。elector と
//     notifier がそれぞれ別に 1 本ずつではない）。これに加え、設定されたキューの
//     MaxWorkers（ingest/encode/thumbnail の合計は通常数本、ruler/reconciler/epg_sync/
//     record_sweep 等の定期ジョブ専用キューは MaxWorkers 1）を合わせても世帯スケールでは
//     十分な余裕がある。**加えて、実行中の ingest 1 本ごとに rel_path advisory lock 用の
//     コネクションを 1 本、転送が終わるまで長期保持する**（internal/worker/relpath_lock.go、
//     docs/recording/ingest.md §5.3）。ingest の同時実行は site あたり 1〜2 にキャップ
//     されており、この 8 は 1 site ぶんを見込んだ値。2 site 目以降は
//     perSiteConnBudget が worker あたり workerPerSiteConns を上乗せする
//   - watcher (3): 1 site ぶんのリーダー選出の advisory lock 用に 1 本を保持し
//     続け、record 処理の短いクエリが散発する。2 site 目以降は site ごとに
//     goroutine + advisory lock を持つため（cmd/rokuban/server.go の watcher
//     ループ）、perSiteConnBudget が watcher あたり watcherPerSiteConns を上乗せする
//   - notifier (3): LISTEN 用に 1 本を保持し続けるだけ（site 数に依存しない）
//   - streamer (4): バイト転送そのものは DB 接続を保持しない（X-Accel-Redirect か
//     Go のファイル配信）。リクエストごとのメタデータ照会だけで足りる（site 数に依存しない）
var roleConnBudget = map[string]int32{
	"api":      10,
	"worker":   8,
	"watcher":  3,
	"notifier": 3,
	"streamer": 4,
}

// watcherPerSiteConns / workerPerSiteConns は、2 site 目以降の束縛サイトごとに
// watcher / worker ロールへ追加で見込むコネクション数（perSiteConnBudget /
// minRequiredConns が使う。roleConnBudget の doc コメント参照）。
const (
	// watcherPerSiteConns: 2 site 目以降、site ごとに 1 つの advisory lock 用
	// コネクションが追加で専有される（cmd/rokuban/server.go の watcher ループが
	// site ごとに role.RunSingleton を呼ぶ。issue #532）。
	watcherPerSiteConns = 1
	// workerPerSiteConns: 2 site 目以降、ingest の同時実行キャップ（既定 2、
	// internal/worker.defaultIngestConcurrency）ぶんの rel_path advisory lock
	// コネクションが site ごとに追加で乗りうる（roleConnBudget の worker 8 が
	// 見込んでいるのは 1 site ぶんだけ）。
	workerPerSiteConns = 2
)

// perSiteConnBudget は、束縛サイトが 2 つ以上のとき roleConnBudget に上乗せする
// コネクション数を返す（1 site 以下は roleConnBudget の値がそのまま 1 site 分の
// 見込みなので上乗せ 0）。
func perSiteConnBudget(roles []string, numSites int) int32 {
	if numSites <= 1 {
		return 0
	}
	extraSites := int32(numSites - 1)
	var per int32
	if slices.Contains(roles, "watcher") {
		per += watcherPerSiteConns
	}
	if slices.Contains(roles, "worker") {
		per += workerPerSiteConns
	}
	return per * extraSites
}

// minAutoMaxConns はロール集合から算出した MaxConns の下限。roleConnBudget に
// 無い未知ロールしか渡されたときに合計が 0 になる（=コネクションを 1 本も
// 張れないプールになる）事態を防ぐための安全弁。roleConnBudget の最小値
// （watcher/notifier の 3）を上書きしないよう、それより低い値にしてある。
const minAutoMaxConns = 2

// KnownRoles は internal/db がロール別の挙動（roleConnBudget によるプール
// サイジング・poolerIncompatibleRoles による pooler_compat の fail-fast）を
// 知っているロール名の集合を、重複を除いてソート済みで返す。
//
// これは `cmd/rokuban` の allRoles と一致しているべき、権威が 2 箇所に分かれた
// 値である。両者が unexported のままだと「一致している」ことをテストで書けず、
// M4-6 で新しいロールが増えたときに roleConnBudget / poolerIncompatibleRoles への
// 追記漏れが静かに素通りする（新ロールは自動的に minAutoMaxConns にフォールバック
// し、pooler_compat の fail-fast も素通りする）。`cmd/rokuban` 側のテストで
// `allRoles` と KnownRoles() の集合が一致することを確認する（issue #90 レビュー）。
func KnownRoles() []string {
	seen := make(map[string]struct{}, len(roleConnBudget)+len(poolerIncompatibleRoles))
	for r := range roleConnBudget {
		seen[r] = struct{}{}
	}
	for _, r := range poolerIncompatibleRoles {
		seen[r] = struct{}{}
	}
	roles := make([]string, 0, len(seen))
	for r := range seen {
		roles = append(roles, r)
	}
	slices.Sort(roles)
	return roles
}

// poolerIncompatibleRoles は cfg.PoolerCompat=true のとき同居できないロール。
// advisory lock（watcher）と LISTEN/NOTIFY（worker が使う River の内部機構 / notifier）は
// セッション状態に依存するため、transaction pooling で物理コネクションが要求ごとに
// 入れ替わると構造的に壊れる。pooler を通せるのは api ロールと streamer ロールだけ、というデプロイの契約
// （docs/operations.md §3）を起動時 fail-fast で強制する。
var poolerIncompatibleRoles = []string{"worker", "watcher", "notifier"}

// NewPool は接続プールを作成し、Ping で疎通確認する。
// 接続失敗を起動時に即検出する (EPGStation#628 の教訓: エラーを握り潰さない)。
//
// roles はこのプロセスが実際に担うロール集合（cmd/rokuban/server.go の
// resolveRoles の戻り値）。プロセスは常に 1 個のプールしか持たない（全ロールが
// それを共有する）ため、「ロール別プール上限」はこの 1 個のプールの MaxConns を
// roles から決めることを指す（issue #90）。cfg.MaxConns が明示されていればそれを
// 優先する。roles が空（rescue/enqueue/shadow-diff 等の単発 CLI コマンド）なら
// pgxpool の既定値（max(4, NumCPU)）をそのまま使う。
//
// numSites はこのプロセスが束縛している mirakc サイト数（cmd/rokuban が --sites
// から解決した `bound` の長さ。issue #532）。watcher は site ごとに advisory lock
// 用のコネクションを 1 本専有し続け、worker も site ごとの ingest キューが
// rel_path advisory lock 用のコネクションを追加で必要としうるため、2 サイト以上の
// 束縛ではこの数を pool サイジングに反映する（roleConnBudget / minRequiredConns の
// doc コメント参照）。site 束縛の概念が無い呼び出し元（rescue/enqueue/shadow-diff
// 等の単発 CLI コマンド、testutil）は 0 を渡す --- roles が空ならどのみち
// site 数は判定に使われない。
//
// cfg.PoolerCompat=true のとき roles に worker/watcher/notifier のいずれかが
// 含まれる場合はエラーを返す（pooler を通せるのは api ロールと streamer ロールだけ、という
// デプロイの契約。docs/operations.md §3）。
func NewPool(ctx context.Context, cfg config.DBConfig, roles []string, numSites int) (*pgxpool.Pool, error) {
	poolCfg, err := buildPoolConfig(cfg, roles, numSites)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	return pool, nil
}

// buildPoolConfig は NewPool のロジック本体（MaxConns の算出・pooler 互換設定の適用・
// statement_timeout の設定・fail-fast 検査）を、実接続を伴わずにテストできる形で切り出す。
func buildPoolConfig(cfg config.DBConfig, roles []string, numSites int) (*pgxpool.Config, error) {
	if cfg.PoolerCompat {
		for _, r := range poolerIncompatibleRoles {
			if slices.Contains(roles, r) {
				return nil, fmt.Errorf(
					"db.pooler_compat=true is incompatible with role %q: "+
						"transaction pooling breaks LISTEN/NOTIFY and advisory locks "+
						"used by worker/watcher/notifier (docs/operations.md §3, only "+
						"the api and streamer roles may be deployed behind a pooler)", r)
			}
		}
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parsing connection string: %w", err)
	}

	switch {
	case cfg.MaxConns > 0:
		if min := minRequiredConns(roles, numSites); int32(cfg.MaxConns) < min {
			return nil, fmt.Errorf(
				"db.max_conns=%d is too small for roles %v bound to %d site(s): at least %d "+
					"connections are required so that roles holding a connection for their "+
					"entire lifetime (watcher's advisory lock -- one per bound site, "+
					"worker's/notifier's LISTEN) don't starve the rest of the process's work "+
					"out of the single shared pool (docs/operations.md §3)",
				cfg.MaxConns, roles, numSites, min)
		}
		poolCfg.MaxConns = int32(cfg.MaxConns)
	case len(roles) > 0:
		poolCfg.MaxConns = maxConnsForRoles(roles, numSites)
	}

	if cfg.PoolerCompat {
		// prepared statement キャッシュは transaction pooling 下で壊れるため、
		// 拡張プロトコルの prepare をやめて毎回テキストで送る（QueryExecModeExec）。
		poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	}

	if slices.Contains(roles, "api") {
		timeout := cfg.APIStatementTimeout
		if timeout <= 0 {
			timeout = defaultAPIStatementTimeout
		}
		// RuntimeParams は接続の起動パケットに乗せる session default。クエリ単位の
		// context timeout ではなく接続そのものに載せることで「付け忘れた 1 本」を
		// 作らない（issue #90 の決定）。
		poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(timeout.Milliseconds(), 10)
	}

	return poolCfg, nil
}

// maxConnsForRoles は roles から roleConnBudget の合計（+ 2 サイト目以降の
// perSiteConnBudget の上乗せ）を算出する。
//
// roles は重複除去してから合算する。重複除去しないと同じロールの budget を
// 二重に数えてプール上限が過大になる（issue #90 レビュー）。resolveRoles
// （cmd/rokuban/server.go）が `--roles api,api` を畳むようになった後も、
// ここは多重防御として残す --- db.NewPool の呼び出し元は server だけではない。
func maxConnsForRoles(roles []string, numSites int) int32 {
	var total int32
	for r := range uniqueRoles(roles) {
		total += roleConnBudget[r]
	}
	total += perSiteConnBudget(roles, numSites)
	if total < minAutoMaxConns {
		total = minAutoMaxConns
	}
	return total
}

// uniqueRoles は roles の重複を除いた集合を返す。
func uniqueRoles(roles []string) map[string]struct{} {
	set := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		set[r] = struct{}{}
	}
	return set
}

// dedicatedConnRoles はプロセスの生存期間中コネクションを 1 本専有し続けるロール
// （1 site 束縛の場合の値。watcher は 2 site 目以降 1 site につき 1 本ずつ
// 追加で専有する。下記 minRequiredConns 参照）。roleConnBudget の doc コメントで
// 裏を取った専有元:
//   - watcher: リーダー選出の advisory lock（internal/role.RunSingleton が
//     pool.Acquire したコネクションをリーダーである間保持し続ける）。issue #532
//     で site ごとに goroutine を持つようになったため、束縛サイトの数だけ
//     この専有が増える（cmd/rokuban/server.go の watcher ループ）
//   - worker: River の内部機構の LISTEN（elector と notifier で共有される 1 本。
//     riverqueue/river@v0.40.0/client.go で確認済み）。これは site 数に依存
//     しないプロセス単位の資源なので、site が増えても専有本数は変わらない
//     ---ingest の rel_path advisory lock は転送中だけの一時専有であり、
//     watcher の advisory lock のように「プロセスが生きている間ずっと」では
//     ないため、この恒久専有のカウントには含めない（roleConnBudget /
//     perSiteConnBudget の workerPerSiteConns はソフトな見込みとして別に
//     加算している。無症状デッドロックの検査は「絶対に戻ってこないコネクション」
//     だけを対象にする）
//   - notifier: ブラウザへの SSE 配送のための LISTEN
//     （internal/notifier.EventHub.Run が保持し続ける。site 数に依存しない）
var dedicatedConnRoles = []string{"watcher", "worker", "notifier"}

// minRequiredConns は、明示された db.max_conns がこのロール集合・束縛サイト数に
// とって小さすぎないかを検査するための下限を返す（issue #90 レビュー指摘。
// issue #532 で numSites を追加）。
//
// watcher / worker / notifier はいずれもプロセスの生存期間中コネクションを
// 専有し続ける（dedicatedConnRoles）。watcher は束縛サイトごとに 1 本
// （2 site 目以降 watcherPerSiteConns ずつ追加）。専有分だけでプールが埋まると、
// 同じプロセスが行う他の仕事（watcher の record 処理クエリ、worker のジョブ
// claim、/metrics のバックログクエリ等）が「二度と解放されないコネクション」を
// 待ち続けて無症状にデッドロックする。そのため専有分の合計に加えて、他の仕事の
// ための余地を最低 1 本要求する。
func minRequiredConns(roles []string, numSites int) int32 {
	var dedicated int32
	for _, r := range dedicatedConnRoles {
		if slices.Contains(roles, r) {
			dedicated++
		}
	}
	if slices.Contains(roles, "watcher") && numSites > 1 {
		dedicated += watcherPerSiteConns * int32(numSites-1)
	}
	if dedicated == 0 {
		return 1
	}
	return dedicated + 1
}
