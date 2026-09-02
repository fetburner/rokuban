package api

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ptr"
	"github.com/fetburner/rokuban/internal/rulequery"
)

// SearchPrograms はルール条件と同じコンパイラで EPG を検索する（M2-2）。
// ruler 評価（MatchProgramIDs）と同一経路。
func (h *Server) SearchPrograms(ctx context.Context, req SearchProgramsRequestObject) (SearchProgramsResponseObject, error) {
	if !h.knownSite(req.Site) {
		return SearchPrograms404JSONResponse{Error: "unknown site"}, nil
	}
	if req.Body == nil {
		return SearchPrograms400JSONResponse{Error: "request body is required"}, nil
	}

	c := conditionsFromSearch(*req.Body)

	// 検索 API でも ARE の不正パターンを 400 にする（ルール作成時と同規約）
	if msg := searchRegexError(ctx, h, c); msg != "" {
		return SearchPrograms400JSONResponse{Error: msg}, nil
	}

	ids, err := rulequery.MatchProgramIDs(ctx, h.pool, req.Site, c)
	if err != nil {
		return nil, err
	}
	// ponytail: site はパスからの複写であって、マッチした行から観測した値では
	// ない。sites に評価 site 以外を指定しても常にパスの site が返るため、
	// #530 が入るまでこの値を信じるクライアントは誤った値を受け取る（黙って、
	// スキーマ的には正当に見える形で）。#530 で sites 駆動の述語に置き換える。
	matches := make([]ProgramSearchMatch, len(ids))
	for i, id := range ids {
		matches[i] = ProgramSearchMatch{Site: req.Site, ProgramId: id}
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

// searchRegexError は不正な正規表現があればユーザー向けメッセージを返す（なければ空文字）。
// error ではなくメッセージを返すのは、これが 400 のレスポンス本文になるだけで
// 呼び出し側に伝播する失敗ではないため（epg.go の windowError と同じ規約）。
func searchRegexError(ctx context.Context, h *Server, c rulequery.Conditions) string {
	q := sqlcgen.New(h.pool)
	for _, m := range c.TextMatches {
		if m.Mode != "regex" || m.Value == "" {
			continue
		}
		if err := q.ValidateRegexPattern(ctx, m.Value); err != nil {
			return fmt.Sprintf("invalid regex %q (must be POSIX ARE compatible, not full PCRE/JavaScript regex — e.g. named capture groups like (?<name>...) are rejected; lookahead and lookbehind are supported): %v", m.Value, err)
		}
	}
	return ""
}
