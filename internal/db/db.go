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

// roleConnBudget はロールごとにこのプロセスが必要とする最大コネクション数の目安。
// db.max_conns が未指定のとき、プロセスが担う roles の合計を
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
//     十分な余裕がある
//   - watcher (3): リーダー選出の advisory lock 用に 1 本を保持し続け、record 処理の
//     短いクエリが散発する
//   - notifier (3): LISTEN 用に 1 本を保持し続けるだけ
//   - streamer (4): バイト転送そのものは DB 接続を保持しない（X-Accel-Redirect か
//     Go のファイル配信）。リクエストごとのメタデータ照会だけで足りる
var roleConnBudget = map[string]int32{
	"api":      10,
	"worker":   8,
	"watcher":  3,
	"notifier": 3,
	"streamer": 4,
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
// cfg.PoolerCompat=true のとき roles に worker/watcher/notifier のいずれかが
// 含まれる場合はエラーを返す（pooler を通せるのは api ロールと streamer ロールだけ、という
// デプロイの契約。docs/operations.md §3）。
func NewPool(ctx context.Context, cfg config.DBConfig, roles []string) (*pgxpool.Pool, error) {
	poolCfg, err := buildPoolConfig(cfg, roles)
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
func buildPoolConfig(cfg config.DBConfig, roles []string) (*pgxpool.Config, error) {
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
		if min := minRequiredConns(roles); int32(cfg.MaxConns) < min {
			return nil, fmt.Errorf(
				"db.max_conns=%d is too small for roles %v: at least %d connections are "+
					"required so that roles holding a connection for their entire lifetime "+
					"(watcher's advisory lock, worker's/notifier's LISTEN) don't starve the "+
					"rest of the process's work out of the single shared pool (docs/operations.md §3)",
				cfg.MaxConns, roles, min)
		}
		poolCfg.MaxConns = int32(cfg.MaxConns)
	case len(roles) > 0:
		poolCfg.MaxConns = maxConnsForRoles(roles)
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

// maxConnsForRoles は roles から roleConnBudget の合計を算出する。
//
// roles は重複除去してから合算する。resolveRoles（cmd/rokuban/server.go）は
// `--roles api,api` のような重複指定をそのまま通すため、重複除去しないと
// 同じロールの budget を二重に数えてプール上限が過大になる（issue #90 レビュー）。
func maxConnsForRoles(roles []string) int32 {
	var total int32
	for r := range uniqueRoles(roles) {
		total += roleConnBudget[r]
	}
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

// dedicatedConnRoles はプロセスの生存期間中コネクションを 1 本専有し続けるロール。
// roleConnBudget の doc コメントで裏を取った専有元:
//   - watcher: リーダー選出の advisory lock（internal/role.RunSingleton が
//     pool.Acquire したコネクションをリーダーである間保持し続ける）
//   - worker: River の内部機構の LISTEN（elector と notifier で共有される 1 本。
//     riverqueue/river@v0.40.0/client.go で確認済み）
//   - notifier: ブラウザへの SSE 配送のための LISTEN
//     （internal/notifier.EventHub.Run が保持し続ける）
var dedicatedConnRoles = []string{"watcher", "worker", "notifier"}

// minRequiredConns は、明示された db.max_conns がこのロール集合にとって小さすぎないかを
// 検査するための下限を返す（issue #90 レビュー指摘）。
//
// watcher / worker / notifier はいずれもプロセスの生存期間中コネクションを 1 本専有し
// 続ける（dedicatedConnRoles）。専有分だけでプールが埋まると、同じプロセスが行う他の
// 仕事（watcher の record 処理クエリ、worker のジョブ claim、/metrics のバックログ
// クエリ等）が「二度と解放されないコネクション」を待ち続けて無症状にデッドロックする。
// そのため専有分の合計に加えて、他の仕事のための余地を最低 1 本要求する。
func minRequiredConns(roles []string) int32 {
	var dedicated int32
	for _, r := range dedicatedConnRoles {
		if slices.Contains(roles, r) {
			dedicated++
		}
	}
	if dedicated == 0 {
		return 1
	}
	return dedicated + 1
}
