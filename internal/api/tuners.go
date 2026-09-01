package api

import (
	"context"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// ListTuners は tuner_sync の行をそのまま返す（GET /api/sites/{site}/tuners）。
//
// 導出はしない（不変条件 9・11。列の意味の権威は docs/data/capacity.md §6.5）。
// 不変条件 1 のとおり mirakc には問い合わせない --- worker が周期で投影した
// tuner_sync（使い捨てプロジェクション）を読むだけ。
//
// is_available / is_fault で絞らない（internal/db/queries/tuner_sync.sql の
// ListTunerSync のコメントと同じ理由）。「存在するが数えない」と「そもそも
// 射影が無い」の区別を UI 側にも渡す必要がある --- 射影が 1 行も無いサイトは
// 空配列を返し、それ自体が「何も主張しない」（docs/data/capacity.md §6.5）。
func (h *Server) ListTuners(ctx context.Context, req ListTunersRequestObject) (ListTunersResponseObject, error) {
	if !h.knownSite(req.Site) {
		return ListTuners404JSONResponse{Error: "unknown site"}, nil
	}
	rows, err := sqlcgen.New(h.pool).ListTunerSync(ctx, req.Site)
	if err != nil {
		return nil, err
	}

	result := make([]Tuner, 0, len(rows))
	for _, r := range rows {
		types := make([]TunerTypes, 0, len(r.Types))
		for _, t := range r.Types {
			types = append(types, TunerTypes(t))
		}
		result = append(result, Tuner{
			Index:       int(r.TunerIndex),
			Name:        r.Name,
			Types:       types,
			IsAvailable: r.IsAvailable,
			IsFault:     r.IsFault,
			ObservedAt:  r.ObservedAt,
		})
	}
	return ListTuners200JSONResponse(result), nil
}
