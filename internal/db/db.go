package db

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

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
//   - worker (8): **床（下限）であって合計ではない。** 実際の予算は workerConnBudget が
//     lockSlots（同時に走りうる job advisory lock 保持ジョブの本数）から導出する。
//     値そのものは「lockSlots が小さい構成で、既存デプロイの上限を下げない」ための床
//     として残っている
//   - watcher (3): 1 site ぶんのリーダー選出の advisory lock 用に 1 本を保持し
//     続け、record 処理の短いクエリが散発する。2 site 目以降は site ごとに
//     goroutine + advisory lock を持つため（cmd/rokuban/server.go の watcher
//     ループ）、perSiteConnBudget が watcher あたり watcherPerSiteConns を上乗せする
//   - notifier (3): LISTEN 用に 1 本を保持し続けるだけ（site 数に依存しない）
//   - streamer (4): バイト転送そのものは DB 接続を保持しない（X-Accel-Redirect か
//     Go のファイル配信）。リクエストごとのメタデータ照会だけで足りる（site 数に依存しない）
var roleConnBudget = map[string]int32{
	"api":      10,
	"worker":   workerConnFloor,
	"watcher":  3,
	"notifier": 3,
	"streamer": 4,
}

const (
	// workerConnFloor は worker ロールの予算の床（roleConnBudget の worker の値）。
	//
	// 床が効くのは lockSlots が 4 以下のとき（1 + lockSlots + workerConnSlack <= 8）。
	// 既定構成の lockSlots は 5（ingest 3 + encode 1 + cm_detect 1、1 site）なので、
	// 既定の予算は床ではなく式の側（9）で決まる。**ingest を引かないデプロイ
	// （`--queues=ruler` 等）の上限を、この変更で下げないために置いてある。**
	workerConnFloor = 8

	// workerConnSlack は worker の予算のうち、LISTEN でも job lock でもない仕事
	// （ジョブ claim、進捗書き込み、`/metrics` のバックログクエリ等）に残す本数。
	//
	// **未測定である。** 値は「この変更の前の予算 8 − 既定で長期保持する
	// 1(LISTEN) + 4(job lock) をそのまま据え置いた」もので、実測に基づかない。
	workerConnSlack = 3

	// workerListenConns は River の内部機構が LISTEN 用に長時間保持する本数。
	// `river.Client` は `notifier.New` で 1 個の Listener だけを作り、leadership の
	// elector もそれを共有する（`river@v0.47.0 client.go` の `notifier.New` と
	// `leadership.NewElector` で確認済み。elector と notifier がそれぞれ別に
	// 1 本ずつではない）。site 数に依存しないプロセス単位の資源。
	workerListenConns = 1

	// watcherPerSiteConns は、2 site 目以降の束縛サイトごとに watcher ロールへ
	// 追加で見込むコネクション数（perSiteConnBudget / minRequiredConns が使う）。
	// site ごとに 1 つの advisory lock 用コネクションが追加で専有される
	// （cmd/rokuban/server.go の watcher ループが site ごとに role.RunSingleton を
	// 呼ぶ。issue #532）。
	watcherPerSiteConns = 1
)

// workerConnBudget は worker ロールの予算を lockSlots から導出する。
//
// 構成（workerListenConns + lockSlots + workerConnSlack）を、既存デプロイの
// 上限を下げないための床 workerConnFloor で下支えする。
//
// **lockSlots は呼び出し元が数える**（internal/worker.LockSlots）。同時実行数は
// 設定（ingest.concurrency / encode.concurrency）と束縛サイト数から決まるので、
// db 側に既定値を焼き込むと運用者が設定を変えたときに予算が追随しない。
func workerConnBudget(lockSlots int) int32 {
	return max(workerConnFloor, int32(workerListenConns+lockSlots+workerConnSlack))
}

