package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fetburner/rokuban/internal/breaker"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
)

// knownCircuitBreakerNames はブレーカー識別子の既知集合。値の権威は
// internal/breaker.All（DB に CHECK 制約は無い。internal/breaker/breaker.go
// のコメント参照）で、ここはそれを導出するだけ。手書きの複製を持つと片方だけ
// 更新されてずれる事故が起きる（issue #199: breaker.DeleteReconcile が
// 複製から漏れ、発動中なのに resume が 400 を返し続けた）。未知の名前を
// 渡された resume はタイポを黙って無視せず 400 にする。
var knownCircuitBreakerNames = func() map[string]bool {
	m := make(map[string]bool, len(breaker.All))
	for _, name := range breaker.All {
		m[name] = true
	}
	return m
}()

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
		// name の権威は internal/breaker.All だが、openapi.yaml の
		// CircuitBreakerName enum はそれを手で複製したものなので、breaker.All
		// に定数を足して openapi.yaml 側を直し忘れるとここでずれが顕在化しうる。
		//
		// CircuitBreakerName.Valid() による無効値の検査は行わない --- 消費者
		// である web/src/components/circuit-breaker-banner.tsx は isError を
		// 見ておらず（`unwrap(query.data) ?? []` → 0 件なら非表示）、500 は
		// 「未知の名前を見せる」よりまずい「発動中の他のブレーカーも含めて
		// 一覧そのものが消える」を引き起こす（1 行の enum 外の値がループ全体の
		// 500 に波及する）。web/src/pages/home.tsx の警告セクションも isError
		// を見ていないので同様に静かに消える。つまり「fail loud」のつもりが、
		// これら消費者では fail silent になる（PR #265 のレビューで指摘）。
		//
		// enum と breaker.All のずれの検知は internal/api/breakers_test.go の
		// TestBreakerAllNamesAreValidCircuitBreakerNameEnumMembers（DB も
		// HTTP も使わない純ユニットテスト）に閉じ、ハンドラは値をそのまま
		// 通す。クライアント側は元々このずれを見越したフォールバックを持つ
		// （web/src/lib/breaker.ts の describeBreakerName / describeBreakerReason
		// —— 未知の値は識別子そのもの表示 / 理由は空文字）。
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
// site 固定だと GET /api/breakers（サイト横断で一覧できる）で見えている
// 他サイトの発動を再開できなかった（issue #102）。api は不変条件 1 によりどの
// site にも束縛されないので、レジストリに無い site が来た場合だけ 400 にする
// （issue #184 M4-12。レジストリの全 site をこのプロセスから操作できる）。
func (h *Server) ResumeCircuitBreaker(ctx context.Context, req ResumeCircuitBreakerRequestObject) (ResumeCircuitBreakerResponseObject, error) {
	if !h.knownSite(req.Site) {
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
