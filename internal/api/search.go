package api

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/ptr"
	"github.com/fetburner/rokuban/internal/rulequery"
)

// SearchPrograms はルール条件と同じコンパイラで EPG を検索する（M2-2）。
// ruler 評価（rulequery.Compile/MatchProgramIDsForRule）と WHERE 句のコンパイラを
// 共有するが、1 クエリで複数 site を横断できる rulequery.MatchPrograms を使う点が
// ruler（1 site ずつ評価する）と異なる（#530）。
func (h *Server) SearchPrograms(ctx context.Context, req SearchProgramsRequestObject) (SearchProgramsResponseObject, error) {
	if !h.knownSite(req.Site) {
		return SearchPrograms404JSONResponse{Error: "unknown site"}, nil
	}
	if req.Body == nil {
		return SearchPrograms400JSONResponse{Error: "request body is required"}, nil
	}

	c := conditionsFromSearch(*req.Body)

	// sites は rule_sites と同じ「空 = 全サイト、非空 = そのリストのみ」の軸規約
	// （docs/recording/ruler.md「サイトの扱い」）。validateRuleSites と同じ規律で
	// 未知の site 名を 400 にする（保存済み免除は無い一回限りの問い合わせなので
	// savedSites は nil）。省略時はレジストリ全件で埋めてから Compile に渡す
	// （Compile は空の Sites を許さない。#530）。
	siteErr := h.validateRuleSites(c.Sites, nil)
	if siteErr != nil {
		// nolint:nilerr // validateRuleSites の失敗は 400 レスポンスの本文として
		// 返す（rules.go の validateRuleInput 呼び出しと同じ規律。呼び出し側に
		// 伝播する失敗ではないので関数の error 戻り値は nil のままでよい）。
		return SearchPrograms400JSONResponse{Error: siteErr.Error()}, nil
	}
	if len(c.Sites) == 0 {
		c.Sites = h.siteNames
	}

	// 検索 API でも ARE の不正パターンを 400 にする（ルール作成時と同規約）
	if msg := searchRegexError(ctx, h, c); msg != "" {
		return SearchPrograms400JSONResponse{Error: msg}, nil
	}

	rows, err := rulequery.MatchPrograms(ctx, h.pool, c)
	if err != nil {
		return nil, err
	}
	// 行ごとに実際にマッチした site を返す（パスの {site} の複写ではない）。
	// sites がパスの site 以外を指しても、同一放送が複数 site でマッチしても
	// 畳まずそのまま行として運ぶ（#530 受け入れ: [{site, programId}] のフラット）。
	matches := make([]ProgramSearchMatch, len(rows))
	for i, row := range rows {
		matches[i] = ProgramSearchMatch{Site: row.Site, ProgramId: row.ProgramID}
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
