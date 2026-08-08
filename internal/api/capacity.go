package api

import (
	"context"
	"fmt"

	"github.com/fetburner/rokuban/internal/capacity"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ListCapacityOverages はチューナーが不足している区間を結合済みで返す
// (GET /api/capacity/overages)。
//
// **主張は下界に限る。** 返した区間が超過していることは確実だが、返らなかった区間が
// 「収まる」ことは保証しない（見えない消費者と mirakc の excluded_channels により、
// 既知の盲点はすべて「警告を見逃す」方向に偏っている。docs/data.md §6.5）。
// 返すのは区間の性質だけで、**どの番組・どの予約が負けるかは主張しない**。
//
// 不変条件 1 のとおり mirakc には問い合わせない。チューナーの本数と対応種別は
// worker が投影した tuner_sync（使い捨てプロジェクション）から読む。
//
// api は site に束縛されない（不変条件 1）ため、全サイトの超過区間を返す
// （issue #184 M4-12）。判定は internal/capacity.Compute が site ごとに独立に
// 行うので、全サイト分をまとめて読んでもサイト間の需要は混ざらない。
//
// 地平線全体を 1 回解いてから窓で切る（窓ごとに解かない。docs/data.md §6.5）。
// 予約集合は ruler の GC でローリングウィンドウに有界なので、8 日分の走査は
// 数十マイクロ秒のオーダーに収まる。キャッシュは入れていない --- 入れる場合は
// TTL のみにする必要がある（api は M2-19 以降 NOTIFY を LISTEN しない。docs/api.md）。
func (h *Server) ListCapacityOverages(ctx context.Context, req ListCapacityOveragesRequestObject) (ListCapacityOveragesResponseObject, error) {
	// windowError（ListPrograms）と違って窓幅の上限は課さない。判定は窓に関係なく
	// 地平線全体を 1 回解くので広い窓が高くつかず、返る区間は結合済みで少ない。
	if !req.Params.End.After(req.Params.Start) {
		return ListCapacityOverages400JSONResponse{Error: "end must be after start"}, nil
	}

	overages, err := capacity.LoadAllSites(ctx, sqlcgen.New(h.pool))
	if err != nil {
		return nil, fmt.Errorf("computing capacity overages: %w", err)
	}

	inWindow := capacity.Intersecting(overages, req.Params.Start, req.Params.End)
	result := make([]CapacityOverage, 0, len(inWindow))
	for _, o := range inWindow {
		jammed := make([]CapacityOverageJammedTypes, 0, len(o.JammedTypes))
		for _, t := range o.JammedTypes {
			jammed = append(jammed, CapacityOverageJammedTypes(t))
		}
		result = append(result, CapacityOverage{
			Site:        o.Site,
			StartAt:     o.StartAt,
			EndAt:       o.EndAt,
			Shortfall:   o.Shortfall,
			JammedTypes: jammed,
		})
	}
	return ListCapacityOverages200JSONResponse(result), nil
}
