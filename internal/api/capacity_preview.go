package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/fetburner/rokuban/internal/capacity"
	"github.com/fetburner/rokuban/internal/ruler"
	"github.com/fetburner/rokuban/internal/store"
)

// PreviewCapacityOverages は、検索条件をルールとして保存したときに新たに増える容量超過の
// 区間だけを予測する。EPG の一致に加えて予約・意図・録画・チューナー射影・dedupe の状態を
// 読むので、SearchPrograms とは別の操作にしてある。
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
	message, err := searchRegexError(ctx, h, conditions)
	if err != nil {
		return nil, err
	}
	if message != "" {
		return PreviewCapacityOverages400JSONResponse{Error: message}, nil
	}

	var editingRule *store.CapacityPreviewRule
	if req.Body.RuleId != nil {
		if *req.Body.RuleId <= 0 {
			return PreviewCapacityOverages400JSONResponse{Error: "ruleId must be greater than zero"}, nil
		}
		row, err := h.capacityStore.PreviewRule(ctx, *req.Body.RuleId)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return PreviewCapacityOverages404JSONResponse{Error: "rule not found"}, nil
			}
			return nil, fmt.Errorf("loading rule for capacity preview: %w", err)
		}
		editingRule = &row
	}

	if h.programSearch == nil {
		return nil, fmt.Errorf("program search store is required")
	}
	matches, err := h.programSearch.SearchPrograms(ctx, conditions)
	if err != nil {
		return nil, fmt.Errorf("matching programs for capacity preview: %w", err)
	}
	refs := make([]capacity.ProgramRef, len(matches))
	for i, match := range matches {
		refs[i] = capacity.ProgramRef{Site: match.Site, ProgramID: match.ProgramID}
	}
	candidates, err := capacity.PreviewCandidates(ctx, h.capacityStore, refs, req.Body.RuleId)
	if err != nil {
		return nil, err
	}

	// 無効なルールは予約を作らない。新規ルールは有効として数える（フォームの enabled は
	// 送られない）。編集中のルールは保存済みの enabled を使う。
	if editingRule != nil && !editingRule.Enabled {
		candidates = nil
	} else if editingRule != nil && editingRule.DedupeEnabled {
		candidates, err = h.removeDedupeSkips(ctx, *req.Body.RuleId, candidates)
		if err != nil {
			return nil, fmt.Errorf("evaluating dedupe for capacity preview: %w", err)
		}
	}

	demands := make([]capacity.Demand, len(candidates))
	for i, candidate := range candidates {
		demands[i] = candidate.Demand
	}
	added, err := capacity.Preview(ctx, h.capacityStore, req.Body.RuleId, demands)
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

func (h *Server) removeDedupeSkips(ctx context.Context, ruleID int64, candidates []capacity.PreviewCandidate) ([]capacity.PreviewCandidate, error) {
	bySite := make(map[string][]ruler.DedupeCandidate)
	for _, candidate := range candidates {
		bySite[candidate.Site] = append(bySite[candidate.Site], ruler.DedupeCandidate{
			ProgramID: candidate.ProgramID,
			RuleID:    ruleID,
		})
	}
	skipped := make(map[previewProgramKey]struct{})
	for _, site := range sortedPreviewSites(bySite) {
		matches, err := ruler.EvaluateDedupe(ctx, h.pool, site, bySite[site])
		if err != nil {
			return nil, err
		}
		for programID := range matches {
			skipped[previewProgramKey{site: site, programID: programID}] = struct{}{}
		}
	}

	filtered := make([]capacity.PreviewCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.IntentAction != nil && *candidate.IntentAction == "record" {
			filtered = append(filtered, candidate)
			continue
		}
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
