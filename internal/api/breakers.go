package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// knownCircuitBreakerNames はブレーカー識別子の既知集合。値の権威は
// internal/breaker の定数（DB に CHECK 制約は無い。internal/breaker/breaker.go
// のコメント参照）で、ここはそれを参照するだけ。未知の名前を渡された resume は
// タイポを黙って無視せず 400 にする。
var knownCircuitBreakerNames = map[string]bool{
	breaker.RulerDeletes:       true,
	breaker.ReconcileTotalLoss: true,
}

// ListCircuitBreakers は発動中のサーキットブレーカー一覧を返す
// (GET /api/breakers)。circuit_breakers は行の存在そのものが「発動中」を表すので、
// 停止していないブレーカーは結果に現れない（すべて停止していれば空配列が
// 正常系。docs/recording.md §3.2）。
//
// detail は発動時に「何が消されようとしていたか」を説明する抜粋
// （internal/breaker.Sample と同じ JSON 形）で、手動確認の材料になる。
func (h *Server) ListCircuitBreakers(ctx context.Context, _ ListCircuitBreakersRequestObject) (ListCircuitBreakersResponseObject, error) {
	q := sqlcgen.New(h.pool)
	rows, err := q.ListCircuitBreakers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing circuit breakers: %w", err)
	}

	result := make([]CircuitBreaker, 0, len(rows))
	for _, r := range rows {
		var sample CircuitBreakerSample
		if err := json.Unmarshal(r.Detail, &sample); err != nil {
			return nil, fmt.Errorf("unmarshalling detail for circuit breaker %s/%s: %w", r.Site, r.Name, err)
		}
		result = append(result, CircuitBreaker{
			Site:      r.Site,
			Name:      CircuitBreakerName(r.Name),
			TrippedAt: r.TrippedAt,
			Pending:   int(r.Pending),
			Threshold: int(r.Threshold),
			Detail:    sample,
		})
	}
	return ListCircuitBreakers200JSONResponse(result), nil
}

// ResumeCircuitBreaker は手動確認後の再開
// (POST /api/sites/{site}/breakers/{name}/resume)。
//
// ブレーカーは人間が確認するまで止まり続けるラッチで、解除は行の DELETE
// （internal/breaker/breaker.go のコメント参照）。DELETE ではなく POST の
// サブリソースにしているのは、運用者から見た操作が「行を削除する」ではなく
// 「確認したので再開する」であるため（openapi.yaml の description 参照）。
//
// ResumeCircuitBreaker は :execrows なので、削除された行数で「そもそも発動して
// いなかった」を区別できる。0 行なら 404 にする —
// 「再開したつもりが実は別のブレーカーだった／既に再開済みだった」を
// 黙って成功にしないため。
//
// site はパスに含める。circuit_breakers の PK は (site, name) であり、
// h.site 固定だと GET /api/breakers（サイト横断で一覧できる）で見えている
// 他サイトの発動を再開できなかった（issue #102）。h.site 以外の site が
// 来た場合は 400 —— 現状 1 プロセス 1 site の構成では、他 site の行は
// このプロセスからは操作できない。
func (h *Server) ResumeCircuitBreaker(ctx context.Context, req ResumeCircuitBreakerRequestObject) (ResumeCircuitBreakerResponseObject, error) {
	if req.Site != h.site {
		return ResumeCircuitBreaker400JSONResponse{Error: fmt.Sprintf("unknown site %q", req.Site)}, nil
	}
	if !knownCircuitBreakerNames[req.Name] {
		return ResumeCircuitBreaker400JSONResponse{Error: fmt.Sprintf("unknown circuit breaker %q", req.Name)}, nil
	}

	q := sqlcgen.New(h.pool)
	n, err := q.ResumeCircuitBreaker(ctx, sqlcgen.ResumeCircuitBreakerParams{
		Site: req.Site,
		Name: req.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("resuming circuit breaker %q: %w", req.Name, err)
	}
	if n == 0 {
		return ResumeCircuitBreaker404JSONResponse{Error: fmt.Sprintf("circuit breaker %q is not tripped", req.Name)}, nil
	}
	return ResumeCircuitBreaker204Response{}, nil
}
