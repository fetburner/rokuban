package capacity

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ProgramRef identifies one result row from rulequery.MatchPrograms. Site is part of the
// identity because a broadcast projected at two sites creates two independent reservations.
type ProgramRef struct {
	Site      string
	ProgramID int64
}

// PreviewCandidate keeps the matched program identity beside its capacity demand so the
// caller can run ruler's shared dedupe evaluator before projecting the demand.
type PreviewCandidate struct {
	ProgramID int64
	Demand
}

// PreviewCandidates returns search matches that would become additional reservations for
// a rule. Existing reservations are omitted, except reservations currently owned by the
// rule being edited. Skip intents and fulfilled programs are omitted as ruler does.
// Channel identity comes from epg_services, just like the rule compiler's service join.
func PreviewCandidates(ctx context.Context, pool *pgxpool.Pool, refs []ProgramRef, ruleID *int64) ([]PreviewCandidate, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	sites := make([]string, len(refs))
	programIDs := make([]int64, len(refs))
	for i, ref := range refs {
		sites[i] = ref.Site
		programIDs[i] = ref.ProgramID
	}

	const query = `
WITH wanted AS (
    SELECT * FROM unnest($1::text[], $2::bigint[]) AS w(site, program_id)
)
SELECT p.site,
       p.program_id,
       s.channel_type,
       s.channel,
       p.start_at,
       (p.start_at + (p.duration_ms * interval '1 millisecond'))::timestamptz AS end_at
FROM wanted w
JOIN epg_programs p
  ON p.site = w.site AND p.program_id = w.program_id
JOIN epg_services s
  ON s.site = p.site AND s.network_id = p.network_id AND s.service_id = p.service_id
WHERE NOT EXISTS (
          SELECT 1
          FROM reservations r
          WHERE r.site = p.site AND r.program_id = p.program_id
            AND ($3::bigint IS NULL OR r.rule_id IS DISTINCT FROM $3::bigint)
      )
  AND NOT EXISTS (
          SELECT 1 FROM program_intents i
          WHERE i.site = p.site AND i.program_id = p.program_id AND i.action = 'skip'
      )
  AND NOT EXISTS (
          SELECT 1
          FROM recordings rec
          JOIN media_assets a ON a.recording_id = rec.id AND a.kind = 'original'
          WHERE rec.site = p.site
            AND rec.network_id = p.network_id
            AND rec.service_id = p.service_id
            AND rec.event_id = p.event_id
      )
ORDER BY p.site, p.start_at, p.program_id`

	var excludedRule any
	if ruleID != nil {
		excludedRule = *ruleID
	}
	rows, err := pool.Query(ctx, query, sites, programIDs, excludedRule)
	if err != nil {
		return nil, fmt.Errorf("listing capacity preview candidates: %w", err)
	}
	defer rows.Close()

	var candidates []PreviewCandidate
	for rows.Next() {
		var candidate PreviewCandidate
		if err := rows.Scan(&candidate.Site, &candidate.ProgramID, &candidate.ChannelType, &candidate.Channel, &candidate.StartAt, &candidate.EndAt); err != nil {
			return nil, fmt.Errorf("scanning capacity preview candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating capacity preview candidates: %w", err)
	}
	return candidates, nil
}

// Preview loads the same current demand and tuner projection used by LoadAllSites, then
// computes the intervals that would be newly over capacity after adding eligible candidates.
// For an edited rule, its current reservations are removed from the hypothetical demand first.
func Preview(ctx context.Context, q *sqlcgen.Queries, ruleID *int64, candidates []Demand) ([]Overage, error) {
	rows, err := q.ListCapacityDemandAllSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing capacity preview baseline: %w", err)
	}
	current, withoutRule, err := demandsFromAllSiteRows(rows, ruleID)
	if err != nil {
		return nil, fmt.Errorf("resolving capacity preview baseline: %w", err)
	}

	tunerRows, err := q.ListTunerSyncAllSites(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing tuner projection for capacity preview: %w", err)
	}
	tuners := tunersFromRows(tunerRows)
	currentOverages := Compute(current, tuners)

	after := make([]Demand, 0, len(withoutRule)+len(candidates))
	after = append(after, withoutRule...)
	after = append(after, candidates...)
	newOverages := Compute(after, tuners)
	return NewlyAdded(currentOverages, newOverages), nil
}

// NewlyAdded subtracts baseline intervals that cover an after interval with the same or a
// greater shortfall. A worsening shortfall remains new even when it overlaps an older
// overage. JammedTypes do not define coverage: they describe the chosen proof for that
// particular interval, while the severity comparison is the shortfall count.
func NewlyAdded(before, after []Overage) []Overage {
	ordered := append([]Overage(nil), after...)
	slices.SortFunc(ordered, func(a, b Overage) int {
		if a.Site != b.Site {
			if a.Site < b.Site {
				return -1
			}
			return 1
		}
		return a.StartAt.Compare(b.StartAt)
	})

	var out []Overage
	for _, post := range ordered {
		var covering []Overage
		for _, pre := range before {
			if pre.Site != post.Site || pre.Shortfall < post.Shortfall ||
				!pre.StartAt.Before(post.EndAt) || !pre.EndAt.After(post.StartAt) {
				continue
			}
			covering = append(covering, pre)
		}
		slices.SortFunc(covering, func(a, b Overage) int { return a.StartAt.Compare(b.StartAt) })

		cursor := post.StartAt
		for _, cover := range covering {
			coverStart := maxTime(cover.StartAt, post.StartAt)
			coverEnd := minTime(cover.EndAt, post.EndAt)
			if cursor.Before(coverStart) {
				out = appendOverageSegment(out, post, cursor, coverStart)
			}
			if coverEnd.After(cursor) {
				cursor = coverEnd
			}
			if !cursor.Before(post.EndAt) {
				break
			}
		}
		if cursor.Before(post.EndAt) {
			out = appendOverageSegment(out, post, cursor, post.EndAt)
		}
	}
	return out
}

func appendOverageSegment(out []Overage, source Overage, start, end time.Time) []Overage {
	if !end.After(start) {
		return out
	}
	segment := source
	segment.StartAt = start
	segment.EndAt = end
	segment.JammedTypes = slices.Clone(source.JammedTypes)
	if len(out) > 0 {
		last := &out[len(out)-1]
		if last.Site == segment.Site && last.EndAt.Equal(segment.StartAt) &&
			last.Shortfall == segment.Shortfall && slices.Equal(last.JammedTypes, segment.JammedTypes) {
			last.EndAt = segment.EndAt
			return out
		}
	}
	return append(out, segment)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
