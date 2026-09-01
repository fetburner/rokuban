package ruler_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ruler"
	"github.com/fetburner/rokuban/internal/testutil"
)

// このファイルは docs/recording/ruler.md「サイトの扱い」が固定する意味論
// ---「ルールはサイトに従属しないグローバルな資産で、実体化はマッチした
// 全サイトで予約を作る（N 予約が既定）」--- を 2 サイトのフィクスチャで
// 直接測る（issue #528）。
//
// 実運用では ruler のサイトループ（RunPass の `for _, site := range r.sites`）は
// 1 周しか回らない --- 呼び出し元の internal/worker.RulerPassWorker がジョブ
// 引数のサイトを 1 つだけ渡す（排他がサイト単位のため。ruler.go の New の doc
// コメント参照）。それでもこのファイルは `ruler.New` に複数サイトを渡す経路を
// 直接使う: N 予約は「複数サイト構成でマッチした全サイトに予約ができる」という
// 主張なので、1 サイトしか渡さない経路をどれだけ検証しても測れない。サイト
// ループを先頭 1 件で打ち切る変異（本来なら 2 サイト目を無視してしまうバグ）も、
// 1 サイトしか渡さないテストでは通ってしまう。
const (
	nresSiteAName = "site-a"
	nresSiteBName = "site-b"
)

// nresNetworkID/nresServiceID はこのファイル専用の NID/SID。放送規格のスコープで
// サイトに依存しないため、2 サイトの epg_services/epg_programs に同じ値で
// フィクスチャを入れることで「同一放送を複数サイトで受けている」状態を作れる
// （docs/recording/ruler.md「サイトの扱い」）。
const (
	nresNetworkID  int32 = 40001
	nresServiceID  int32 = 2048
	nresProgramID  int64 = 900001
	nresDurationMs int64 = 1800000
)

// insertServiceAtSite は site の epg_services に nresNetworkID/nresServiceID の
// チャンネル行を用意する。
func insertServiceAtSite(t *testing.T, pool *pgxpool.Pool, ctx context.Context, site string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
INSERT INTO epg_services (site, network_id, service_id, type, logo_id, remote_control_key_id, name, channel_type, channel, has_logo_data)
VALUES ($1, $2, $3, 1, 0, 1, 'テスト局', 'GR', '27', false)
ON CONFLICT (site, network_id, service_id) DO NOTHING`, site, nresNetworkID, nresServiceID)
	if err != nil {
		t.Fatalf("inserting epg_services fixture at site %s: %v", site, err)
	}
}

// insertProgramAtSite は site の epg_programs に、両サイト共通の programId
// （Mirakurun の ID 合成により同一放送は全サイトで同一 programId を持つ）で
// 番組行を用意する。
func insertProgramAtSite(t *testing.T, pool *pgxpool.Pool, ctx context.Context, site string, startAt time.Time) {
	t.Helper()
	endAt := startAt.Add(time.Duration(nresDurationMs) * time.Millisecond)
	_, err := pool.Exec(ctx, `
INSERT INTO epg_programs (
  site, program_id, network_id, service_id, event_id,
  start_at, duration_ms, end_at, is_free, name, description, genre_lv1
) VALUES ($1, $2, $3, $4, 0, $5, $6, $7, true, 'N予約対象番組', '', '{}'::smallint[])
ON CONFLICT (site, program_id) DO NOTHING`,
		site, nresProgramID, nresNetworkID, nresServiceID, startAt, nresDurationMs, endAt)
	if err != nil {
		t.Fatalf("inserting epg_programs fixture at site %s: %v", site, err)
	}
}

// insertServiceRule はサービス条件（rule_services、(network_id, service_id) の
// 一致）だけを持つ有効なルールを作る。テキスト条件ではなくサービス条件を使うのは、
// internal/rulequery のサービス述語（compile.go の `(p.network_id, p.service_id)
// IN (...)`）を直接経由させるため --- ここに `p.site` を混ぜる変異が実際に踏む
// 経路でなければ検証にならない。
func insertServiceRule(t *testing.T, pool *pgxpool.Pool, ctx context.Context, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `
INSERT INTO rules (name, priority) VALUES ($1, 10) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatalf("inserting rule fixture: %v", err)
	}
	q := sqlcgen.New(pool)
	if err := q.InsertRuleService(ctx, sqlcgen.InsertRuleServiceParams{
		RuleID: id, NetworkID: nresNetworkID, ServiceID: nresServiceID,
	}); err != nil {
		t.Fatalf("inserting rule_services fixture: %v", err)
	}
	return id
}

// reservationSitesForProgram は programID の予約行が存在する site の集合を返す
// （呼び出し側でソート済みの比較に使うため昇順ソートして返す）。
func reservationSitesForProgram(t *testing.T, pool *pgxpool.Pool, ctx context.Context, programID int64) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT site FROM reservations WHERE program_id = $1 ORDER BY site`, programID)
	if err != nil {
		t.Fatalf("querying reservations by program_id: %v", err)
	}
	defer rows.Close()
	var sites []string
	for rows.Next() {
		var site string
		if err := rows.Scan(&site); err != nil {
			t.Fatalf("scanning site: %v", err)
		}
		sites = append(sites, site)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating reservations by program_id: %v", err)
	}
	return sites
}

