package rulequery

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// LoadConditions は DB のルール行 + 子テーブルから Conditions を組み立てる。
func LoadConditions(ctx context.Context, q *sqlcgen.Queries, ruleID int64) (Conditions, error) {
	row, err := q.GetRule(ctx, ruleID)
	if err != nil {
		return Conditions{}, fmt.Errorf("getting rule %d: %w", ruleID, err)
	}

	c := Conditions{
		DurationMinMs: row.DurationMinMs,
		DurationMaxMs: row.DurationMaxMs,
		PeriodStartAt: row.PeriodStartAt,
		PeriodEndAt:   row.PeriodEndAt,
	}
	if row.IsFree.Valid {
		v := row.IsFree.Bool
		c.IsFree = &v
	}

	texts, err := q.ListRuleTextMatches(ctx, ruleID)
	if err != nil {
		return Conditions{}, err
	}
	for _, t := range texts {
		c.TextMatches = append(c.TextMatches, TextMatch{
			Target:        t.Target,
			Mode:          t.Mode,
			Value:         t.Value,
			CaseSensitive: t.CaseSensitive,
			Negate:        t.Negate,
		})
	}

	services, err := q.ListRuleServices(ctx, ruleID)
	if err != nil {
		return Conditions{}, err
	}
	for _, s := range services {
		c.Services = append(c.Services, ServiceRef{NetworkID: s.NetworkID, ServiceID: s.ServiceID})
	}

	cts, err := q.ListRuleChannelTypes(ctx, ruleID)
	if err != nil {
		return Conditions{}, err
	}
	for _, ct := range cts {
		c.ChannelTypes = append(c.ChannelTypes, ct.ChannelType)
	}

	genres, err := q.ListRuleGenres(ctx, ruleID)
	if err != nil {
		return Conditions{}, err
	}
	for _, g := range genres {
		c.Genres = append(c.Genres, g.GenreLv1)
	}

	times, err := q.ListRuleTimes(ctx, ruleID)
	if err != nil {
		return Conditions{}, err
	}
	for _, tw := range times {
		c.Times = append(c.Times, TimeWindow{
			Weekdays: int(tw.Weekdays),
			StartSec: int(tw.StartSec),
			EndSec:   int(tw.EndSec),
		})
	}

	sites, err := q.ListRuleSites(ctx, ruleID)
	if err != nil {
		return Conditions{}, err
	}
	for _, s := range sites {
		c.Sites = append(c.Sites, s.Site)
	}

	return c, nil
}