// perSiteConnBudget は、束縛サイトが 2 つ以上のとき roleConnBudget に上乗せする
// コネクション数を返す（1 site 以下は roleConnBudget の値がそのまま 1 site 分の
// 見込みなので上乗せ 0）。
//
// **worker はここに含まれない。** job advisory lock の本数は site 数に比例するが、
// それは lockSlots として呼び出し元から渡ってくる（workerConnBudget）。
func perSiteConnBudget(roles []string, numSites int) int32 {
	if numSites <= 1 {
		return 0
	}
	extraSites := int32(numSites - 1)
	var per int32
	if slices.Contains(roles, "watcher") {
		per += watcherPerSiteConns
	}
	return per * extraSites
}

// minAutoMaxConns はロール集合から算出した MaxConns の下限。roleConnBudget に
// 無い未知ロールしか渡されたときに合計が 0 になる（=コネクションを 1 本も
// 張れないプールになる）事態を防ぐための安全弁。roleConnBudget の最小値
// （watcher/notifier の 3）を上書きしないよう、それより低い値にしてある。
const minAutoMaxConns = 2

// KnownRoles は internal/db がプールサイジングを知っているロール名の集合を、
// 重複を除いてソート済みで返す。
//
// これは `cmd/rokuban` の allRoles と一致しているべき、権威が 2 箇所に分かれた
// 値である。両者が unexported のままだと「一致している」ことをテストで書けず、
// M4-6 で新しいロールが増えたときに roleConnBudget への追記漏れが静かに素通りする
// （新ロールは自動的に minAutoMaxConns にフォールバックする）。`cmd/rokuban` 側の
// テストで `allRoles` と KnownRoles() の集合が一致することを確認する（issue #90
// レビュー）。
func KnownRoles() []string {
	seen := make(map[string]struct{}, len(roleConnBudget))
	for r := range roleConnBudget {
		seen[r] = struct{}{}
	}
	roles := make([]string, 0, len(seen))
	for r := range seen {
		roles = append(roles, r)
	}
	slices.Sort(roles)
	return roles
}

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
// 用のコネクションを 1 本専有し続けるため、2 サイト以上の束縛ではこの数を
// pool サイジングに反映する（roleConnBudget / minRequiredConns の
// doc コメント参照）。site 束縛の概念が無い呼び出し元（rescue/enqueue/shadow-diff
// 等の単発 CLI コマンド、testutil）は 0 を渡す --- roles が空ならどのみち
// site 数は判定に使われない。
//
// lockSlots はこのプロセスで同時に走りうる job advisory lock 保持ジョブの本数
// （internal/worker.LockSlots が設定と束縛サイト数から数える）。**db は worker を
// import しない**ので値そのものを受け取る。worker ロールが無ければ 0。
func NewPool(ctx context.Context, cfg config.DBConfig, roles []string, numSites, lockSlots int) (*pgxpool.Pool, error) {
	poolCfg, err := buildPoolConfig(cfg, roles, numSites, lockSlots)
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

// buildPoolConfig は NewPool のロジック本体（MaxConns の算出と
// statement_timeout の設定）を、実接続を伴わずにテストできる形で切り出す。
func buildPoolConfig(cfg config.DBConfig, roles []string, numSites, lockSlots int) (*pgxpool.Config, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parsing connection string: %w", err)
	}

	switch {
	case cfg.MaxConns > 0:
		if min := minRequiredConns(roles, numSites, lockSlots); int32(cfg.MaxConns) < min {
			return nil, fmt.Errorf(
				"db.max_conns=%d is too small for roles %v bound to %d site(s) with %d job "+
					"lock slot(s): at least %d connections are required so that roles holding a "+
					"connection whose release depends on acquiring another one don't starve the "+
					"rest of the process's work out of the single shared pool -- watcher's "+
					"advisory lock (one per bound site), worker's/notifier's LISTEN, and one "+
					"connection per running ingest/encode/cm_detect job (those jobs write "+
					"progress on a second connection before releasing the first) "+
					"(docs/operations.md §3)",
				cfg.MaxConns, roles, numSites, lockSlots, min)
		}
		poolCfg.MaxConns = int32(cfg.MaxConns)
	case len(roles) > 0:
		poolCfg.MaxConns = maxConnsForRoles(roles, numSites, lockSlots)
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

// maxConnsForRoles は roles から予算の合計（+ 2 サイト目以降の
// perSiteConnBudget の上乗せ）を算出する。
//
// roles は重複除去してから合算する。重複除去しないと同じロールの budget を
// 二重に数えてプール上限が過大になる（issue #90 レビュー）。resolveRoles
// （cmd/rokuban/server.go）が `--roles api,api` を畳むようになった後も、
// ここは多重防御として残す --- db.NewPool の呼び出し元は server だけではない。
//
// worker だけは roleConnBudget の表を使わず、lockSlots から導出する
// （connBudgetForRole / workerConnBudget）。
func maxConnsForRoles(roles []string, numSites, lockSlots int) int32 {
	var total int32
	for r := range uniqueRoles(roles) {
		total += connBudgetForRole(r, lockSlots)
	}
	total += perSiteConnBudget(roles, numSites)
	if total < minAutoMaxConns {
		total = minAutoMaxConns
	}
	return total
}

// connBudgetForRole はロール 1 つぶんの予算を返す。
func connBudgetForRole(role string, lockSlots int) int32 {
	if role == "worker" {
		return workerConnBudget(lockSlots)
	}
	return roleConnBudget[role]
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
//     `river@v0.47.0 client.go` の `notifier.New` と `leadership.NewElector` で確認済み）。これは site 数に依存
//     しないプロセス単位の資源なので、site が増えても専有本数は変わらない
//     ---job advisory lock のぶんはここに入らない（本数が設定から決まるので
//     lockSlots として別に数える。minRequiredConns 参照）
//   - notifier: ブラウザへの SSE 配送のための LISTEN
//     （internal/notifier.EventHub.Run が保持し続ける。site 数に依存しない）
var dedicatedConnRoles = []string{"watcher", "worker", "notifier"}

// minRequiredConns は、明示された db.max_conns がこのロール集合・束縛サイト数に
// とって小さすぎないかを検査するための下限を返す（issue #90 レビュー指摘。
// issue #532 で numSites を追加）。
//
// 数え上げるのは「**解放が別の接続取得に依存する**専有」である:
//
//   - watcher / worker / notifier の恒久専有（dedicatedConnRoles）。watcher は
//     束縛サイトごとに 1 本（2 site 目以降 watcherPerSiteConns ずつ追加）
//   - 実行中の ingest / encode / cm_detect 1 本ごとの job advisory lock
//     （lockSlots）。**かつてこれを「転送中だけの一時専有」として除外していたのは
//     誤りだった。** lock を持つジョブは、解放する前に同じプールからもう 1 本
//     取る（進捗書き込み・commit。internal/worker/ingest_progress.go）。
//     LISTEN と lock でプールが埋まると、ジョブ同士が互いの接続を待つ循環になる
//     --- heartbeat は lock セッション自身の上で動くので lock は生き続け、
//     record_sweep も回収しない。**構造から確定した結論で、実測はしていない。**
//
// 専有分だけでプールが埋まると、同じプロセスが行う他の仕事（watcher の record 処理
// クエリ、worker のジョブ claim、/metrics のバックログクエリ等）が「二度と解放
// されないコネクション」を待ち続けて無症状にデッドロックする。そのため専有分の
// 合計に加えて、他の仕事のための余地を最低 1 本要求する。
//
// **lock をプール外の接続で張る案は採らない。** db.max_conns がプロセスの接続
// 上限だという契約を破ることになる（監視・サーバー側の max_connections の見積もりが
// 両方とも成り立たなくなる）。
func minRequiredConns(roles []string, numSites, lockSlots int) int32 {
	var dedicated int32
	for _, r := range dedicatedConnRoles {
		if slices.Contains(roles, r) {
			dedicated++
		}
	}
	if slices.Contains(roles, "watcher") && numSites > 1 {
		dedicated += watcherPerSiteConns * int32(numSites-1)
	}
	return dedicated + int32(lockSlots) + 1
}
