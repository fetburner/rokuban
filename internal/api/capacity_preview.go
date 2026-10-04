package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/capacity"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/rulequery"
	"github.com/fetburner/rokuban/internal/ruler"
)

// PreviewCapacityOverages predicts only the overage intervals added by saving a search as
// a rule. It is a separate operation from SearchPrograms because it reads reservation,
// intent, recording, tuner projection, and dedupe state in addition to matching EPG rows.
func (h *Server) PreviewCapacityOverages(ctx context.Context, req PreviewCapacityOveragesRequestObject) (PreviewCapacityOveragesResponseObject, error) {
	if req.Body == nil {
		return PreviewCapacityOverages400JSONResponse{Error: "request body is required"}, nil
	}
	searchRequest := searchRequestFromCapacityPreview(*req.Body)
	conditions := conditionsFromSearch(searchRequest)

	if err := h.validateRuleSites(conditions.Sites, nil); err != nil {
		//nolint:nilerr // Validation errors become a client-facing 400 response.
		return PreviewCapacityOverages400JSONResponse{Error: err.Error()}, nil
	}
	if len(conditions.Sites) == 0 {
		conditions.Sites = h.siteNames
	}
	if message := searchRegexError(ctx, h, conditions); message != "" {
		return PreviewCapacityOverages400JSONResponse{Error: message}, nil
	}

	q := sqlcgen.New(h.pool)
	var editingRule *sqlcgen.Rule
	if req.Body.RuleId != nil {
		if *req.Body.RuleId <= 0 {
			return PreviewCapacityOverages400JSONResponse{Error: "ruleId must be greater than zero"}, nil
		}
		row, err := q.GetRule(ctx, *req.Body.RuleId)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return PreviewCapacityOverages404JSONResponse{Error: "rule not found"}, nil
			}
			return nil, fmt.Errorf("loading rule for capacity preview: %w", err)
		}
		editingRule = &row
	}

	matches, err := rulequery.MatchPrograms(ctx, h.pool, conditions)
	if err != nil {
		return nil, fmt.Errorf("matching programs for capacity preview: %w", err)
	}
	refs := make([]capacity.ProgramRef, len(matches))
	for i, match := range matches {
		refs[i] = capacity.ProgramRef{Site: match.Site, ProgramID: match.ProgramID}
	}
	candidates, err := capacity.PreviewCandidates(ctx, h.pool, refs, req.Body.RuleId)
	if err != nil {
		return nil, err
	}

	// A disabled rule does not create desired reservations. New rules default to enabled;
	// existing rules retain their saved enabled state while being edited.
	if editingRule != nil && !editingRule.Enabled {
		candidates = nil
	} else if editingRule != nil && editingRule.DedupeEnabled {
		candidates, err = removeDedupeSkips(ctx, h.pool, *req.Body.RuleId, candidates)
		if err != nil {
			return nil, fmt.Errorf("evaluating dedupe for capacity preview: %w", err)
		}
	}

	demands := make([]capacity.Demand, len(candidates))
	for i, candidate := range candidates {
		demands[i] = candidate.Demand
	}
	added, err := capacity.Preview(ctx, q, req.Body.RuleId, demands)
	if err != nil {
		return nil, fmt.Errorf("computing capacity preview: %w", err)
	}

	result := make([]CapacityOverage, 0, len(added))
	for _, overage := range added {
		jammed := make([]CapacityOverageJammedTypes, 0, len(overage.JammedTypes))
		for _, channelType := range overage.JammedTypes {
			jammed = append(jammed, CapacityOverageJammedTypes(channelType))
		}
		result = append(result, CapacityOverage{
			Site:        overage.Site,
			StartAt:     overage.StartAt,
			EndAt:       overage.EndAt,
			Shortfall:   overage.Shortfall,
			JammedTypes: jammed,
		})
	}
	return PreviewCapacityOverages200JSONResponse(result), nil
}

func searchRequestFromCapacityPreview(in CapacityPreviewRequest) ProgramSearchRequest {
	out := ProgramSearchRequest{
		DurationMaxMs: in.DurationMaxMs,
		DurationMinMs: in.DurationMinMs,
		Genres:        in.Genres,
		IsFree:        in.IsFree,
		PeriodEndAt:   in.PeriodEndAt,
		PeriodStartAt: in.PeriodStartAt,
		Services:      in.Services,
		Sites:         in.Sites,
		TextMatches:   in.TextMatches,
		Times:         in.Times,
	}
	if in.ChannelTypes != nil {
		channelTypes := make([]ProgramSearchRequestChannelTypes, len(*in.ChannelTypes))
		for i, channelType := range *in.ChannelTypes {
			channelTypes[i] = ProgramSearchRequestChannelTypes(channelType)
		}
		out.ChannelTypes = &channelTypes
	}
	return out
}

type previewProgramKey struct {
	site      string
	programID int64
}

func removeDedupeSkips(ctx context.Context, pool *pgxpool.Pool, ruleID int64, candidates []capacity.PreviewCandidate) ([]capacity.PreviewCandidate, error) {
	bySite := make(map[string][]ruler.DedupeCandidate)
	for _, candidate := range candidates {
		bySite[candidate.Site] = append(bySite[candidate.Site], ruler.DedupeCandidate{
			ProgramID: candidate.ProgramID,
			RuleID:    ruleID,
		})
	}
	skipped := make(map[previewProgramKey]struct{})
	for _, site := range sortedPreviewSites(bySite) {
		matches, err := ruler.EvaluateDedupe(ctx, pool, site, bySite[site])
		if err != nil {
			return nil, err
		}
		for programID := range matches {
			skipped[previewProgramKey{site: site, programID: programID}] = struct{}{}
		}
	}

	filtered := make([]capacity.PreviewCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := skipped[previewProgramKey{site: candidate.Site, programID: candidate.ProgramID}]; ok {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered, nil
}

func sortedPreviewSites(groups map[string][]ruler.DedupeCandidate) []string {
	sites := make([]string, 0, len(groups))
	for site := range groups {
		sites = append(sites, site)
	}
	slices.Sort(sites)
	return sites
}
