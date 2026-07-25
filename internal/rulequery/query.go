package rulequery

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// MatchProgramIDs は条件にマッチする epg_programs.program_id を返す。
// site は評価対象サイト。ruler はサイトごとにこれを呼ぶ。
func MatchProgramIDs(ctx context.Context, pool *pgxpool.Pool, site string, c Conditions) ([]int64, error) {
	compiled, err := Compile(site, c)
	if err != nil {
		return nil, err
	}

	var sql string
	if compiled.NeedsServiceJoin {
		sql = `
SELECT p.program_id
FROM epg_programs p
JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
WHERE ` + compiled.Where + `
ORDER BY p.program_id`
	} else {
		sql = `
SELECT p.program_id
FROM epg_programs p
WHERE ` + compiled.Where + `
ORDER BY p.program_id`
	}

	rows, err := pool.Query(ctx, sql, compiled.Args...)
	if err != nil {
		return nil, fmt.Errorf("matching programs: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MatchProgramIDsForRule は rule_id の条件でマッチする program_id を返す。
func MatchProgramIDsForRule(ctx context.Context, pool *pgxpool.Pool, site string, ruleID int64) ([]int64, error) {
	c, err := LoadConditions(ctx, sqlcgen.New(pool), ruleID)
	if err != nil {
		return nil, err
	}
	return MatchProgramIDs(ctx, pool, site, c)
}