// 解くべき問題そのもの: rule_sites を一切指定しない（= 全サイト、CLAUDE.md
// 不変条件 10 により空集合を表す行は作らない）ルールが、同一放送を受けている
// 2 サイトの両方で予約行を作る（N 予約が既定。docs/recording/ruler.md
// 「サイトの扱い」）。
//
// 変異確認（issue #528 の罠、両方が実際に落ちることを確認済み。詳細は PR 説明）:
//
//   - ruler.go の RunPass のサイトループ（`for _, site := range r.sites`）を
//     先頭 1 件で打ち切ると、siteB の予約が作られず本テストが失敗する。
//
//   - internal/rulequery/compile.go のサービス述語
//     `(p.network_id, p.service_id) IN (...)` を
//     `(p.network_id, p.service_id, p.site) IN ((nid, sid, 'site-a'))` に変える
//     （固定した実在サイトを混ぜる）と、siteA は変わらずマッチし続けるが siteB
//     側の評価では一致しなくなり、本テストだけが失敗して
//     TestRunPass_RuleSitesRestrictsToListedSiteOnly は通り続ける ---
//     「サイトが 1 つに絞られていないと落ちる」ことをこの対の差で示す。
//
//     **混ぜる値は評価対象サイト（`site` 引数）そのものであってはならない。**
//     Compile は既に `p.site = $1`（compile.go:91、arg(site)）を無条件で AND
//     しているため、そこにさらに `arg(site)` を足しても常に真になり意味論を
//     一切変えない（両テストとも変化なく通り、何も検証しない変異になる）。
//     評価対象サイトとは異なる固定サイト値を使ってはじめて、
//     「サービス条件が特定サイトに紐づいてしまう」バグを再現できる。
func TestRunPass_MultiSiteMatchCreatesReservationPerSite(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertServiceAtSite(t, pool, ctx, nresSiteAName)
	insertServiceAtSite(t, pool, ctx, nresSiteBName)
	insertProgramAtSite(t, pool, ctx, nresSiteAName, start)
	insertProgramAtSite(t, pool, ctx, nresSiteBName, start)

	insertServiceRule(t, pool, ctx, "n-reservation-all-sites")
	// rule_sites には何も入れない = 全サイト（不変条件 10: 空集合を表す行を作らない）。

	r := ruler.New([]string{nresSiteAName, nresSiteBName}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	got := reservationSitesForProgram(t, pool, ctx, nresProgramID)
	want := []string{nresSiteAName, nresSiteBName}
	if !slices.Equal(got, want) {
		t.Fatalf("reservation sites for program %d = %v, want %v (matched rule must materialize a reservation on every matching site — N reservations is the default)",
			nresProgramID, got, want)
	}
}

// rule_sites に 1 サイトだけ入れると、両サイトで同一放送を受けていても
// そのサイトだけに予約ができる（rule_sites が唯一の絞り込み機構であることの
// 反対側）。
func TestRunPass_RuleSitesRestrictsToListedSiteOnly(t *testing.T) {
	pool := testutil.SetupDB(t)
	ctx := context.Background()

	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	insertServiceAtSite(t, pool, ctx, nresSiteAName)
	insertServiceAtSite(t, pool, ctx, nresSiteBName)
	insertProgramAtSite(t, pool, ctx, nresSiteAName, start)
	insertProgramAtSite(t, pool, ctx, nresSiteBName, start)

	ruleID := insertServiceRule(t, pool, ctx, "n-reservation-single-site")
	q := sqlcgen.New(pool)
	if err := q.InsertRuleSite(ctx, sqlcgen.InsertRuleSiteParams{RuleID: ruleID, Site: nresSiteAName}); err != nil {
		t.Fatalf("inserting rule_sites fixture: %v", err)
	}

	r := ruler.New([]string{nresSiteAName, nresSiteBName}, pool, nil)
	if err := r.RunPass(ctx); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	got := reservationSitesForProgram(t, pool, ctx, nresProgramID)
	want := []string{nresSiteAName}
	if !slices.Equal(got, want) {
		t.Fatalf("reservation sites for program %d = %v, want %v (rule_sites listing only %s must restrict materialization to that site)",
			nresProgramID, got, want, nresSiteAName)
	}
}
