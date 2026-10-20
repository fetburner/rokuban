package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/fetburner/rokuban/internal/ptr"
	"github.com/fetburner/rokuban/internal/rulequery"
	"github.com/fetburner/rokuban/internal/store"
)

// SearchPrograms は ProgramSearch store を使ってルール条件と同じ条件で EPG を検索する。
// 複数 site を 1 回の検索で横断し、マッチした site ごとに結果を返す。
func (h *Server) SearchPrograms(ctx context.Context, req SearchProgramsRequestObject) (SearchProgramsResponseObject, error) {
	if req.Body == nil {
		return SearchPrograms400JSONResponse{Error: "request body is required"}, nil
	}

	c := conditionsFromSearch(*req.Body)

	// sites は rule_sites と同じ規約（空 = 全サイト。docs/recording/ruler.md「サイトの扱い」）。
	// validateRuleSites で未知の site 名を 400 にする（一回限りの問い合わせなので savedSites は nil）。
	siteErr := h.validateRuleSites(c.Sites, nil)
	if siteErr != nil {
		//nolint:nilerr // 400 の本文にするだけで呼び出し側に伝播する失敗ではない。
		// rules.go の CreateRule/UpdateRule が同じ変換で nilerr に引っかからないのは
		// defer tx.Rollback があり nilerr が歩く SSA のブロック形が変わるためで、
		// エラーハンドリングの規律の違いではない（defer を外すと同様に検出される）。
		return SearchPrograms400JSONResponse{Error: siteErr.Error()}, nil
	}
	if len(c.Sites) == 0 {
		c.Sites = h.siteNames // 条件検証で Sites が必須なので、登録済みの site を入れる
	}

	// 検索 API でも ARE の不正パターンを 400 にする（ルール作成時と同規約）
	msg, err := searchRegexError(ctx, h, c)
	if err != nil {
		return nil, err
	}
	if msg != "" {
		return SearchPrograms400JSONResponse{Error: msg}, nil
	}

	if h.programSearch == nil {
		return nil, fmt.Errorf("program search store is required")
	}
	rows, err := h.programSearch.SearchPrograms(ctx, c)
	if err != nil {
		return nil, err
	}
	// 行ごとに実際にマッチした site を返す。畳まない。
	matches := make([]ProgramSearchMatch, len(rows))
	for i, row := range rows {
		matches[i] = ProgramSearchMatch{
			Site:       row.Site,
			ProgramId:  row.ProgramID,
			NetworkId:  int(row.NetworkID),
			ServiceId:  int(row.ServiceID),
			StartAt:    row.StartAt,
			DurationMs: row.DurationMs,
			Name:       row.Name,
			IsFree:     row.IsFree,
		}
	}
	return SearchPrograms200JSONResponse(matches), nil
}

func conditionsFromSearch(in ProgramSearchRequest) rulequery.Conditions {
	c := rulequery.Conditions{
		IsFree:        in.IsFree,
		DurationMinMs: in.DurationMinMs,
		DurationMaxMs: in.DurationMaxMs,
		PeriodStartAt: in.PeriodStartAt,
		PeriodEndAt:   in.PeriodEndAt,
	}
	if in.TextMatches != nil {
		for _, m := range *in.TextMatches {
			c.TextMatches = append(c.TextMatches, rulequery.TextMatch{
				Target:        string(m.Target),
				Mode:          string(m.Mode),
				Value:         m.Value,
				CaseSensitive: ptr.Deref(m.CaseSensitive),
				Negate:        ptr.Deref(m.Negate),
			})
		}
	}
	if in.Services != nil {
		for _, s := range *in.Services {
			c.Services = append(c.Services, rulequery.ServiceRef{
				NetworkID: int32(s.NetworkId),
				ServiceID: int32(s.ServiceId),
			})
		}
	}
	if in.ChannelTypes != nil {
		for _, ct := range *in.ChannelTypes {
			c.ChannelTypes = append(c.ChannelTypes, string(ct))
		}
	}
	if in.Genres != nil {
		for _, g := range *in.Genres {
			c.Genres = append(c.Genres, int16(g))
		}
	}
	if in.Times != nil {
		for _, tw := range *in.Times {
			c.Times = append(c.Times, rulequery.TimeWindow{
				Weekdays: tw.Weekdays,
				StartSec: tw.StartSec,
				EndSec:   tw.EndSec,
			})
		}
	}
	if in.Sites != nil {
		c.Sites = append(c.Sites, (*in.Sites)...)
	}
	return c
}

// searchRegexError は不正な正規表現のメッセージまたは検証処理の失敗を返す。
func searchRegexError(ctx context.Context, h *Server, c rulequery.Conditions) (string, error) {
	if h.programSearch == nil {
		return "", fmt.Errorf("program search store is required")
	}
	for _, m := range c.TextMatches {
		if m.Mode != "regex" || m.Value == "" {
			continue
		}
		if err := h.programSearch.ValidateRegex(ctx, m.Value); err != nil {
			if errors.Is(err, store.ErrInvalidPattern) {
				return fmt.Sprintf("invalid regex %q (must be POSIX ARE compatible, not full PCRE/JavaScript regex — e.g. named capture groups like (?<name>...) are rejected; lookahead and lookbehind are supported): %v", m.Value, err), nil
			}
			return "", fmt.Errorf("validating search regex %q: %w", m.Value, err)
		}
	}
	return "", nil
}
