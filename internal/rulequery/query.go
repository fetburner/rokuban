package rulequery

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ProgramMatch は 1 件のマッチ（どの site のどの programId か）。
// API 検索は 1 クエリで複数 site をまたぐため、行ごとに site を持ち帰る必要がある
// （#530。同一放送が複数 site でマッチしても畳まない）。
type ProgramMatch struct {
	Site      string
	ProgramID int64
}

// MatchPrograms は条件にマッチする (site, programId) を返す。c.Sites が絞り込み対象
// （Compile 参照）。API 検索（internal/api/search.go）はこちらを使う。
func MatchPrograms(ctx context.Context, pool *pgxpool.Pool, c Conditions) ([]ProgramMatch, error) {
	compiled, err := Compile(c)
	if err != nil {
		return nil, err
	}

	var sql string
	if compiled.NeedsServiceJoin {
		sql = `
SELECT p.site, p.program_id
FROM epg_programs p
JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
WHERE ` + compiled.Where + `
ORDER BY p.site, p.program_id`
	} else {
		sql = `
SELECT p.site, p.program_id
FROM epg_programs p
WHERE ` + compiled.Where + `
ORDER BY p.site, p.program_id`
	}

	rows, err := pool.Query(ctx, sql, compiled.Args...)
	if err != nil {
		return nil, fmt.Errorf("matching programs: %w", err)
	}
	defer rows.Close()

	var matches []ProgramMatch
	for rows.Next() {
		var m ProgramMatch
		if err := rows.Scan(&m.Site, &m.ProgramID); err != nil {
			return nil, err
		}
		matches = append(matches, m)
	}
	return matches, rows.Err()
}

// MatchProgramIDs は条件にマッチする epg_programs.program_id を返す（site は捨てる）。
// ruler は評価対象を常に 1 site に絞ってから呼ぶため、呼び出し側が site を既知
// （MatchProgramIDsForRule 参照）。
func MatchProgramIDs(ctx context.Context, pool *pgxpool.Pool, c Conditions) ([]int64, error) {
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

// MatchProgramIDsForRule は rule_id の条件で、site 1 つ分のマッチする program_id を返す。
// ruler がサイトごとに呼ぶ（1 パスは site のループで全ルールを評価する。
// docs/recording/ruler.md「サイトの扱い」）。
//
// rule_sites（c.Sites）が非空かつ site を含まなければ、そのルールは site の対象外
// なのでクエリを投げずに空を返す（#530: Compile が Sites の空を許さないため、
// 「rule_sites 空 = 全サイト、非空 = そのリストのみ」という規約はここで解決してから
// c.Sites を site 1 件に置き換える）。
func MatchProgramIDsForRule(ctx context.Context, pool *pgxpool.Pool, site string, ruleID int64) ([]int64, error) {
	c, err := LoadConditions(ctx, sqlcgen.New(pool), ruleID)
	if err != nil {
		return nil, err
	}
	if len(c.Sites) > 0 && !slices.Contains(c.Sites, site) {
		return nil, nil
	}
	c.Sites = []string{site}
	return MatchProgramIDs(ctx, pool, c)
}
