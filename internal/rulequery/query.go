package rulequery

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ProgramMatch は 1 件のマッチ（どの site のどの番組か）。
// API 検索は表示に使う番組情報も同じクエリから返すため、検索結果の行として必要な
// 最小限の射影をここで持ち帰る。site は番組の identity の一部なので、同一放送が
// 複数 site でマッチしても行を畳まない（#530）。
type ProgramMatch struct {
	Site       string
	ProgramID  int64
	NetworkID  int32
	ServiceID  int32
	StartAt    time.Time
	DurationMs int64
	Name       string
	IsFree     bool
}

// MatchPrograms は条件にマッチした番組の検索結果行を返す。c.Sites が絞り込み対象を
// 指定している場合はそのサイトだけを対象とする（Compile 参照）。API 検索
// （internal/api/search.go）はこちらを使う。
func MatchPrograms(ctx context.Context, pool *pgxpool.Pool, c Conditions) ([]ProgramMatch, error) {
	compiled, err := Compile(c)
	if err != nil {
		return nil, err
	}

	var sql string
	if compiled.NeedsServiceJoin {
		sql = `
SELECT p.site, p.program_id, p.network_id, p.service_id,
       p.start_at, p.duration_ms, p.name, p.is_free
FROM epg_programs p
JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
WHERE ` + compiled.Where + `
ORDER BY p.program_id, p.site`
	} else {
		sql = `
SELECT p.site, p.program_id, p.network_id, p.service_id,
       p.start_at, p.duration_ms, p.name, p.is_free
FROM epg_programs p
WHERE ` + compiled.Where + `
ORDER BY p.program_id, p.site`
	}

	rows, err := pool.Query(ctx, sql, compiled.Args...)
	if err != nil {
		return nil, fmt.Errorf("matching programs: %w", err)
	}
	defer rows.Close()

	var matches []ProgramMatch
	for rows.Next() {
		var m ProgramMatch
		if err := rows.Scan(
			&m.Site,
			&m.ProgramID,
			&m.NetworkID,
			&m.ServiceID,
			&m.StartAt,
			&m.DurationMs,
			&m.Name,
			&m.IsFree,
		); err != nil {
			return nil, err
		}
		matches = append(matches, m)
	}
	return matches, rows.Err()
}

// MatchProgramIDsForRule は rule_id の条件で、site 1 つ分のマッチする program_id を返す。
// ruler がサイトごとに呼ぶ（1 パスは site のループで全ルールを評価する。
// docs/recording/ruler.md「サイトの扱い」）。
//
// rule_sites（c.Sites）が非空かつ site を含まなければ、そのルールは site の対象外
// なのでクエリを投げずに空を返す。対象内なら site 1 件に絞って MatchPrograms を呼び、
// site は呼び出し側が既知なので programId だけ返す。
func MatchProgramIDsForRule(ctx context.Context, pool *pgxpool.Pool, site string, ruleID int64) ([]int64, error) {
	c, err := LoadConditions(ctx, sqlcgen.New(pool), ruleID)
	if err != nil {
		return nil, err
	}
	if len(c.Sites) > 0 && !slices.Contains(c.Sites, site) {
		return nil, nil
	}
	c.Sites = []string{site}
	matches, err := MatchPrograms(ctx, pool, c)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(matches))
	for i, m := range matches {
		ids[i] = m.ProgramID
	}
	return ids, nil
}
