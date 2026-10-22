package pgstore

import (
	"context"
	"fmt"
	"sort"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

func (s *Rules) bumpRulerInputRevisions(ctx context.Context, q *sqlcgen.Queries, sites []string, all bool) error {
	if all {
		sites = s.siteNames
	}
	return bumpRulerInputRevisions(ctx, q, sites)
}

func bumpRulerInputRevisions(ctx context.Context, q *sqlcgen.Queries, sites []string) error {
	sites = append([]string(nil), sites...)
	sort.Strings(sites)
	previous := ""
	for _, site := range sites {
		if site == previous {
			continue
		}
		if err := bumpRulerInputRevision(ctx, q, site); err != nil {
			return err
		}
		previous = site
	}
	return nil
}

func bumpRulerInputRevision(ctx context.Context, q *sqlcgen.Queries, site string) error {
	if _, err := q.BumpRulerInputRevision(ctx, site); err != nil {
		return fmt.Errorf("bumping ruler input revision for site %q: %w", site, err)
	}
	return nil
}
